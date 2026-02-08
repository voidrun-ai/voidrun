package server

import (
	"context"
	"fmt"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/middleware"
	"voidrun/pkg/machine"

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
}

// New creates a new server instance
func New(cfg *config.Config) (*Server, error) {
	// Initialize machine package with config paths
	machine.SetInstancesRoot(cfg.Paths.InstancesDir)

	mongoClient, err := Connect(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	db := mongoClient.Database(cfg.Mongo.Database)

	repos := InitRepositories(cfg, db)
	services := InitServices(cfg, repos)
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
	fmt.Printf("🚀 Hyper-Fleet Server running on %s\n", s.cfg.Server.Address())
	return s.router.Run(s.cfg.Server.Address())
}

func (s *Server) startHealthMonitor() {
	if !s.cfg.Health.Enabled {
		return
	}
	intervalSec := s.cfg.Health.IntervalSec
	if intervalSec <= 0 {
		intervalSec = 60
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

	// Static files
	r.Static("/ui", "./static")

	api := r.Group("/api")

	// Registration route (no auth)
	api.POST("/register", h.Auth.Register)

	// Protected routes require X-API-Key
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(s.APIKey))

	// Sandbox routes
	sandboxes := protected.Group("/sandboxes")
	{
		sandboxes.GET("", h.Sandbox.List)
		sandboxes.POST("", h.Sandbox.Create)
		// sandboxes.POST("/restore", h.Sandbox.Restore)
		sandboxes.GET("/:id", h.Sandbox.Get)
		sandboxes.DELETE("/:id", h.Sandbox.Delete)
		// sandboxes.POST("/:id/stop", h.Sandbox.Stop)
		// sandboxes.POST("/:id/pause", h.Sandbox.Pause)
		// sandboxes.POST("/:id/resume", h.Sandbox.Resume)
		// sandboxes.POST("/:id/snapshot", h.Sandbox.Snapshot)
		// sandboxes.GET("/:id/snapshots", h.Sandbox.ListSnapshots)
		sandboxes.POST("/:id/exec", h.Exec.Exec)
		sandboxes.POST("/:id/exec-stream", h.Exec.ExecStream)
		sandboxes.POST("/:id/session-exec", h.Exec.SessionExec)
		sandboxes.POST("/:id/session-exec-stream", h.Exec.SessionExecStream)

		// Commands (Process Management)
		sandboxes.POST("/:id/commands/run", h.Commands.Run)
		sandboxes.GET("/:id/commands/list", h.Commands.List)
		sandboxes.POST("/:id/commands/kill", h.Commands.Kill)
		sandboxes.POST("/:id/commands/attach", h.Commands.Attach)
		sandboxes.POST("/:id/commands/wait", h.Commands.Wait)

		// PTY Session Management
		sandboxes.GET("/:id/pty", h.PTY.Proxy)
		sandboxes.POST("/:id/pty/sessions", h.PTY.CreateSession)
		sandboxes.GET("/:id/pty/sessions", h.PTY.ListSessions)
		sandboxes.GET("/:id/pty/sessions/:sessionId", h.PTY.ConnectSession)
		sandboxes.DELETE("/:id/pty/sessions/:sessionId", h.PTY.DeleteSession)
		sandboxes.POST("/:id/pty/sessions/:sessionId/execute", h.PTY.ExecuteCommand)
		sandboxes.GET("/:id/pty/sessions/:sessionId/buffer", h.PTY.GetBuffer)
		sandboxes.POST("/:id/pty/sessions/:sessionId/resize", h.PTY.ResizeTerminal)

		sandboxes.GET("/:id/files", h.FS.ListFiles)
		sandboxes.GET("/:id/files/download", h.FS.DownloadFile)
		sandboxes.POST("/:id/files/upload", h.FS.UploadFile)
		sandboxes.POST("/:id/files/mkdir", h.FS.CreateDirectory)
		sandboxes.POST("/:id/files/create", h.FS.CreateFile)
		sandboxes.POST("/:id/files/copy", h.FS.CopyFile)
		sandboxes.GET("/:id/files/head-tail", h.FS.HeadTail)
		sandboxes.POST("/:id/files/chmod", h.FS.ChangePermissions)
		sandboxes.GET("/:id/files/du", h.FS.DiskUsage)
		sandboxes.GET("/:id/files/search", h.FS.SearchFiles)
		sandboxes.POST("/:id/files/compress", h.FS.CompressFile)
		sandboxes.POST("/:id/files/extract", h.FS.ExtractArchive)
		sandboxes.DELETE("/:id/files", h.FS.DeleteFile)
		sandboxes.POST("/:id/files/move", h.FS.MoveFile)
		sandboxes.GET("/:id/files/stat", h.FS.StatFile)

		// File watch routes
		sandboxes.POST("/:id/files/watch", h.FS.StartWatch)
		sandboxes.GET("/:id/files/watch/:sessionId/stream", h.FS.StreamWatchEvents)
	}

	// Image routes
	images := protected.Group("/images")
	{
		images.GET("", h.Image.List)
		images.POST("", h.Image.Create)
		images.GET("/:id", h.Image.Get)
		images.DELETE("/:id", h.Image.Delete)
		images.GET("/name/:name", h.Image.GetByName)
	}

	// Org routes with auth middleware (API Key required)
	org := protected.Group("/orgs")
	{
		org.GET("/me", h.Org.GetCurrentOrg)

		// API key routes under org
		apiKeys := org.Group("/:orgId/apikeys")
		apiKeys.GET("", h.Org.ListAPIKeys)
		apiKeys.POST("", h.Org.GenerateAPIKey)
		apiKeys.DELETE("/:keyId", h.Org.DeleteAPIKey)
		apiKeys.POST("/:keyId/activate", h.Org.ActivateAPIKey)
		apiKeys.PATCH("/:keyId/touch", h.Org.TouchAPIKey)
	}

	return r
}
