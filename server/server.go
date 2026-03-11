package server

import (
	"context"
	"fmt"
	"time"

	"voidrun/config"
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
func New(cfg *config.Config) (*Server, error) {
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
	middlewares := InitMiddlewares(cfg, services)

	if err := PopulateInitialData(cfg, repos); err != nil {
		return nil, fmt.Errorf("failed to populate initial data: %w", err)
	}

	router := setupRouter(cfg, handlers, services, middlewares)

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

func setupRouter(cfg *config.Config, h *Handlers, s *Services, mw *Middlewares) *gin.Engine {
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

	// Protected routes require API key or JWT auth
	protected := api.Group("")
	protected.Use(mw.Auth)

	// Sandbox routes
	sandboxes := protected.Group("/sandboxes")
	{
		sandboxes.GET("", h.Sandbox.List)
		sandboxes.POST("", h.Sandbox.Create)

		sandboxByID := sandboxes.Group("/:id")
		sandboxByID.GET("", h.Sandbox.Get)
		sandboxByID.DELETE("", h.Sandbox.Delete)
		sandboxByID.POST("/start", h.Sandbox.Start)
		sandboxByID.POST("/stop", h.Sandbox.Stop)
		sandboxByID.POST("/pause", h.Sandbox.Pause)
		sandboxByID.POST("/resume", h.Sandbox.Resume)
		sandboxByID.POST("/exec", h.Exec.Exec)
		sandboxByID.POST("/exec-stream", h.Exec.ExecStream)
		sandboxByID.POST("/session-exec", h.Exec.SessionExec)
		sandboxByID.POST("/session-exec-stream", h.Exec.SessionExecStream)

		// Commands (Process Management)
		sandboxByID.POST("/commands/run", h.Commands.Run)
		sandboxByID.GET("/commands/list", h.Commands.List)
		sandboxByID.POST("/commands/kill", h.Commands.Kill)
		sandboxByID.POST("/commands/attach", h.Commands.Attach)
		sandboxByID.POST("/commands/wait", h.Commands.Wait)

		// PTY Session Management
		sandboxByID.GET("/pty", h.PTY.Proxy)
		sandboxByID.POST("/pty/sessions", h.PTY.CreateSession)
		sandboxByID.GET("/pty/sessions", h.PTY.ListSessions)
		sandboxByID.GET("/pty/sessions/:sessionId", h.PTY.ConnectSession)
		sandboxByID.DELETE("/pty/sessions/:sessionId", h.PTY.DeleteSession)
		sandboxByID.POST("/pty/sessions/:sessionId/execute", h.PTY.ExecuteCommand)
		sandboxByID.GET("/pty/sessions/:sessionId/buffer", h.PTY.GetBuffer)
		sandboxByID.POST("/pty/sessions/:sessionId/resize", h.PTY.ResizeTerminal)

		sandboxByID.GET("/files", h.FS.ListFiles)
		sandboxByID.GET("/files/download", h.FS.DownloadFile)
		sandboxByID.POST("/files/upload", h.FS.UploadFile)
		sandboxByID.POST("/files/mkdir", h.FS.CreateDirectory)
		sandboxByID.POST("/files/create", h.FS.CreateFile)
		sandboxByID.POST("/files/copy", h.FS.CopyFile)
		sandboxByID.GET("/files/head-tail", h.FS.HeadTail)
		sandboxByID.POST("/files/chmod", h.FS.ChangePermissions)
		sandboxByID.GET("/files/du", h.FS.DiskUsage)
		sandboxByID.GET("/files/search", h.FS.SearchFiles)
		sandboxByID.POST("/files/compress", h.FS.CompressFile)
		sandboxByID.POST("/files/extract", h.FS.ExtractArchive)
		sandboxByID.DELETE("/files", h.FS.DeleteFile)
		sandboxByID.POST("/files/move", h.FS.MoveFile)
		sandboxByID.GET("/files/stat", h.FS.StatFile)

		// File watch routes
		sandboxByID.POST("/files/watch", h.FS.StartWatch)
		sandboxByID.GET("/files/watch/:sessionId/stream", h.FS.StreamWatchEvents)
	}

	// Image routes
	images := protected.Group("/images")
	{
		images.GET("", h.Image.List)
		// images.POST("", h.Image.Create)
		images.GET("/:id", h.Image.Get)
		// images.DELETE("/:id", h.Image.Delete)
		images.GET("/name/:name", h.Image.GetByName)
	}

	// Org routes with auth middleware (API Key required)
	org := protected.Group("/orgs")
	{
		org.GET("/users", h.Org.GetOrgUsers)

		// API key routes under org
		apiKeys := org.Group("/apikeys")
		apiKeys.GET("", h.Org.ListAPIKeys)
		apiKeys.POST("", h.Org.GenerateAPIKey)
		apiKeys.DELETE("/:keyId", h.Org.DeleteAPIKey)
		apiKeys.POST("/:keyId/activate", h.Org.ActivateAPIKey)
		apiKeys.PATCH("/:keyId/touch", h.Org.TouchAPIKey)
	}

	// User routes
	user := protected.Group("/users")
	{
		user.GET("/me", h.User.GetMe)
	}

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
