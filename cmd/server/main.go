package main

import (
	"log"

	"voidrun/config"
	"voidrun/server"

	_ "voidrun/plugins/cloudhypervisor"
	_ "voidrun/plugins/firecracker"

	"github.com/joho/godotenv"
)

func main() {
	// Load .env file if it exists (ignore error if file doesn't exist)
	_ = godotenv.Load()

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
