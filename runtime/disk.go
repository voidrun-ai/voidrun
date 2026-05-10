package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"voidrun/config"
	"voidrun/model"
	"voidrun/util"
)

var (
	baseImageSizeCache sync.Map
)

func PrepareInstance(ctx context.Context, cfg config.Config, spec model.SandboxSpec) (string, error) {
	defer util.Track("PrepareInstance (Total)")()

	log.Printf("[DEBUG] Preparing instance %s with spec: %+v", spec.ID, spec)
	baseName := ""
	if idx := strings.Index(spec.Type, ":"); idx != -1 {
		name := spec.Type[:idx]
		tag := spec.Type[idx+1:]
		baseName = fmt.Sprintf("%s-%s.qcow2", name, tag)
	}
	basePath := filepath.Join(cfg.Paths.BaseImagesDir, baseName)

	// Use centralized path helpers
	instanceDir := GetInstanceDir(spec.ID)
	overlayPath := GetOverlayPath(spec.ID)

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

	// log.Printf("[DEBUG] Creating overlay: %s -> %s (Size: %s)", basePath, overlayPath, sizeArg)

	// Force qcow2-flat for debugging
	if cfg.Sandbox.DiskFormat == "qcow2-flat" {
		log.Printf("[DEBUG] Cloning QCOW2 flat image: %s -> %s", basePath, overlayPath)
		cpCmd := exec.CommandContext(cmdCtx, "cp", "--reflink=always", basePath, overlayPath)
		if output, err := cpCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("cp reflink failed: %v. Output: %s", err, string(output))
		}
		if spec.DiskMB > baseMB {
			log.Printf("[DEBUG] Resizing overlay to: %s", sizeArg)
			resizeCmd := exec.CommandContext(cmdCtx, "qemu-img", "resize", overlayPath, sizeArg)
			if output, err := resizeCmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("qemu-img resize failed: %v. Output: %s", err, string(output))
			}
		}
	} else {
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
	}

	return overlayPath, nil
}

func PrepareInstanceRaw(ctx context.Context, cfg config.Config, spec model.SandboxSpec) (string, error) {
	defer util.Track("PrepareInstance (Total)")()

	// 1. IMPORTANT: We now expect a .raw base image instead of .qcow2
	baseName := spec.Type + "-base.raw"
	if idx := strings.Index(spec.Type, ":"); idx != -1 {
		name := spec.Type[:idx]
		tag := spec.Type[idx+1:]
		baseName = fmt.Sprintf("%s-%s.raw", name, tag)
	}
	basePath := filepath.Join(cfg.Paths.BaseImagesDir, baseName)

	instanceDir := GetInstanceDir(spec.ID)
	overlayPath := GetOverlayPath(spec.ID) // Make sure this returns a .raw path

	if _, err := os.Stat(basePath); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("base image missing at path: %s", basePath)
		}
		return "", fmt.Errorf("failed to stat base image: %w", err)
	}

	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance dir %s: %w", instanceDir, err)
	}

	baseMB, err := getCachedBaseSize(ctx, basePath)
	if err != nil {
		log.Printf("[WARN] Could not determine base image size: %v. Proceeding blindly.", err)
		baseMB = spec.DiskMB // Fallback
	} else if spec.DiskMB < baseMB {
		log.Printf("[INFO] Instance %s: Requested %dMB < Base %dMB. Bumping size.", spec.ID, spec.DiskMB, baseMB)
		spec.DiskMB = baseMB
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 2. INSTANT CLONE: Use filesystem reflink instead of qcow2 backing files.
	// If your OS filesystem supports it (XFS, Btrfs, or modern ext4), this takes 1 millisecond
	// and uses exactly 0 bytes of extra disk space.
	log.Printf("[DEBUG] Cloning RAW image: %s -> %s", basePath, overlayPath)

	cpCmd := exec.CommandContext(cmdCtx, "cp", "--reflink=always", basePath, overlayPath)
	if output, err := cpCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cp reflink failed: %v. Output: %s", err, string(output))
	}

	log.Printf("[DEBUG] Cloned RAW image: %s -> %s", basePath, overlayPath)

	// 3. INSTANT RESIZE: If the requested size is larger than the base image,
	// we use 'truncate' to instantly grow the RAW sparse file to the exact size.
	if spec.DiskMB > baseMB {
		sizeArg := fmt.Sprintf("%dM", spec.DiskMB)
		log.Printf("[DEBUG] Resizing overlay to: %s", sizeArg)

		resizeCmd := exec.CommandContext(cmdCtx, "truncate", "-s", sizeArg, overlayPath)
		if output, err := resizeCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("truncate resize failed: %v. Output: %s", err, string(output))
		}
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
