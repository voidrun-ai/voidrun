package main

import (
	"log"

	"voidrun/internal/config"
	"voidrun/internal/server"
)

func main() {
	// Load configuration
	cfg := config.New()

	// Create and run server
	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	if err := srv.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
