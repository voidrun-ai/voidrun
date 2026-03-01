package server

import (
	"voidrun/internal/config"
	"voidrun/internal/handler"
	"voidrun/internal/metrics"
	"voidrun/internal/model"
	"voidrun/internal/repository"
	"voidrun/internal/service"
	"voidrun/pkg/util"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Repositories holds all data stores
type Repositories struct {
	User    repository.IUserRepository
	Sandbox repository.ISandboxRepository
	Image   repository.IImageRepository
	APIKey  repository.IAPIKeyRepository
	Org     repository.IOrgRepository
}

func InitRepositories(cfg *config.Config, db *mongo.Database) *Repositories {
	return &Repositories{
		User:    repository.NewUserRepository(cfg, db),
		Sandbox: repository.NewSandboxRepository(cfg, db),
		Image:   repository.NewImageRepository(cfg, db),
		APIKey:  repository.NewAPIKeyRepository(cfg, db),
		Org:     repository.NewOrgRepository(cfg, db),
	}
}

// Services holds all business logic layers
type Services struct {
	User       *service.UserService
	Sandbox    *service.SandboxService
	Image      *service.ImageService
	Exec       *service.ExecService
	Session    *service.SessionExecService
	FS         *service.FSService
	APIKey     *service.APIKeyService
	Org        *service.OrgService
	PTY        *service.VsockWSDialer
	PTYSession *service.PTYSessionService
	Commands   *service.CommandsService
	Metrics    *metrics.Manager
}

func InitServices(cfg *config.Config, repos *Repositories, metricsManager *metrics.Manager) *Services {
	return &Services{
		User:       service.NewUserService(cfg, repos.User),
		Sandbox:    service.NewSandboxService(cfg, repos.Sandbox, repos.Image, metricsManager),
		Image:      service.NewImageService(cfg, repos.Image),
		Exec:       service.NewExecService(cfg),
		Session:    service.NewSessionExecService(cfg),
		FS:         service.NewFSService(),
		APIKey:     service.NewAPIKeyService(repos.APIKey, cfg),
		Org:        service.NewOrgService(repos.Org),
		PTY:        service.NewVsockWSDialer(),
		PTYSession: service.NewPTYSessionService(),
		Commands:   service.NewCommandsService(cfg),
		Metrics:    metricsManager,
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
	Auth     *handler.AuthHandler
	PTY      *handler.PTYHandler
	Commands *handler.CommandsHandler
	Version  *handler.VersionHandler
}

func InitHandlers(services *Services) *Handlers {
	return &Handlers{
		User:     handler.NewUserHandler(services.User),
		Sandbox:  handler.NewSandboxHandler(services.Sandbox),
		Image:    handler.NewImageHandler(services.Image),
		Exec:     handler.NewExecHandler(services.Exec, services.Session, services.Sandbox, services.Commands),
		FS:       handler.NewFSHandler(services.FS, services.Sandbox),
		Org:      handler.NewOrgHandler(services.Org, services.APIKey, services.User),
		Auth:     handler.NewAuthHandler(services.User, services.Org, services.APIKey),
		PTY:      handler.NewPTYHandler(services.PTY, services.PTYSession, services.Sandbox),
		Commands: handler.NewCommandsHandler(services.Commands, services.Sandbox),
		Version:  handler.NewVersionHandler(),
	}
}

// PopulateInitialData seeds system users/images
func PopulateInitialData(cfg *config.Config, repos *Repositories) error {
	userRepo := repos.User
	if err := userRepo.EnsureSystemUser(model.User{
		Name:  cfg.SystemUser.Name,
		Email: cfg.SystemUser.Email,
	}); err != nil {
		return err
	}

	// Create default system images (using concrete repo)
	if imgRepo, ok := repos.Image.(interface{ EnsureSystemImage(model.Image) error }); ok {
		sysUserID, _ := util.ParseObjectID(cfg.SystemUser.ID)
		if err := imgRepo.EnsureSystemImage(model.Image{
			ID:        primitive.NewObjectID(),
			Name:      "alpine",
			Tag:       "latest",
			CreatedBy: sysUserID,
		}); err != nil {
			return err
		}
		if err := imgRepo.EnsureSystemImage(model.Image{
			ID:        primitive.NewObjectID(),
			Name:      "debian",
			Tag:       "latest",
			CreatedBy: sysUserID,
		}); err != nil {
			return err
		}
	}

	return nil
}
