package server

import (
	"context"
	"fmt"
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

// New creates a new server instance
func New(cfg *config.Config, extraProtectedMiddlewares ...gin.HandlerFunc) (*Server, error) {
	// Initialize machine package with config paths
	runtime.SetInstancesRoot(cfg.Paths.InstancesDir)
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

	router := setupRouter(cfg, handlers, services, middlewares, extraProtectedMiddlewares...)

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
	sandboxes, err := s.repos.Sandbox.FindForHealth(ctx, options.FindOptions{})
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
	}

	s.services.Monitor.ResumeAll(ctx, meta)
}

func setupRouter(cfg *config.Config, h *Handlers, s *Services, mw *Middlewares, extraProtectedMiddlewares ...gin.HandlerFunc) *gin.Engine {
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
	for _, extra := range extraProtectedMiddlewares {
		protected.Use(extra)
	}

	// Sandbox routes
	sandboxes := protected.Group("/sandboxes")
	{
		sandboxes.GET("", handler.Handle(h.Sandbox.List))
		sandboxes.POST("", handler.Handle(h.Sandbox.Create))

		sandboxByID := sandboxes.Group("/:id")
		sandboxByID.GET("", handler.Handle(h.Sandbox.Get))
		sandboxByID.DELETE("", handler.Handle(h.Sandbox.Delete))
		sandboxByID.POST("/start", handler.Handle(h.Sandbox.Start))
		sandboxByID.POST("/stop", handler.Handle(h.Sandbox.Stop))
		sandboxByID.POST("/pause", handler.Handle(h.Sandbox.Pause))
		sandboxByID.POST("/resume", handler.Handle(h.Sandbox.Resume))
		sandboxByID.POST("/exec", handler.Handle(h.Exec.Exec))
		sandboxByID.POST("/exec-stream", handler.Handle(h.Exec.ExecStream))
		sandboxByID.POST("/session-exec", handler.Handle(h.Exec.SessionExec))
		sandboxByID.POST("/session-exec-stream", handler.Handle(h.Exec.SessionExecStream))

		// Commands (Process Management)
		sandboxByID.POST("/commands/run", handler.Handle(h.Commands.Run))
		sandboxByID.GET("/commands/list", handler.Handle(h.Commands.List))
		sandboxByID.POST("/commands/kill", handler.Handle(h.Commands.Kill))
		sandboxByID.POST("/commands/attach", handler.Handle(h.Commands.Attach))
		sandboxByID.POST("/commands/wait", handler.Handle(h.Commands.Wait))

		// PTY Session Management
		sandboxByID.GET("/pty", handler.Handle(h.PTY.Proxy))
		sandboxByID.POST("/pty/sessions", handler.Handle(h.PTY.CreateSession))
		sandboxByID.GET("/pty/sessions", handler.Handle(h.PTY.ListSessions))
		sandboxByID.GET("/pty/sessions/:sessionId", handler.Handle(h.PTY.ConnectSession))
		sandboxByID.DELETE("/pty/sessions/:sessionId", handler.Handle(h.PTY.DeleteSession))
		sandboxByID.POST("/pty/sessions/:sessionId/execute", handler.Handle(h.PTY.ExecuteCommand))
		sandboxByID.GET("/pty/sessions/:sessionId/buffer", handler.Handle(h.PTY.GetBuffer))
		sandboxByID.POST("/pty/sessions/:sessionId/resize", handler.Handle(h.PTY.ResizeTerminal))

		sandboxByID.GET("/files", handler.Handle(h.FS.ListFiles))
		sandboxByID.GET("/files/download", handler.Handle(h.FS.DownloadFile))
		sandboxByID.POST("/files/upload", handler.Handle(h.FS.UploadFile))
		sandboxByID.POST("/files/mkdir", handler.Handle(h.FS.CreateDirectory))
		sandboxByID.POST("/files/create", handler.Handle(h.FS.CreateFile))
		sandboxByID.POST("/files/copy", handler.Handle(h.FS.CopyFile))
		sandboxByID.GET("/files/head-tail", handler.Handle(h.FS.HeadTail))
		sandboxByID.POST("/files/chmod", handler.Handle(h.FS.ChangePermissions))
		sandboxByID.GET("/files/du", handler.Handle(h.FS.DiskUsage))
		sandboxByID.GET("/files/search", handler.Handle(h.FS.SearchFiles))
		sandboxByID.POST("/files/compress", handler.Handle(h.FS.CompressFile))
		sandboxByID.POST("/files/extract", handler.Handle(h.FS.ExtractArchive))
		sandboxByID.DELETE("/files", handler.Handle(h.FS.DeleteFile))
		sandboxByID.POST("/files/move", handler.Handle(h.FS.MoveFile))
		sandboxByID.GET("/files/stat", handler.Handle(h.FS.StatFile))

		// File watch routes
		sandboxByID.POST("/files/watch", handler.Handle(h.FS.StartWatch))
		sandboxByID.GET("/files/watch/:sessionId/stream", handler.Handle(h.FS.StreamWatchEvents))
	}

	// Image routes
	images := protected.Group("/images")
	{
		images.GET("", handler.Handle(h.Image.List))
		// images.POST("", handler.Handle(h.Image.Create))
		images.GET("/:id", handler.Handle(h.Image.Get))
		// images.DELETE("/:id", handler.Handle(h.Image.Delete))
		images.GET("/name/:name", handler.Handle(h.Image.GetByName))
	}

	// Org routes with auth middleware (API Key required)
	org := protected.Group("/orgs")
	{
		org.GET("/users", handler.Handle(h.Org.GetOrgUsers))

		// API key routes under org
		apiKeys := org.Group("/apikeys")
		apiKeys.GET("", handler.Handle(h.Org.ListAPIKeys))
		apiKeys.POST("", handler.Handle(h.Org.GenerateAPIKey))
		apiKeys.DELETE("/:keyId", handler.Handle(h.Org.DeleteAPIKey))
		apiKeys.POST("/:keyId/activate", handler.Handle(h.Org.ActivateAPIKey))
		apiKeys.PATCH("/:keyId/touch", handler.Handle(h.Org.TouchAPIKey))
	}

	// User routes
	user := protected.Group("/users")
	{
		user.GET("/me", handler.Handle(h.User.GetMe))
	}

	// MCP (Model Context Protocol) endpoint — single route handles all MCP methods
	protected.Any("/mcp", handler.Handle(h.MCP.Handle))

	return r
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
