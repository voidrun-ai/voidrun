package server

import (
	"context"
	"fmt"

	"voidrun/config"
	"voidrun/handler"
	"voidrun/metrics"
	"voidrun/middleware"
	"voidrun/model"
	"voidrun/repository"
	"voidrun/runtime"
	"voidrun/service"
	"voidrun/util"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo"
)

// Repositories holds all data stores
type Repositories struct {
	User    repository.IUserRepository
	Sandbox repository.ISandboxRepository
	Image   repository.IImageRepository
	APIKey  repository.IAPIKeyRepository
	Org     repository.IOrgRepository
	Event   repository.IEventRepository
}

func InitRepositories(cfg *config.Config, db *mongo.Database) *Repositories {
	sbRepo := repository.NewSandboxRepository(cfg, db)
	if err := sbRepo.Init(context.Background()); err != nil {
		panic(err)
	}

	return &Repositories{
		User:    repository.NewUserRepository(cfg, db),
		Sandbox: sbRepo,
		Image:   repository.NewImageRepository(cfg, db),
		APIKey:  repository.NewAPIKeyRepository(cfg, db),
		Org:     repository.NewOrgRepository(cfg, db),
		Event:   repository.NewEventRepository(db),
	}
}

// Services holds all business logic layers
type Services struct {
	User             *service.UserService
	Sandbox          *service.SandboxService
	Image            *service.ImageService
	Exec             *service.ExecService
	Session          *service.SessionExecService
	FS               *service.FSService
	APIKey           *service.APIKeyService
	Org              *service.OrgService
	Dialer           *service.VsockWSDialer
	PTYSession       *service.PTYSessionService
	Commands         *service.CommandsService
	Metrics          *metrics.Manager
	Clerk            *service.ClerkService
	AuthCache        *service.AuthCache
	Monitor          *runtime.EventMonitor
	LifecycleManager *service.LifecycleManager
}

func InitServices(cfg *config.Config, repos *Repositories, metricsManager *metrics.Manager) *Services {
	clerkSvc := service.NewClerkService(cfg)
	orgSvc := service.NewOrgService(repos.Org)

	// Initialize AuthCache (Redis-backed)
	var authCache *service.AuthCache
	authCache, err := service.NewAuthCache(cfg)
	if err != nil {
		fmt.Printf("[WARN] Failed to initialize Redis auth cache: %v. Auth will fall back to database lookups.\n", err)
		// authCache remains nil, which triggers graceful degradation
	}

	// Initialize Event Monitor if enabled
	var monitor *runtime.EventMonitor
	if cfg.Monitor.Enabled {
		monitor = runtime.NewEventMonitor(repos.Event)
		// Set root context for monitor (used for watcher goroutines)
		monitor.SetRootContext(context.Background())
	}

	// Shared per-sandbox lifecycle locks. Both SandboxService and LifecycleManager
	// receive the same instance so manual API ops and the background sweeper
	// serialize on the same sandbox ID.
	lifecycleLocks := service.NewSandboxLifecycleLocks()

	// Build the sandbox service eagerly so the lifecycle manager can reuse its
	// Snapshot implementation directly. This keeps the manual /snapshot API
	// and the auto-snapshot sweep on a single shared code path.
	sandboxSvc := service.NewSandboxService(cfg, repos.Sandbox, repos.Image, metricsManager, monitor, lifecycleLocks)

	return &Services{
		User:             service.NewUserService(cfg, repos.User, clerkSvc, orgSvc),
		Sandbox:          sandboxSvc,
		Image:            service.NewImageService(cfg, repos.Image),
		Exec:             service.NewExecService(cfg),
		Session:          service.NewSessionExecService(cfg),
		FS:               service.NewFSService(),
		APIKey:           service.NewAPIKeyService(repos.APIKey, cfg, authCache),
		Org:              orgSvc,
		Dialer:           service.NewVsockWSDialer(),
		PTYSession:       service.NewPTYSessionService(),
		Commands:         service.NewCommandsService(cfg),
		Metrics:          metricsManager,
		Clerk:            clerkSvc,
		AuthCache:        authCache,
		Monitor:          monitor,
		LifecycleManager: service.NewLifecycleManager(cfg.AutoLifecycle, repos.Sandbox, monitor, metricsManager, lifecycleLocks, sandboxSvc),
	}
}

// Handlers holds all HTTP handlers
type Handlers struct {
	User     *handler.UserHandler
	Sandbox  *handler.SandboxHandler
	Image    *handler.ImageHandler
	Exec     *handler.ExecHandler
	FS       *handler.FSHandler
	Org      *handler.OrgHandler
	PTY      *handler.PTYHandler
	Commands *handler.CommandsHandler
	Version  *handler.VersionHandler
	MCP      *handler.MCPHandler
}

func InitHandlers(services *Services) *Handlers {
	return &Handlers{
		User:     handler.NewUserHandler(services.User, services.Org),
		Sandbox:  handler.NewSandboxHandler(services.Sandbox),
		Image:    handler.NewImageHandler(services.Image),
		Exec:     handler.NewExecHandler(services.Exec, services.Session, services.Sandbox),
		FS:       handler.NewFSHandler(services.FS, services.Sandbox, services.Dialer),
		Org:      handler.NewOrgHandler(services.Org, services.APIKey, services.User),
		PTY:      handler.NewPTYHandler(services.Dialer, services.PTYSession, services.Sandbox),
		Commands: handler.NewCommandsHandler(services.Commands, services.Sandbox),
		Version:  handler.NewVersionHandler(),
		MCP:      handler.NewMCPHandler(services.Sandbox, services.Exec, services.FS, services.Commands, services.Image),
	}
}

// Middlewares stores reusable middleware handlers.
type Middlewares struct {
	Auth gin.HandlerFunc
}

// InitMiddlewares builds middleware handler references for reuse.
func InitMiddlewares(cfg *config.Config, s *Services) *Middlewares {
	if cfg.Auth.LocalMode {
		return &Middlewares{
			Auth: middleware.LocalModeMiddleware(cfg.SystemUser.OrgID.Hex(), cfg.SystemUser.ID.Hex()),
		}
	}

	return &Middlewares{
		Auth: middleware.AuthMiddleware(cfg, s.APIKey, s.User, s.Clerk, s.AuthCache),
	}
}

// PopulateInitialData seeds system users/images
func PopulateInitialData(cfg *config.Config, repos *Repositories) error {
	userRepo := repos.User
	seedSystemUserID := util.GenerateObjectID()
	if err := userRepo.EnsureSystemUser(model.User{
		ID:    seedSystemUserID,
		Name:  cfg.SystemUser.Name,
		Email: cfg.SystemUser.Email,
	}); err != nil {
		return err
	}

	systemUser, err := userRepo.FindByEmail(context.Background(), cfg.SystemUser.Email)
	if err != nil {
		return err
	}
	if systemUser == nil {
		return fmt.Errorf("system user not found after initialization")
	}
	systemUserID := systemUser.ID
	cfg.SystemUser.ID = systemUserID

	if cfg.Auth.LocalMode {
		orgSvc := service.NewOrgService(repos.Org)
		localOrg, err := orgSvc.EnsureDefaultOrg(context.Background(), systemUserID, "Local")
		if err != nil {
			return fmt.Errorf("ensure local org: %w", err)
		}
		cfg.SystemUser.OrgID = localOrg.ID
	}

	return nil
}
