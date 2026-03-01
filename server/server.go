package server

import (
	"context"
	"fmt"
	"time"

	"voidrun/config"
	"voidrun/metrics"
	"voidrun/middleware"
	machine "voidrun/runtime"
	"voidrun/util"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Server represents the HTTP server
type Server struct {
	cfg      *config.Config
	router   *gin.Engine
	mongo    *mongo.Client
	services *Services
	metrics  *metrics.Manager
	stopFn   context.CancelFunc
}

// New creates a new server instance
func New(cfg *config.Config) (*Server, error) {
	// Initialize machine package with config paths
	machine.SetInstancesRoot(cfg.Paths.InstancesDir)
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

	router := setupRouter(cfg, handlers, services)

	return &Server{
		cfg:      cfg,
		router:   router,
		mongo:    mongoClient,
		services: services,
		metrics:  metricsManager,
		stopFn:   stopFn,
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

// Close disconnects MongoDB client
func (s *Server) Close() error {
	if s.stopFn != nil {
		s.stopFn()
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

func setupRouter(cfg *config.Config, h *Handlers, s *Services) *gin.Engine {
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

	// Static files
	r.Static("/ui", "./static")

	api := r.Group("/api")

	// Public metadata routes
	api.GET("/version", h.Version.Get)

	// Registration route (no auth)
	api.POST("/register", h.Auth.Register)

	// Protected routes require API key or JWT auth
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(cfg, s.APIKey, s.Org, s.User))

	// Sandbox routes
	sandboxes := protected.Group("/sandboxes")
	{
		sandboxes.GET("", middleware.RequirePermission("sandbox:read"), h.Sandbox.List)
		sandboxes.POST("", middleware.RequirePermission("sandbox:create"), h.Sandbox.Create)

		sandboxByID := sandboxes.Group("/:id")
		sandboxByID.Use(middleware.SandboxAccessMiddleware(s.Sandbox))
		sandboxByID.GET("", middleware.RequirePermission("sandbox:read"), h.Sandbox.Get)
		sandboxByID.DELETE("", middleware.RequirePermission("sandbox:delete"), h.Sandbox.Delete)
		sandboxByID.POST("/start", middleware.RequirePermission("sandbox:update"), h.Sandbox.Start)
		sandboxByID.POST("/stop", middleware.RequirePermission("sandbox:update"), h.Sandbox.Stop)
		sandboxByID.POST("/pause", middleware.RequirePermission("sandbox:update"), h.Sandbox.Pause)
		sandboxByID.POST("/resume", middleware.RequirePermission("sandbox:update"), h.Sandbox.Resume)
		sandboxByID.POST("/exec", middleware.RequirePermission("sandbox:exec"), h.Exec.Exec)
		sandboxByID.POST("/exec-stream", middleware.RequirePermission("sandbox:exec"), h.Exec.ExecStream)
		sandboxByID.POST("/session-exec", middleware.RequirePermission("sandbox:exec"), h.Exec.SessionExec)
		sandboxByID.POST("/session-exec-stream", middleware.RequirePermission("sandbox:exec"), h.Exec.SessionExecStream)

		// Commands (Process Management)
		sandboxByID.POST("/commands/run", middleware.RequirePermission("sandbox:exec"), h.Commands.Run)
		sandboxByID.GET("/commands/list", middleware.RequirePermission("sandbox:exec"), h.Commands.List)
		sandboxByID.POST("/commands/kill", middleware.RequirePermission("sandbox:exec"), h.Commands.Kill)
		sandboxByID.POST("/commands/attach", middleware.RequirePermission("sandbox:exec"), h.Commands.Attach)
		sandboxByID.POST("/commands/wait", middleware.RequirePermission("sandbox:exec"), h.Commands.Wait)

		// PTY Session Management
		sandboxByID.GET("/pty", middleware.RequirePermission("sandbox:pty"), h.PTY.Proxy)
		sandboxByID.POST("/pty/sessions", middleware.RequirePermission("sandbox:pty"), h.PTY.CreateSession)
		sandboxByID.GET("/pty/sessions", middleware.RequirePermission("sandbox:pty"), h.PTY.ListSessions)
		sandboxByID.GET("/pty/sessions/:sessionId", middleware.RequirePermission("sandbox:pty"), h.PTY.ConnectSession)
		sandboxByID.DELETE("/pty/sessions/:sessionId", middleware.RequirePermission("sandbox:pty"), h.PTY.DeleteSession)
		sandboxByID.POST("/pty/sessions/:sessionId/execute", middleware.RequirePermission("sandbox:pty"), h.PTY.ExecuteCommand)
		sandboxByID.GET("/pty/sessions/:sessionId/buffer", middleware.RequirePermission("sandbox:pty"), h.PTY.GetBuffer)
		sandboxByID.POST("/pty/sessions/:sessionId/resize", middleware.RequirePermission("sandbox:pty"), h.PTY.ResizeTerminal)

		sandboxByID.GET("/files", middleware.RequirePermission("sandbox:fs"), h.FS.ListFiles)
		sandboxByID.GET("/files/download", middleware.RequirePermission("sandbox:fs"), h.FS.DownloadFile)
		sandboxByID.POST("/files/upload", middleware.RequirePermission("sandbox:fs"), h.FS.UploadFile)
		sandboxByID.POST("/files/mkdir", middleware.RequirePermission("sandbox:fs"), h.FS.CreateDirectory)
		sandboxByID.POST("/files/create", middleware.RequirePermission("sandbox:fs"), h.FS.CreateFile)
		sandboxByID.POST("/files/copy", middleware.RequirePermission("sandbox:fs"), h.FS.CopyFile)
		sandboxByID.GET("/files/head-tail", middleware.RequirePermission("sandbox:fs"), h.FS.HeadTail)
		sandboxByID.POST("/files/chmod", middleware.RequirePermission("sandbox:fs"), h.FS.ChangePermissions)
		sandboxByID.GET("/files/du", middleware.RequirePermission("sandbox:fs"), h.FS.DiskUsage)
		sandboxByID.GET("/files/search", middleware.RequirePermission("sandbox:fs"), h.FS.SearchFiles)
		sandboxByID.POST("/files/compress", middleware.RequirePermission("sandbox:fs"), h.FS.CompressFile)
		sandboxByID.POST("/files/extract", middleware.RequirePermission("sandbox:fs"), h.FS.ExtractArchive)
		sandboxByID.DELETE("/files", middleware.RequirePermission("sandbox:fs"), h.FS.DeleteFile)
		sandboxByID.POST("/files/move", middleware.RequirePermission("sandbox:fs"), h.FS.MoveFile)
		sandboxByID.GET("/files/stat", middleware.RequirePermission("sandbox:fs"), h.FS.StatFile)

		// File watch routes
		sandboxByID.POST("/files/watch", middleware.RequirePermission("sandbox:fs"), h.FS.StartWatch)
		sandboxByID.GET("/files/watch/:sessionId/stream", middleware.RequirePermission("sandbox:fs"), h.FS.StreamWatchEvents)
	}

	// Image routes
	images := protected.Group("/images")
	{
		images.GET("", middleware.RequirePermission("image:read"), h.Image.List)
		images.POST("", middleware.RequirePermission("image:create"), h.Image.Create)
		images.GET("/:id", middleware.RequirePermission("image:read"), h.Image.Get)
		images.DELETE("/:id", middleware.RequirePermission("image:delete"), h.Image.Delete)
		images.GET("/name/:name", middleware.RequirePermission("image:read"), h.Image.GetByName)
	}

	// Org routes with auth middleware (API Key required)
	org := protected.Group("/orgs")
	{
		org.GET("/me", middleware.RequirePermission("org:read"), h.Org.GetCurrentOrg)
		org.GET("/:orgId/users", middleware.RequirePermission("org:read"), h.Org.GetOrgUsers)

		// API key routes under org
		apiKeys := org.Group("/:orgId/apikeys")
		apiKeys.GET("", middleware.RequirePermission("apikey:read"), h.Org.ListAPIKeys)
		apiKeys.POST("", middleware.RequirePermission("apikey:create"), h.Org.GenerateAPIKey)
		apiKeys.DELETE("/:keyId", middleware.RequirePermission("apikey:delete"), h.Org.DeleteAPIKey)
		apiKeys.POST("/:keyId/activate", middleware.RequirePermission("apikey:update"), h.Org.ActivateAPIKey)
		apiKeys.PATCH("/:keyId/touch", middleware.RequirePermission("apikey:update"), h.Org.TouchAPIKey)
	}

	return r
}
