package runtime

// Decoupled snapshot/restore path; dispatched from lifecycle.go when DecoupledSnapshotEnabled.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"voidrun/config"
	"voidrun/model"
	"voidrun/util"
)

// decoupledMemZoneID is the fixed CH memory-zone id for file-backed guest RAM.
const decoupledMemZoneID = "mem0"

// decoupledRestoreMode is OnDemand (userfaultfd) for constant restore latency vs RAM size.
const decoupledRestoreMode = "OnDemand"

// buildDecoupledMemoryConfig maps to --memory size=0 --memory-zone id=mem0,...,shared=on.
func buildDecoupledMemoryConfig(cfg config.Config, spec model.SandboxSpec, ramPath string) *MemoryConfig {
	sizeBytes := int64(spec.MemoryMB) * 1024 * 1024
	return &MemoryConfig{
		Size:      0,
		Shared:    false,
		Mergeable: false,
		Zones: []MemoryZoneConfig{{
			ID:        decoupledMemZoneID,
			Size:      sizeBytes,
			File:      ramPath,
			Shared:    true,
			Hugepages: cfg.Sandbox.MemoryHugepages,
			Prefault:  cfg.Sandbox.MemoryPrefault,
		}},
	}
}

// buildDecoupledMemoryCLIArgs is the CLI equivalent of buildDecoupledMemoryConfig.
func buildDecoupledMemoryCLIArgs(cfg config.Config, spec model.SandboxSpec, ramPath string) []string {
	zone := fmt.Sprintf("id=%s,size=%dM,file=%s,shared=on",
		decoupledMemZoneID, spec.MemoryMB, ramPath)
	if cfg.Sandbox.MemoryHugepages {
		zone += ",hugepages=on"
	}
	if cfg.Sandbox.MemoryPrefault {
		zone += ",prefault=on"
	}
	return []string{"--memory", "size=0", "--memory-zone", zone}
}

// createDecoupled is Create with file-backed guest RAM.
func createDecoupled(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("Sandbox Start Decoupled (Total)")()

	overlayPath, _ = filepath.Abs(overlayPath)

	ramSize := int64(spec.MemoryMB) * 1024 * 1024
	if err := EnsureRAMFile(spec.ID, ramSize); err != nil {
		return fmt.Errorf("decoupled: %w", err)
	}
	ramPath := GetRAMFilePath(spec.ID)

	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)
	eventPath := GetEventPath(spec.ID)

	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + eventPath,
	}

	fmt.Printf(">> [Decoupled] Spawning empty CLH process inside NetNS %s...\n", spec.NetNSName)
	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.CHBinary}, args...)
	cmd := exec.Command("ip", netnsArgs...)

	logFile, _ := os.Create(logPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = RemoveRAMFile(spec.ID)
		return fmt.Errorf("decoupled: process start failed: %v", err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		_ = RemoveRAMFile(spec.ID)
		return err
	}
	cmd.Process.Release()

	apiClient := NewAPIClient(socketPath)
	if err := apiClient.WaitForSocket(2 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		Stop(spec.ID)
		return fmt.Errorf("decoupled: VM crashed on start. Logs:\n%s", string(logs))
	}

	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] decoupled EnsureTapBridge failed: %v", err)
	}

	debugConsole := cfg.Sandbox.DebugBootConsole
	consoleMode := "Null"
	if debugConsole {
		consoleMode = "Tty"
	}

	payload := PayloadConfig{
		Kernel:  cfg.Paths.KernelPath,
		Cmdline: strings.TrimSpace(cfg.Sandbox.KernelCmdline),
	}
	if cfg.Paths.InitrdPath != "" {
		initrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
		payload.Initramfs = initrdPath
	}

	vmCfg := VmConfig{
		Payload: &payload,
		Cpus: &CpusConfig{
			BootVcpus: spec.CPUs,
			MaxVcpus:  spec.CPUs,
		},
		Memory:  buildDecoupledMemoryConfig(cfg, spec, ramPath),
		Disks:   []DiskConfig{{Path: overlayPath}},
		Net:     []NetConfig{{ID: defaultNetDeviceID, Tap: spec.TapName, Mac: spec.MacAddress}},
		Rng:     &RngConfig{Src: "/dev/urandom"},
		Serial:  &ConsoleConfig{Mode: consoleMode},
		Console: &ConsoleConfig{Mode: consoleMode},
		Vsock: &VsockConfig{
			Cid:    getCidFromIP(spec.IPAddress),
			Socket: vsockPath,
		},
	}

	if cfg.Sandbox.BalloonEnabled {
		vmCfg.Balloon = &BalloonConfig{
			Size:           0,
			DeflateOnOOM:   true,
			FreePageReport: true,
		}
	}

	clhClient := NewCLHClient(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := clhClient.VmCreate(ctx, &vmCfg); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("decoupled vm.create failed: %w", err)
	}
	if err := clhClient.VmBoot(ctx); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("decoupled vm.boot failed: %w", err)
	}

	fmt.Printf("   [+] Decoupled VM Active! PID: %d, NetNS: %s, RAM: %s\n", pid, spec.NetNSName, ramPath)
	return nil
}

// buildCLIArgsDecoupled is BuildCLIArgs with file-backed memory and RAM landlock rule.
func buildCLIArgsDecoupled(cfg config.Config, spec model.SandboxSpec, overlayPath string) []string {
	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	vsockPath := GetVsockPath(spec.ID)
	eventPath := GetEventPath(spec.ID)
	ramPath := GetRAMFilePath(spec.ID)

	cmdLine := strings.TrimSpace(cfg.Sandbox.KernelCmdline)

	consoleMode := "off"
	if cfg.Sandbox.DebugBootConsole {
		consoleMode = "tty"
	}

	imageType := "qcow2"
	backingFiles := "on"
	if cfg.Sandbox.DiskFormat == "raw" {
		imageType = "raw"
		backingFiles = "off"
	} else if cfg.Sandbox.DiskFormat == "qcow2-flat" {
		imageType = "qcow2"
		backingFiles = "off"
	}

	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + eventPath,
		"--kernel", cfg.Paths.KernelPath,
		"--cmdline", cmdLine,
		"--cpus", fmt.Sprintf("boot=%d,max=%d", spec.CPUs, spec.CPUs),
	}
	args = append(args, buildDecoupledMemoryCLIArgs(cfg, spec, ramPath)...)
	args = append(args,
		"--disk", fmt.Sprintf("path=%s,backing_files=%s,image_type=%s", overlayPath, backingFiles, imageType),
		"--net", fmt.Sprintf("tap=%s,mac=%s", spec.TapName, spec.MacAddress),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", getCidFromIP(spec.IPAddress), vsockPath),
		"--rng", "src=/dev/urandom",
		"--serial", consoleMode,
		"--console", consoleMode,
	)

	if cfg.Paths.InitrdPath != "" {
		initrdPath, _ := filepath.Abs(cfg.Paths.InitrdPath)
		args = append(args, "--initramfs", initrdPath)
	}

	if cfg.Sandbox.BalloonEnabled {
		args = append(args, "--balloon", "size=0,deflate_on_oom=on,free_page_reporting=on")
	}

	if cfg.Sandbox.Seccomp {
		args = append(args, "--seccomp", "true", "--landlock")
		args = append(args, "--landlock-rules")
		args = append(args, buildDecoupledLandlockRules(cfg, spec, overlayPath, logPath, ramPath)...)
	}

	return args
}

// buildDecoupledLandlockRules adds rw access to the RAM backing file.
func buildDecoupledLandlockRules(cfg config.Config, spec model.SandboxSpec, overlayPath, logPath, ramPath string) []string {
	absKernel, _ := filepath.Abs(cfg.Paths.KernelPath)
	absBaseDir, _ := filepath.Abs(cfg.Paths.BaseImagesDir)
	absInstanceDir, _ := filepath.Abs(filepath.Dir(overlayPath))
	absRAMFile, _ := filepath.Abs(ramPath)

	backingFiles := "on"
	if cfg.Sandbox.DiskFormat == "raw" || cfg.Sandbox.DiskFormat == "qcow2-flat" {
		backingFiles = "off"
	}

	baseName := spec.Type + "-base.qcow2"
	if idx := strings.Index(spec.Type, ":"); idx != -1 {
		name := spec.Type[:idx]
		tag := spec.Type[idx+1:]
		baseName = fmt.Sprintf("%s-%s.qcow2", name, tag)
	}
	absBackingFile, _ := filepath.Abs(filepath.Join(absBaseDir, baseName))

	rulesMap := make(map[string]string)
	rulesMap[absKernel] = "r"
	rulesMap[logPath] = "rw"
	rulesMap[absInstanceDir] = "rw"
	rulesMap["/dev/urandom"] = "r"
	rulesMap["/dev/net/tun"] = "rw"
	rulesMap["/sys"] = "r"
	rulesMap[absRAMFile] = "rw"

	if cfg.Paths.InitrdPath != "" {
		absInitrd, _ := filepath.Abs(cfg.Paths.InitrdPath)
		rulesMap[absInitrd] = "r"
	}
	if backingFiles == "on" {
		absDataDir, _ := filepath.Abs(filepath.Dir(absBaseDir))
		rulesMap[absDataDir] = "r"
		rulesMap[absBaseDir] = "r"
		rulesMap[absBackingFile] = "r"
	}

	var paths []string
	for p := range rulesMap {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) < len(paths[j]) })

	var out []string
	for _, p := range paths {
		out = append(out, fmt.Sprintf("path=%s,access=%s", p, rulesMap[p]))
	}
	return out
}

// createCLIDecoupled is CreateCLI with file-backed guest RAM.
func createCLIDecoupled(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("Sandbox Start Decoupled (Total CLI)")()

	overlayPath, _ = filepath.Abs(overlayPath)

	ramSize := int64(spec.MemoryMB) * 1024 * 1024
	if err := EnsureRAMFile(spec.ID, ramSize); err != nil {
		return fmt.Errorf("decoupled: %w", err)
	}

	socketPath := GetSocketPath(spec.ID)
	logPath := GetLogPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)

	args := buildCLIArgsDecoupled(cfg, spec, overlayPath)
	log.Println(args)

	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.CHBinary}, args...)

	fmt.Printf(">> [Decoupled/CLI] Spawning full CLH process inside NetNS %s...\n", spec.NetNSName)
	cmd := exec.Command("ip", netnsArgs...)

	logFile, _ := os.Create(logPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = RemoveRAMFile(spec.ID)
		return fmt.Errorf("decoupled: process start failed: %v", err)
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		_ = RemoveRAMFile(spec.ID)
		return err
	}
	cmd.Process.Release()

	apiClient := NewAPIClient(socketPath)
	if err := apiClient.WaitForSocket(2 * time.Second); err != nil {
		logs, _ := os.ReadFile(logPath)
		Stop(spec.ID)
		return fmt.Errorf("decoupled: VM crashed on start. Logs:\n%s", string(logs))
	}

	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] decoupled EnsureTapBridge failed: %v", err)
	}

	fmt.Printf("   [+] Decoupled VM Active! PID: %d, NetNS: %s\n", pid, spec.NetNSName)
	return nil
}

// snapshotDecoupled: pause → metadata VmSnapshot → shutdown → sparse-move RAM to snapshot dir.
func snapshotDecoupled(id string) error {
	defer util.Track("lifecycle: Sandbox Snapshot Decoupled")()

	socketPath := GetSocketPath(id)
	baseSnapshotDir := GetSnapshotBaseDir(id)
	snapshotDir := filepath.Join(baseSnapshotDir, fmt.Sprintf("snap-%d", time.Now().UnixNano()))

	const snapshotTimeout = 30 * time.Second

	client := NewCLHClientWithTimeout(socketPath, snapshotTimeout)
	if !client.IsSocketAvailable() {
		return fmt.Errorf("Sandbox not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	defer cancel()

	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("decoupled: mkdir snapshot dir: %w", err)
	}

	if err := client.VmPause(ctx); err != nil {
		log.Printf("[SnapshotDecoupled] VmPause for %s: %v (continuing — may already be paused)", id, err)
	}

	snapshotURL := "file://" + snapshotDir + "/"
	if err := client.VmSnapshot(ctx, snapshotURL); err != nil {
		if resumeErr := client.VmResume(ctx); resumeErr != nil {
			log.Printf("[SnapshotDecoupled] VmResume after VmSnapshot failure for %s failed (%v); tearing VMM down", id, resumeErr)
			if sErr := shutdownVMM(ctx, client, id, socketPath, "SnapshotDecoupled cleanup"); sErr != nil {
				log.Printf("[SnapshotDecoupled] cleanup: %v", sErr)
			}
		}
		_ = os.RemoveAll(snapshotDir)
		return fmt.Errorf("decoupled VmSnapshot failed: %w", err)
	}

	// CH must not dump RAM when file-backed memory is active.
	if snapHasMemoryDump(snapshotDir) {
		_ = shutdownVMM(ctx, client, id, socketPath, "SnapshotDecoupled")
		_ = os.RemoveAll(snapshotDir)
		return fmt.Errorf("decoupled: CH wrote memory-ranges — file-backed RAM path not active for %s", id)
	}

	if err := shutdownVMM(ctx, client, id, socketPath, "SnapshotDecoupled"); err != nil {
		return err
	}

	if err := EvictRAMToSnapshot(id, snapshotDir); err != nil {
		_ = os.RemoveAll(snapshotDir)
		return fmt.Errorf("decoupled: %w", err)
	}

	log.Printf("[SnapshotDecoupled] VM %s snapshotted to %s", id, snapshotDir)

	if entries, err := os.ReadDir(baseSnapshotDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "snap-") {
				fullPath := filepath.Join(baseSnapshotDir, entry.Name())
				if fullPath != snapshotDir {
					if rmErr := os.RemoveAll(fullPath); rmErr != nil {
						log.Printf("[SnapshotDecoupled] cleanup old snap %s: %v", fullPath, rmErr)
					}
				}
			}
		}
	}

	return nil
}

// snapHasMemoryDump reports whether CH wrote memory-ranges/region files.
func snapHasMemoryDump(snapshotDir string) bool {
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "memory-ranges") || strings.HasPrefix(name, "memory-region") {
			return true
		}
	}
	return false
}

// restoreDecoupled restores parked RAM in parallel with empty-VMM spawn, then VmRestore (OnDemand).
func restoreDecoupled(cfg config.Config, spec model.SandboxSpec, overlayPath, snapshotDir string) error {
	defer util.Track("lifecycle: Sandbox Restore Decoupled")()

	if err := EnsureSandboxNetNS(cfg, &spec); err != nil {
		return fmt.Errorf("decoupled: ensure netns: %w", err)
	}

	overlayPath, _ = filepath.Abs(overlayPath)

	socketPath := GetSocketPath(spec.ID)
	pidPath := GetPIDPath(spec.ID)
	logPath := GetLogPath(spec.ID)

	os.Remove(socketPath)
	os.Remove(GetEventPath(spec.ID))
	os.Remove(GetEventOffsetPath(spec.ID))
	os.Remove(GetVsockPath(spec.ID))

	fmt.Printf(">> [RestoreDecoupled] Spawning empty CLH for %s inside NetNS %s (parallel with RAM restore)...\n", spec.ID, spec.NetNSName)

	ramErrCh := make(chan error, 1)
	ramDoneCh := make(chan time.Duration, 1)
	ramStart := time.Now()
	go func() {
		err := RestoreRAMFromSnapshot(spec.ID, snapshotDir)
		ramDoneCh <- time.Since(ramStart)
		ramErrCh <- err
	}()

	args := []string{
		"--api-socket", socketPath,
		"--log-file", logPath,
		"--event-monitor", "path=" + GetEventPath(spec.ID),
	}
	if cfg.Sandbox.Seccomp {
		args = append(args, "--seccomp", "true")
	}

	netnsArgs := append([]string{"netns", "exec", spec.NetNSName, cfg.CHBinary}, args...)
	cmd := exec.Command("ip", netnsArgs...)

	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	spawnStart := time.Now()
	spawnErr := cmd.Start()
	var pid int
	if spawnErr == nil {
		pid = cmd.Process.Pid
		if wrErr := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); wrErr != nil {
			cmd.Process.Kill()
			spawnErr = wrErr
		} else {
			cmd.Process.Release()
		}
	}

	if spawnErr != nil {
		<-ramErrCh
		_ = RemoveRAMFile(spec.ID)
		return fmt.Errorf("decoupled: process start failed during restore: %v", spawnErr)
	}

	apiClient := NewAPIClient(socketPath)
	if err := apiClient.WaitForSocket(2 * time.Second); err != nil {
		<-ramErrCh
		logs, _ := os.ReadFile(logPath)
		Stop(spec.ID)
		return fmt.Errorf("decoupled: CLH crashed before API socket appeared. Logs:\n%s", string(logs))
	}
	spawnDur := time.Since(spawnStart)

	if err := EnsureTapBridge(spec.NetNSName, spec.TapName); err != nil {
		log.Printf("[WARN] decoupled restore EnsureTapBridge failed: %v", err)
	}

	joinStart := time.Now()
	if err := <-ramErrCh; err != nil {
		Stop(spec.ID)
		return fmt.Errorf("decoupled: %w", err)
	}
	ramDur := <-ramDoneCh
	joinWait := time.Since(joinStart)
	log.Printf("[RestoreDecoupled/phase] %s spawn+socket=%v ramCopy=%v joinWait=%v",
		spec.ID, spawnDur, ramDur, joinWait)

	sourceURL := "file://" + snapshotDir
	if !strings.HasSuffix(sourceURL, "/") {
		sourceURL += "/"
	}

	clhClient := NewCLHClientWithTimeout(socketPath, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vmRestoreStart := time.Now()
	if err := clhClient.VmRestore(ctx, &RestoreConfig{
		SourceURL:         sourceURL,
		Prefault:          cfg.Sandbox.MemoryPrefault,
		Resume:            true,
		MemoryRestoreMode: decoupledRestoreMode,
	}); err != nil {
		Stop(spec.ID)
		return fmt.Errorf("decoupled vm.restore failed: %w", err)
	}
	log.Printf("[RestoreDecoupled/phase] %s vmRestoreAPI=%v", spec.ID, time.Since(vmRestoreStart))

	fmt.Printf("   [+] Decoupled VM %s Restored! PID: %d\n", spec.ID, pid)
	return nil
}

// bootFromDiskDecoupled is BootFromDisk with file-backed guest RAM.
func bootFromDiskDecoupled(cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	defer util.Track("lifecycle: BootFromDisk Decoupled")()

	if err := EnsureSandboxNetNS(cfg, &spec); err != nil {
		return fmt.Errorf("decoupled: ensure netns: %w", err)
	}

	os.Remove(GetSocketPath(spec.ID))
	os.Remove(GetEventPath(spec.ID))
	os.Remove(GetEventOffsetPath(spec.ID))
	os.Remove(GetVsockPath(spec.ID))

	return createCLIDecoupled(cfg, spec, overlayPath)
}

// stopDecoupled shuts down the VMM and removes the live RAM file.
func stopDecoupled(id string) error {
	defer util.Track("lifecycle: Sandbox Stop Decoupled")()
	socketPath := GetSocketPath(id)

	client := NewCLHClientForSandbox(id)
	if !client.IsSocketAvailable() {
		// Still purge orphan RAM when VMM is already gone.
		if err := RemoveRAMFile(id); err != nil {
			log.Printf("[StopDecoupled] RemoveRAMFile %s: %v", id, err)
		}
		return fmt.Errorf("Sandbox not running")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := shutdownVMM(ctx, client, id, socketPath, "StopDecoupled"); err != nil {
		return err
	}
	if err := RemoveRAMFile(id); err != nil {
		log.Printf("[StopDecoupled] RemoveRAMFile %s: %v", id, err)
	}
	log.Printf("[StopDecoupled] VM %s stopped", id)
	return nil
}

// deleteDecoupled is Delete plus live RAM file cleanup.
func deleteDecoupled(id, tapName, nsName string) error {
	socketPath := GetSocketPath(id)
	pidPath := GetPIDPath(id)

	client := NewCLHClient(socketPath)
	if client.IsSocketAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.VmDelete(ctx); err != nil {
			fmt.Printf("Warning: decoupled VmDelete failed for %s: %v\n", id, err)
		}
	}

	if data, err := os.ReadFile(pidPath); err == nil {
		pid, _ := strconv.Atoi(string(data))
		if process, err := os.FindProcess(pid); err == nil {
			process.Signal(syscall.SIGTERM)
			time.Sleep(100 * time.Millisecond)
		}
		os.Remove(pidPath)
	}

	if nsName != "" {
		if err := DeleteSandboxNetNS(nsName); err != nil {
			fmt.Printf("Warning: decoupled DeleteSandboxNetNS failed for %s (ns=%s): %v\n", id, nsName, err)
		}
	} else if tapName != "" {
		DeleteTap(tapName)
	}

	if err := RemoveRAMFile(id); err != nil {
		log.Printf("[DeleteDecoupled] RemoveRAMFile %s: %v", id, err)
	}

	return nil
}
