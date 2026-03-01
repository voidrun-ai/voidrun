package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/machine"
	"voidrun/pkg/timer"
)

var (
	baseImageSizeCache sync.Map
	// Regex to ensure IDs only contain safe characters (alphanumeric, hyphens, underscores)
	safePathRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

func PrepareInstance(ctx context.Context, cfg config.Config, spec model.SandboxSpec) (string, error) {
	defer timer.Track("PrepareInstance (Total)")()

	if !safePathRegex.MatchString(spec.ID) {
		return "", fmt.Errorf("invalid characters in spec ID: %q", spec.ID)
	}

	baseName := spec.Type + "-base.qcow2"
	basePath := filepath.Join(cfg.Paths.BaseImagesDir, baseName)

	// Use centralized path helpers
	instanceDir := machine.GetInstanceDir(spec.ID)
	overlayPath := machine.GetOverlayPath(spec.ID)

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		return "", fmt.Errorf("base image missing at path: %s (ensure you have created the base image)", basePath)
	}

	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance dir %s: %w", instanceDir, err)
	}

	baseMB, err := getCachedBaseSize(ctx, basePath)
	if err != nil {
		log.Printf("[WARN] Could not determine base image size: %v. Proceeding blindly.", err)
	} else {
		if spec.DiskMB < baseMB {
			log.Printf("[INFO] Instance %s: Requested %dMB < Base %dMB. Bumping size.", spec.ID, spec.DiskMB, baseMB)
			spec.DiskMB = baseMB
		}
	}

	sizeArg := fmt.Sprintf("%dM", spec.DiskMB)

	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	log.Printf("[DEBUG] Creating overlay: %s -> %s (Size: %s)", basePath, overlayPath, sizeArg)

	cmd := exec.CommandContext(cmdCtx, "qemu-img", "create",
		"-f", "qcow2",
		"-b", basePath,
		"-F", "qcow2",
		overlayPath,
		sizeArg,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("qemu-img create failed: %v. Output: %s", err, string(output))
	}

	return overlayPath, nil
}

func getCachedBaseSize(ctx context.Context, imagePath string) (int, error) {
	if val, ok := baseImageSizeCache.Load(imagePath); ok {
		return val.(int), nil
	}

	mb, err := getQcow2VirtualSizeMB(ctx, imagePath)
	if err != nil {
		return 0, err
	}

	baseImageSizeCache.Store(imagePath, mb)
	return mb, nil
}

func getQcow2VirtualSizeMB(ctx context.Context, imagePath string) (int, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "qemu-img", "info", "--output=json", imagePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("command failed: %v output: %s", err, string(output))
	}

	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return 0, fmt.Errorf("parse json: %w", err)
	}
	if info.VirtualSize <= 0 {
		return 0, fmt.Errorf("invalid virtual size: %d", info.VirtualSize)
	}

	mb := int((info.VirtualSize + (1024*1024 - 1)) / (1024 * 1024))
	return mb, nil
}
