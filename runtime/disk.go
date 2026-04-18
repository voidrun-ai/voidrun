package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"voidrun/config"
	"voidrun/model"
	"voidrun/util"
)

var (
	baseImageSizeCache sync.Map
	// Regex to ensure IDs only contain safe characters (alphanumeric, hyphens, underscores)
	safePathRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

// PrepareStorage dispatches to the correct disk preparation strategy based on config.
//   - "qcow2"      = thin overlay with backing file (default, native Linux with CH packages)
//   - "qcow2-flat" = standalone qcow2 copy, no backing file (WSL2 / CH static binary)
func PrepareStorage(ctx context.Context, cfg config.Config, spec model.SandboxSpec) (string, error) {
	switch cfg.Sandbox.DiskFormat {
	case "raw":
		log.Printf("[disk] Using RAW reflink strategy")
		return PrepareInstanceRaw(ctx, cfg, spec)
	case "qcow2-flat":
		log.Printf("[disk] Using flat qcow2 (no backing file) strategy")
		return PrepareInstanceFlat(ctx, cfg, spec)
	case "qcow2", "":
		log.Printf("[disk] Using qcow2 overlay (backing file) strategy")
		return PrepareInstance(ctx, cfg, spec)
	default:
		return "", fmt.Errorf("unknown SANDBOX_DISK_FORMAT: %q (use qcow2, qcow2-flat, or raw)", cfg.Sandbox.DiskFormat)
	}
}

// PrepareInstance creates a qcow2 overlay backed by the base image (thin provisioning).
// This is the fastest and most space-efficient method but requires CH builds with backing file support.
func PrepareInstance(ctx context.Context, cfg config.Config, spec model.SandboxSpec) (string, error) {
	defer util.Track("PrepareInstance (Total)")()

	if !safePathRegex.MatchString(spec.ID) {
		return "", fmt.Errorf("invalid characters in spec ID: %q", spec.ID)
	}

	baseName := spec.Type + "-base.qcow2"
	if idx := strings.Index(spec.Type, ":"); idx != -1 {
		name := spec.Type[:idx]
		tag := spec.Type[idx+1:]
		baseName = fmt.Sprintf("%s-%s.qcow2", name, tag)
	}
	basePath := filepath.Join(cfg.Paths.BaseImagesDir, baseName)

	instanceDir := GetInstanceDir(spec.ID)
	overlayPath := GetOverlayPath(spec.ID)

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		return "", fmt.Errorf("base image missing at path: %s (ensure you have created the base image)", basePath)
	}

	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance dir %s: %w", instanceDir, err)
	}

	baseMB, err := getCachedBaseSize(ctx, basePath)
	log.Printf("[INFO] requested size: %dMB, imagePath: %s, baseMB: %d", spec.DiskMB, basePath, baseMB)
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

// PrepareInstanceFlat creates a standalone qcow2 copy without backing files.
// Use this on WSL2 or when Cloud Hypervisor's static binary doesn't support backing files.
func PrepareInstanceFlat(ctx context.Context, cfg config.Config, spec model.SandboxSpec) (string, error) {
	defer util.Track("PrepareInstanceFlat (Total)")()

	if !safePathRegex.MatchString(spec.ID) {
		return "", fmt.Errorf("invalid characters in spec ID: %q", spec.ID)
	}

	baseName := spec.Type + "-base.qcow2"
	basePath := filepath.Join(cfg.Paths.BaseImagesDir, baseName)

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
		baseMB = spec.DiskMB
	} else {
		if spec.DiskMB < baseMB {
			log.Printf("[INFO] Instance %s: Requested %dMB < Base %dMB. Bumping size.", spec.ID, spec.DiskMB, baseMB)
			spec.DiskMB = baseMB
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	log.Printf("[DEBUG] Creating flat qcow2 copy: %s -> %s", basePath, overlayPath)

	convertCmd := exec.CommandContext(cmdCtx, "qemu-img", "convert",
		"-f", "qcow2",
		"-O", "qcow2",
		basePath,
		overlayPath,
	)
	if output, err := convertCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("qemu-img convert failed: %v. Output: %s", err, string(output))
	}

	if spec.DiskMB > baseMB {
		sizeArg := fmt.Sprintf("%dM", spec.DiskMB)
		log.Printf("[DEBUG] Resizing overlay to: %s", sizeArg)
		resizeCmd := exec.CommandContext(cmdCtx, "qemu-img", "resize", overlayPath, sizeArg)
		if output, err := resizeCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("qemu-img resize failed: %v. Output: %s", err, string(output))
		}
	}

	return overlayPath, nil
}

// PrepareInstanceRaw uses raw disk images for fastest boot on WSL2/Cloud Hypervisor static builds.
// It auto-converts the qcow2 base image to raw if the raw base doesn't exist yet.
// Uses cp --reflink=auto for copy (instant on XFS/Btrfs, regular copy on ext4/WSL2).
func PrepareInstanceRaw(ctx context.Context, cfg config.Config, spec model.SandboxSpec) (string, error) {
	defer util.Track("PrepareInstanceRaw (Total)")()

	if !safePathRegex.MatchString(spec.ID) {
		return "", fmt.Errorf("invalid characters in spec ID: %q", spec.ID)
	}

	// Check for the raw base image; auto-convert from qcow2 if missing
	rawBaseName := spec.Type + "-base.raw"
	rawBasePath := filepath.Join(cfg.Paths.BaseImagesDir, rawBaseName)

	if _, err := os.Stat(rawBasePath); os.IsNotExist(err) {
		// Try to find the qcow2 source and convert it
		qcow2BaseName := spec.Type + "-base.qcow2"
		qcow2BasePath := filepath.Join(cfg.Paths.BaseImagesDir, qcow2BaseName)

		if _, err := os.Stat(qcow2BasePath); os.IsNotExist(err) {
			return "", fmt.Errorf("no base image found: tried %s and %s", rawBasePath, qcow2BasePath)
		}

		log.Printf("[disk] Raw base image not found at %s — converting from %s (one-time operation)...", rawBasePath, qcow2BasePath)

		convCtx, convCancel := context.WithTimeout(ctx, 5*time.Minute)
		defer convCancel()

		// Convert to a temp file first, then rename (atomic)
		tmpPath := rawBasePath + ".converting"
		convCmd := exec.CommandContext(convCtx, "qemu-img", "convert",
			"-f", "qcow2",
			"-O", "raw",
			qcow2BasePath,
			tmpPath,
		)
		if output, err := convCmd.CombinedOutput(); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("qemu-img convert qcow2->raw failed: %v. Output: %s", err, string(output))
		}

		if err := os.Rename(tmpPath, rawBasePath); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("failed to rename converted image: %w", err)
		}
		log.Printf("[disk] Converted %s -> %s successfully", qcow2BasePath, rawBasePath)
	} else if err != nil {
		return "", fmt.Errorf("failed to stat base image: %w", err)
	}

	instanceDir := GetInstanceDir(spec.ID)
	overlayPath := GetRawOverlayPath(spec.ID)

	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance dir %s: %w", instanceDir, err)
	}

	baseMB, err := getCachedBaseSize(ctx, rawBasePath)
	if err != nil {
		log.Printf("[WARN] Could not determine base image size: %v. Proceeding blindly.", err)
		baseMB = spec.DiskMB
	} else if spec.DiskMB < baseMB {
		log.Printf("[INFO] Instance %s: Requested %dMB < Base %dMB. Bumping size.", spec.ID, spec.DiskMB, baseMB)
		spec.DiskMB = baseMB
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Copy base image — use --reflink=auto so it's instant on XFS/Btrfs
	// and falls back to regular copy on ext4/WSL2
	log.Printf("[DEBUG] Copying RAW image: %s -> %s", rawBasePath, overlayPath)

	cpCmd := exec.CommandContext(cmdCtx, "cp", "--reflink=auto", rawBasePath, overlayPath)
	if output, err := cpCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cp failed: %v. Output: %s", err, string(output))
	}

	log.Printf("[DEBUG] Copied RAW image: %s -> %s", rawBasePath, overlayPath)

	// Resize if requested size is larger than base
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
