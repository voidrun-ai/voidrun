package main

import (
	"flag"
	"fmt"
	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/machine"
	"voidrun/pkg/storage"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: voidrun <create|snapshot|restore|stop>")
	}

	cfg := config.New()

	switch os.Args[1] {
	case "create":
		cCmd := flag.NewFlagSet("create", flag.ExitOnError)
		id := cCmd.String("id", "", "VM ID")
		imageType := cCmd.String("image", "alpine", "Image Type")
		ip := cCmd.String("ip", "192.168.100.10", "Unique IP")
		cCmd.Parse(os.Args[2:])

		if *id == "" {
			log.Fatal("--id required")
		}

		spec := model.SandboxSpec{
			ID:        *id,
			Type:      *imageType,
			CPUs:      2,
			MemoryMB:  512,
			DiskMB:    10240,
			IPAddress: *ip,
		}

		fmt.Printf(">> Creating Native Sandbox %s (%s)...\n", *id, *ip)
		overlay, err := storage.PrepareInstance(*cfg, spec)
		if err != nil {
			log.Fatalf("Storage error: %v", err)
		}

		// Pass "" for restorePath (New VM)
		if err := machine.Start(*cfg, spec, overlay, ""); err != nil {
			log.Fatalf("Start error: %v", err)
		}

	case "snapshot":
		sCmd := flag.NewFlagSet("snapshot", flag.ExitOnError)
		id := sCmd.String("id", "", "VM ID")
		sCmd.Parse(os.Args[2:])

		if *id == "" {
			log.Fatal("--id required")
		}
		if err := machine.CreateSnapshot(*id); err != nil {
			log.Fatalf("Snapshot error: %v", err)
		}

	case "restore":
		rCmd := flag.NewFlagSet("restore", flag.ExitOnError)
		id := rCmd.String("id", "", "New VM ID")
		src := rCmd.String("source", "", "Snapshot Path")
		ip := rCmd.String("ip", "", "New Unique IP")

		// New Flag
		cold := rCmd.Bool("cold", false, "Discard RAM state to allow IP change")

		rCmd.Parse(os.Args[2:])

		if *id == "" || *src == "" || *ip == "" {
			log.Fatal("Usage: restore --id <id> --source <path> --ip <ip> [--cold]")
		}

		if err := machine.Restore(*cfg, *id, *src, *ip, *cold); err != nil {
			log.Fatalf("Restore error: %v", err)
		}

	case "stop":
		sCmd := flag.NewFlagSet("stop", flag.ExitOnError)
		id := sCmd.String("id", "", "VM ID")
		sCmd.Parse(os.Args[2:])
		if *id == "" {
			log.Fatal("--id required")
		}
		if err := machine.Stop(*id); err != nil {
			log.Fatalf("Stop error: %v", err)
		}

	default:
		fmt.Println("Unknown command")
	}
}
