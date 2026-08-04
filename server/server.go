package server

import (
	"context"
	"fmt"
	"sort"
	"time"

	"voidrun/config"
	"voidrun/handler"
	"voidrun/metrics"
	"voidrun/runtime"
	"voidrun/util"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Server represents the HTTP server
type Server struct {
	cfg         *config.Config
	router      *gin.Engine
	mongo       *mongo.Client
	services    *Services
	metrics     *metrics.Manager
	handlers    *Handlers
	stopFn      context.CancelFunc
	db          *mongo.Database
	repos       *Repositories
	middlewares *Middlewares
}

// New creates a new server instance.
func New(cfg *config.Config, routeMws RouteMiddlewares) (*Server, error) {
	runtime.SetInstancesRoot(cfg.Paths.InstancesDir)
	runtime.SetCHBinary(cfg.CHBinary)
	runtime.SetDecoupledSnapshot(cfg.Sandbox.DecoupledSnapshot, cfg.Sandbox.MemoryBackingMode)
	var metricsManager *metrics.Manager
	var stopFn context.CancelFunc
	if cfg.Metrics.Enabled {
		metricsManager = metrics.NewManager(cfg.Metrics)
		ctx, cancel := context.WithCancel(context.Background())
		metricsManager.Start(ctx)
		stopFn = cancel
	}

	mongoClient, err := Connect(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	db := mongoClient.Database(cfg.Mongo.Database)

	repos := InitRepositories(cfg, db)
	services := InitServices(cfg, repos, metricsManager)
	handlers := InitHandlers(services)

	if err := PopulateInitialData(cfg, repos); err != nil {
		return nil, fmt.Errorf("failed to populate initial data: %w", err)
	}

	middlewares := InitMiddlewares(cfg, services)

	router, err := setupRouter(cfg, handlers, services, middlewares, routeMws)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:         cfg,
		router:      router,
		mongo:       mongoClient,
		services:    services,
		metrics:     metricsManager,
		stopFn:      stopFn,
		handlers:    handlers,
		db:          db,
		repos:       repos,
		middlewares: middlewares,
	}, nil
}

func Connect(cfg *config.Config) (*mongo.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.Mongo.URI))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping MongoDB: %w", err)
	}
	return client, nil
}

// Close disconnects MongoDB client and Redis client
func (s *Server) Close() error {
	if s.stopFn != nil {
		s.stopFn()
	}
	if s.services != nil && s.services.AuthCache != nil {
		if err := s.services.AuthCache.Close(); err != nil {
			fmt.Printf("[server] Failed to close Redis connection: %v\n", err)
		}
	}
	if s.mongo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.mongo.Disconnect(ctx)
	}
	return nil
}

// Run starts the server
func (s *Server) Run() error {
	s.startHealthMonitor()
	s.resumeEventWatchers()
	s.startLifecycleManager()

	if s.cfg.Auth.LocalMode {
		fmt.Println("[WARN] LOCAL_MODE enabled: authentication is bypassed")
	}

	ver := util.Get()
	versionLine := fmt.Sprintf("%s", ver.Version)
	if ver.Commit != "" {
		versionLine = fmt.Sprintf("%s (%s)", ver.Version, ver.Commit)
	}
	if ver.BuildTime != "" {
		versionLine = fmt.Sprintf("%s built %s", versionLine, ver.BuildTime)
	}
	fmt.Printf("🚀 VoidRun Server %s running on %s\n", versionLine, s.cfg.Server.Address())
	return s.router.Run(s.cfg.Server.Address())
}

func (s *Server) startLifecycleManager() {
	if s.services.LifecycleManager != nil {
		s.services.LifecycleManager.Start(context.Background())
	}
}

func (s *Server) startHealthMonitor() {
	if !s.cfg.Health.Enabled {
		return
	}
	intervalSec := s.cfg.Health.IntervalSec
	if intervalSec <= 0 {
		intervalSec = 30
	}
	interval := time.Duration(intervalSec) * time.Second
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			if err := s.services.Sandbox.RefreshStatuses(context.Background()); err != nil {
				fmt.Printf("[health] refresh failed: %v\n", err)
			}
		}
	}()
}

func (s *Server) resumeEventWatchers() {
	if s.services.Monitor == nil {
		return
	}

	ctx := context.Background()
	// Find all non-killed sandboxes to resume watching
	sandboxes, err := s.repos.Sandbox.FindForHealth(ctx, s.cfg.HostID, options.FindOptions{})
	if err != nil {
		fmt.Printf("[event_monitor] failed to fetch sandboxes for resume: %v\n", err)
		return
	}

	var meta []runtime.SandboxMeta
	for _, sb := range sandboxes {
		meta = append(meta, runtime.SandboxMeta{
			SandboxID: sb.ID,
			OrgID:     sb.OrgID,
			UserID:    sb.CreatedBy,
		})
		if s.metrics == nil {
			continue
		}
		id := sb.ID.Hex()
		if sb.Status == "running" {
			s.metrics.RegisterSandbox(id, sb.Name, runtime.GetSocketPath(id), sb.CPU, sb.Mem, sb.DiskMB)
		} else {
			s.metrics.SetSandboxStatus(id, sb.Name, sb.Status)
		}
	}

	s.services.Monitor.ResumeAll(ctx, meta)
}

func setupRouter(cfg *config.Config, h *Handlers, s *Services, mw *Middlewares, routeMws RouteMiddlewares) (*gin.Engine, error) {
	r := gin.Default()
	r.SetTrustedProxies(nil)

	if cfg.CORS.Enabled {
		corsCfg := cors.Config{
			AllowOrigins:     cfg.CORS.AllowOrigins,
			AllowMethods:     cfg.CORS.AllowMethods,
			AllowHeaders:     cfg.CORS.AllowHeaders,
			ExposeHeaders:    cfg.CORS.ExposeHeaders,
			AllowCredentials: cfg.CORS.AllowCredentials,
			MaxAge:           time.Duration(cfg.CORS.MaxAgeSec) * time.Second,
		}
		if cfg.CORS.AllowCredentials && len(cfg.CORS.AllowOrigins) == 1 && cfg.CORS.AllowOrigins[0] == "*" {
			corsCfg.AllowOrigins = nil
			corsCfg.AllowOriginFunc = func(string) bool { return true }
		}
		r.Use(cors.New(corsCfg))
	}
	if cfg.Metrics.Enabled && s.Metrics != nil {
		r.Use(s.Metrics.Middleware())
		r.GET(cfg.Metrics.Path, gin.WrapH(s.Metrics.Handler()))
	}

	if cfg.Auth.LocalMode {
		// Static assets (CSS, JS, etc.)
		r.Static("/ui/assets", "./static")

		// Serve index.html as a static file
		r.GET("/ui", func(c *gin.Context) {
			c.File("./static/index.html")
		})
	}

	api := r.Group("/api")

	// Public metadata routes
	api.GET("/version", handler.Handle(h.Version.Get))

	// Protected routes require API key or JWT auth
	protected := api.Group("")
	protected.Use(mw.Auth)
	for _, global := range routeMws[RouteAll] {
		protected.Use(global)
	}

	registered := make(map[RouteID]struct{})
	track := func(id RouteID) {
		if _, dup := registered[id]; dup {
			panic(fmt.Sprintf("server: route ID %q registered more than once", id))
		}
		registered[id] = struct{}{}
	}

	mount := func(g *gin.RouterGroup, method, path string, fn handler.HandlerFunc) {
		id := RouteIDFor(method, g.BasePath()+path)
		track(id)
		g.Handle(method, path, routeMws.Wrap(id, handler.Handle(fn))...)
	}
	mountAny := func(g *gin.RouterGroup, path string, fn handler.HandlerFunc) {
		id := RouteIDFor(AnyMethod, g.BasePath()+path)
		track(id)
		g.Any(path, routeMws.Wrap(id, handler.Handle(fn))...)
	}

	// Sandbox routes
	sandboxes := protected.Group("/sandboxes")
	{
		mount(sandboxes, "GET", "", h.Sandbox.List)
		mount(sandboxes, "POST", "", h.Sandbox.Create)

		sandboxByID := sandboxes.Group("/:id")
		mount(sandboxByID, "GET", "", h.Sandbox.Get)
		mount(sandboxByID, "DELETE", "", h.Sandbox.Delete)
		mount(sandboxByID, "POST", "/sleep", h.Sandbox.Snapshot)
		mount(sandboxByID, "POST", "/wake", h.Sandbox.Restore)
		mount(sandboxByID, "POST", "/start", h.Sandbox.Start)
		mount(sandboxByID, "PATCH", "/publish-ports", h.Sandbox.UpdatePublishPorts)

		mount(sandboxByID, "POST", "/exec", h.Exec.Exec)
		mount(sandboxByID, "POST", "/exec-stream", h.Exec.ExecStream)
		mount(sandboxByID, "POST", "/session-exec", h.Exec.SessionExec)
		mount(sandboxByID, "POST", "/session-exec-stream", h.Exec.SessionExecStream)

		// Commands (Process Management)
		mount(sandboxByID, "POST", "/commands/run", h.Commands.Run)
		mount(sandboxByID, "GET", "/commands/list", h.Commands.List)
		mount(sandboxByID, "POST", "/commands/kill", h.Commands.Kill)
		mount(sandboxByID, "POST", "/commands/attach", h.Commands.Attach)
		mount(sandboxByID, "POST", "/commands/wait", h.Commands.Wait)

		// PTY Session Management
		mount(sandboxByID, "GET", "/pty", h.PTY.Proxy)
		mount(sandboxByID, "POST", "/pty/sessions", h.PTY.CreateSession)
		mount(sandboxByID, "GET", "/pty/sessions", h.PTY.ListSessions)
		mount(sandboxByID, "GET", "/pty/sessions/:sessionId", h.PTY.ConnectSession)
		mount(sandboxByID, "DELETE", "/pty/sessions/:sessionId", h.PTY.DeleteSession)
		mount(sandboxByID, "POST", "/pty/sessions/:sessionId/execute", h.PTY.ExecuteCommand)
		mount(sandboxByID, "GET", "/pty/sessions/:sessionId/buffer", h.PTY.GetBuffer)
		mount(sandboxByID, "POST", "/pty/sessions/:sessionId/resize", h.PTY.ResizeTerminal)

		// File operations
		mount(sandboxByID, "GET", "/files", h.FS.ListFiles)
		mount(sandboxByID, "GET", "/files/download", h.FS.DownloadFile)
		mount(sandboxByID, "POST", "/files/upload", h.FS.UploadFile)
		mount(sandboxByID, "POST", "/files/mkdir", h.FS.CreateDirectory)
		mount(sandboxByID, "POST", "/files/create", h.FS.CreateFile)
		mount(sandboxByID, "POST", "/files/copy", h.FS.CopyFile)
		mount(sandboxByID, "GET", "/files/head-tail", h.FS.HeadTail)
		mount(sandboxByID, "POST", "/files/chmod", h.FS.ChangePermissions)
		mount(sandboxByID, "GET", "/files/du", h.FS.DiskUsage)
		mount(sandboxByID, "GET", "/files/search", h.FS.SearchFiles)
		mount(sandboxByID, "POST", "/files/compress", h.FS.CompressFile)
		mount(sandboxByID, "POST", "/files/extract", h.FS.ExtractArchive)
		mount(sandboxByID, "DELETE", "/files", h.FS.DeleteFile)
		mount(sandboxByID, "POST", "/files/move", h.FS.MoveFile)
		mount(sandboxByID, "GET", "/files/stat", h.FS.StatFile)

		mount(sandboxByID, "POST", "/files/watch", h.FS.StartWatch)
		mount(sandboxByID, "GET", "/files/watch/:sessionId/stream", h.FS.StreamWatchEvents)
	}

	// Image routes
	images := protected.Group("/images")
	{
		mount(images, "GET", "", h.Image.List)
		mount(images, "GET", "/:id", h.Image.Get)
		mount(images, "GET", "/name/:name", h.Image.GetByName)
	}

	// Org routes
	org := protected.Group("/orgs")
	{
		mount(org, "GET", "/users", h.Org.GetOrgUsers)

		apiKeys := org.Group("/apikeys")
		mount(apiKeys, "GET", "", h.Org.ListAPIKeys)
		mount(apiKeys, "POST", "", h.Org.GenerateAPIKey)
		mount(apiKeys, "DELETE", "/:keyId", h.Org.DeleteAPIKey)
		mount(apiKeys, "POST", "/:keyId/activate", h.Org.ActivateAPIKey)
		mount(apiKeys, "PATCH", "/:keyId/touch", h.Org.TouchAPIKey)
	}

	// User routes
	user := protected.Group("/users")
	{
		mount(user, "GET", "/me", h.User.GetMe)
	}

	// MCP (Model Context Protocol) endpoint — single route handles all methods.
	mountAny(protected, "/mcp", h.MCP.Handle)

	var unknown []string
	for id := range routeMws {
		if id == RouteAll {
			continue
		}
		if _, ok := registered[id]; !ok {
			unknown = append(unknown, string(id))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("route middlewares: unknown route ID(s): %v", unknown)
	}

	return r, nil
}

func (s *Server) Router() *gin.Engine {
	return s.router
}

func (s *Server) MongoClient() *mongo.Client {
	return s.mongo
}

func (s *Server) Services() *Services {
	return s.services
}

func (s *Server) Handlers() *Handlers {
	return s.handlers
}

func (s *Server) Repositories() *Repositories {
	return s.repos
}

func (s *Server) Middlewares() *Middlewares {
	return s.middlewares
}

func (s *Server) DB() *mongo.Database {
	return s.db
}

func (s *Server) Config() *config.Config {
	return s.cfg
}
