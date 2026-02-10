package storage

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/pkg/timer"
)

func PrepareInstance(cfg config.Config, spec model.SandboxSpec) (string, error) {
	defer timer.Track("PrepareInstance (Total)")()

	// 1. Resolve Base Image Path
	baseName := spec.Type + "-base.qcow2"
	basePath := filepath.Join(cfg.Paths.BaseImagesDir, baseName)

	// if _, err := os.Stat(basePath); os.IsNotExist(err) {
	// 	return "", fmt.Errorf("base image missing: %s", basePath)
	// }

	// 2. Prepare Instance Directory (System Path)
	// We no longer use os.Getwd()
	instanceDir := filepath.Join(cfg.Paths.InstancesDir, spec.ID)

	if err := os.MkdirAll(instanceDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create instance dir: %v", err)
	}
	overlayPath := filepath.Join(instanceDir, "overlay.qcow2")
	sizeArg := fmt.Sprintf("%dM", spec.DiskMB)

	log.Printf("Preparing instance %s: base=%s overlay=%s size=%s", spec.ID, basePath, overlayPath, sizeArg)

	// 3. Create QCOW2 Overlay
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", "-b", basePath, "-F", "qcow2", overlayPath, sizeArg)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("qemu-img failed: %v: %s", err, string(output))
	}

	return overlayPath, nil
}
