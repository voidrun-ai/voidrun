package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"voidrun/config"
	"voidrun/metrics"
	"voidrun/model"
	"voidrun/repository"
	"voidrun/runtime"
	"voidrun/util"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var ErrSandboxNotFound = errors.New("sandbox not found")

// SandboxService handles sandbox business logic
type SandboxService struct {
	repo       repository.ISandboxRepository
	imageRepo  repository.IImageRepository
	cfg        *config.Config
	metrics    *metrics.Manager
	monitor    *runtime.EventMonitor
	projection primitive.M
}

// NewSandboxService creates a new sandbox service
func NewSandboxService(cfg *config.Config, repo repository.ISandboxRepository, imageRepo repository.IImageRepository, metricsManager *metrics.Manager, monitor *runtime.EventMonitor) *SandboxService {
	return &SandboxService{
		repo:      repo,
		imageRepo: imageRepo,
		cfg:       cfg,
		metrics:   metricsManager,
		monitor:   monitor,
		projection: bson.M{
			"_id":            1,
			"name":           1,
			"image":          1,
			"cpu":            1,
			"mem":            1,
			"diskMB":         1,
			"status":         1,
			"autoSleep":      1,
			"lastActivityAt": 1,
			"pausedAt":       1,
			"stoppedAt":      1,
			"createdAt":      1,
			"orgId":          1,
			"createdBy":      1,
			"region":         1,
			"refId":          1,
		},
	}
}

func (s *SandboxService) ListByOrgPaginated(ctx context.Context, orgID primitive.ObjectID, page, pageSize int) ([]*model.Sandbox, int64, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = config.DefaultPageSize
	} else if pageSize > config.MaxPageSize {
		pageSize = config.MaxPageSize
	}

	filter := bson.M{"orgId": orgID}

	// Get total count
	total, err := s.repo.Count(ctx, orgID, filter)
	if err != nil {
		return nil, 0, 0, err
	}

	// Use projection to fetch only essential fields for list view
	skip := int64((page - 1) * pageSize)
	opts := options.FindOptions{}
	opts.SetSkip(skip)
	opts.SetLimit(int64(pageSize))
	opts.SetSort(bson.D{{Key: "_id", Value: -1}}) // Sort by _id descending (latest first, uses default index)
	opts.SetProjection(s.projection)
	sbxList, err := s.repo.FindAndOrg(ctx, orgID, filter, opts)
	if err != nil {
		return nil, 0, 0, err
	}

	if sbxList == nil {
		sbxList = []*model.Sandbox{}
	}
	return sbxList, total, pageSize, nil
}

func (s *SandboxService) Get(ctx context.Context, orgID primitive.ObjectID, id string) (*model.Sandbox, error) {
	return s.getOrgScopedSandbox(ctx, orgID, id)
}

func (s *SandboxService) IsRunning(ctx context.Context, orgID primitive.ObjectID, id string) (bool, error) {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return false, err
	}

	if sandbox.Status == "running" {
		return true, nil
	}
	return false, nil
}

func (s *SandboxService) Exists(ctx context.Context, orgID primitive.ObjectID, id string) bool {
	return s.repo.Exists(ctx, orgID, id)
}

func (s *SandboxService) Create(ctx context.Context, req model.CreateSandboxRequest) (*model.Sandbox, error) {
	ip, err := s.repo.NextAvailableIP()
	if err != nil {
		return nil, fmt.Errorf("IP allocation failed: %w", err)
	}

	// Generate ObjectID for filesystem-safe directory name
	objID := util.GenerateObjectID()
	instanceID := objID.Hex()

	// Apply defaults
	cpu := req.CPU
	if cpu == 0 {
		cpu = s.cfg.Sandbox.DefaultVCPUs
	}
	mem := req.Mem
	if mem == 0 {
		mem = s.cfg.Sandbox.DefaultMemoryMB
	}
	diskMB := s.cfg.Sandbox.DefaultDiskMB
	if req.Image == "" {
		req.Image = s.cfg.Sandbox.DefaultImage
	}

	// Resolve image name/tag to actual image record
	resolvedImg, err := s.imageRepo.ResolveImage(ctx, req.OrgID, req.Image)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve image %q: %w", req.Image, err)
	}
	// Update request to use the specific name:tag resolved
	req.Image = fmt.Sprintf("%s:%s", resolvedImg.Name, resolvedImg.Tag)

	if resolvedImg.SizeGB > 0 {
		diskMB = int(resolvedImg.SizeGB * 1024)
	}

	spec := model.SandboxSpec{
		ID:        instanceID,
		Type:      req.Image,
		CPUs:      cpu,
		MemoryMB:  mem,
		DiskMB:    diskMB,
		IPAddress: ip,
	}

	// Rollback function for cleanup on failure
	cleanup := func() {
		fmt.Printf("   [!] Rollback: Deleting failed instance %s\n", spec.ID)
		os.RemoveAll(runtime.GetInstanceDir(spec.ID))
	}

	if err := runtime.ConfigureNetwork(*s.cfg, &spec); err != nil {
		fmt.Printf("❌ CRITICAL BOOT ERROR ConfigureNetwork: %v\n", err)
		cleanup()
		return nil, fmt.Errorf("boot failed: %w", err)
	}

	// Prepare storage (pass config by value, not pointer)
	overlay, err := runtime.PrepareInstance(ctx, *s.cfg, spec)
	if err != nil {
		return nil, fmt.Errorf("storage init failed: %w", err)
	}

	if err := runtime.Create(*s.cfg, spec, overlay); err != nil {
		fmt.Printf("❌ CRITICAL BOOT ERROR: %v\n", err)
		cleanup()
		return nil, fmt.Errorf("boot failed: %w", err)
	}

	netCfg := buildAgentNetConfig(s.cfg, spec.IPAddress, req.Name)
	timeout := time.Duration(s.cfg.Sandbox.SyncTimeoutSec) * time.Second
	syncEnabled := true
	if req.Sync != nil {
		syncEnabled = *req.Sync
	}
	if syncEnabled {
		if err := waitForAgent(ctx, spec.ID, timeout); err != nil {
			runtime.Stop(spec.ID)
			cleanup()
			return nil, fmt.Errorf("agent not ready: %w", err)
		}
	}

	// Set environment variables on the agent if provided
	if len(req.EnvVars) > 0 {
		go func() {
			log.Printf("   [Agent] Setting environment variables on %s (async)...\n", spec.ID)
			if err := setAgentEnvVars(spec.ID, req.EnvVars); err != nil {
				fmt.Printf("[WARN] Failed to set env vars on agent: %v\n", err)
				// Don't fail the creation, just log the warning
			}
		}()
	}

	go func() {
		log.Printf("   [Agent] Configuring network on %s (async)...\n", spec.ID)
		if cfgErr := configureAgentNetwork(spec.ID, &netCfg); cfgErr != nil {
			log.Printf("   [Agent] network config failed on %s: %v\n", spec.ID, cfgErr)
		} else {
			log.Printf("   [Agent] network config done on %s\n", spec.ID)
		}
	}()

	autoSleep := true
	if req.AutoSleep != nil {
		autoSleep = *req.AutoSleep
	}

	now := time.Now()
	sandbox := &model.Sandbox{
		ID:             objID,
		Name:           req.Name,
		Image:          req.Image,
		IP:             ip,
		CPU:            cpu,
		Mem:            mem,
		DiskMB:         diskMB,
		OrgID:          req.OrgID,
		EnvVars:        req.EnvVars,
		AutoSleep:      autoSleep,
		Region:         req.Region,
		RefID:          req.RefID,
		LastActivityAt: &now,
		Status:         "running",
		CreatedAt:      now,
		CreatedBy:      req.UserID,
	}

	log.Printf("   [SandboxService] Created sandbox %s with IP %s\n", sandbox.ID.Hex(), sandbox.OrgID.Hex())
	err = s.repo.Create(ctx, sandbox)
	if err != nil {
		runtime.Stop(spec.ID)
		cleanup()
		return nil, fmt.Errorf("DB save failed: %w", err)
	}

	if s.metrics != nil {
		s.metrics.RegisterSandbox(spec.ID, sandbox.Name, runtime.GetSocketPath(spec.ID), cpu, mem, diskMB)
	}

	// Start CLH event monitor
	if s.monitor != nil {
		s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
	}

	return sandbox, nil
}

func (s *SandboxService) Delete(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	s.repo.FreeIP(ctx, sandbox.IP)

	ok, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "deleted")
	if err != nil {
		return err
	}
	if !ok {
		return ErrSandboxNotFound
	}

	if s.metrics != nil {
		s.metrics.UnregisterSandbox(id)
	}

	if err := runtime.Delete(id); err != nil {
		fmt.Printf("[WARN] Failed to delete sandbox %s: %v\n", id, err)
	}

	// Stop event monitor (performs one final sync)
	if s.monitor != nil {
		s.monitor.Stop(ctx, id)
	}

	// Physical file cleanup after monitor has synced
	if err := runtime.Cleanup(id); err != nil {
		fmt.Printf("[WARN] Failed to cleanup files for %s: %v\n", id, err)
	}

	return nil
}

func (s *SandboxService) Start(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	// Verify it's stopped
	if sandbox.Status != "stopped" {
		return fmt.Errorf("sandbox is not stopped (current status: %s)", sandbox.Status)
	}

	socketPath := runtime.GetSocketPath(id)

	// Check if hypervisor is running (socket exists)
	client := runtime.NewCLHClient(socketPath)
	if client.IsSocketAvailable() {
		// Warm start - hypervisor running, just boot the VM
		log.Printf("[Start] Warm start for sandbox %s\n", id)
		if err := runtime.Start(id); err != nil {
			return fmt.Errorf("failed to start VM: %w", err)
		}

		timeout := 30 * time.Second
		if err := waitForAgent(ctx, id, timeout); err != nil {

			return fmt.Errorf("agent not ready: %w", err)
		}
	} else {
		// Cold start - hypervisor not running, need to recreate
		log.Printf("[Start] Cold start for sandbox %s - recreating VM\n", id)

		// Build spec from DB data
		spec := model.SandboxSpec{
			ID:        id,
			CPUs:      sandbox.CPU,
			MemoryMB:  sandbox.Mem,
			DiskMB:    sandbox.DiskMB,
			IPAddress: sandbox.IP,
		}

		// Get existing overlay path
		overlayPath := runtime.GetOverlayPath(id)

		// Recreate the VM (boots it automatically)
		if err := runtime.Create(*s.cfg, spec, overlayPath); err != nil {
			return fmt.Errorf("failed to recreate VM: %w", err)
		}

		// Wait for agent
		if err := waitForAgent(ctx, id, 30*time.Second); err != nil {
			return fmt.Errorf("agent not ready after restart: %w", err)
		}
	}

	// Update status to running and clear stoppedAt
	if _, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "running"); err != nil {
		// VM is running but DB update failed - log but don't fail
		fmt.Printf("[WARN] VM started but failed to update DB status: %v\n", err)
	}

	// Register with metrics
	if s.metrics != nil {
		spec := model.SandboxSpec{
			ID:       id,
			CPUs:     sandbox.CPU,
			MemoryMB: sandbox.Mem,
			DiskMB:   sandbox.DiskMB,
		}
		s.metrics.RegisterSandbox(spec.ID, sandbox.Name, runtime.GetSocketPath(spec.ID), spec.CPUs, spec.MemoryMB, spec.DiskMB)
	}

	return nil
}

func (s *SandboxService) Stop(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	if sandbox.Status != "running" {
		return fmt.Errorf("sandbox is not running (current status: %s)", sandbox.Status)
	}

	if err := runtime.Stop(id); err != nil {
		return err
	}
	if s.metrics != nil {
		s.metrics.UnregisterSandbox(sandbox.ID.Hex())
	}

	// Update database status to stopped and set stoppedAt
	if _, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "stopped"); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}
	// Also set stoppedAt timestamp for auto-delete tracking
	if err := s.repo.SetStoppedAt(ctx, sandbox.ID); err != nil {
		log.Printf("[WARN] Failed to set stoppedAt for %s: %v", id, err)
	}

	return nil
}

// EnsureRunning checks if sandbox is running and starts it if stopped (auto-start feature)
func (s *SandboxService) EnsureRunning(ctx context.Context, orgID primitive.ObjectID, id string) error {
	// Get sandbox from DB to check status
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	// If already running, return immediately
	if sandbox.Status == "running" {
		return nil
	}

	// If paused, resume it
	if sandbox.Status == "paused" {
		log.Printf("[Auto-Resume] Sandbox %s is paused, resuming...\n", id)
		if err := s.Resume(ctx, orgID, id); err != nil {
			return fmt.Errorf("failed to auto-resume sandbox: %w", err)
		}

		log.Printf("[Auto-Resume] Sandbox %s resumed and ready\n", id)
		return nil
	}

	// If stopped, start it
	if sandbox.Status == "stopped" {
		log.Printf("[Auto-Start] Sandbox %s is stopped, starting...\n", id)
		if err := s.Start(ctx, orgID, id); err != nil {
			return fmt.Errorf("failed to auto-start sandbox: %w", err)
		}

		log.Printf("[Auto-Start] Sandbox %s started and ready\n", id)
		return nil
	}

	// Other states
	return fmt.Errorf("sandbox in unexpected state for auto-start/resume: %s", sandbox.Status)
}

func (s *SandboxService) Pause(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	if sandbox.Status != "running" {
		return fmt.Errorf("sandbox is not running (current status: %s)", sandbox.Status)
	}

	if !sandbox.AutoSleep {
		return fmt.Errorf("sandbox has auto-sleep disabled")
	}

	if err := runtime.Pause(id); err != nil {
		return err
	}

	// Update database status to paused and set pausedAt
	if _, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "paused"); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}
	if err := s.repo.SetPausedAt(ctx, sandbox.ID); err != nil {
		log.Printf("[WARN] Failed to set pausedAt for %s: %v", id, err)
	}

	return nil
}

func (s *SandboxService) Resume(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	if sandbox.Status != "paused" {
		return fmt.Errorf("sandbox is not paused (current status: %s)", sandbox.Status)
	}

	if err := runtime.Resume(id); err != nil {
		log.Printf("[ERROR] Failed to resume sandbox %s: %v\n", id, err)
		return err
	}

	// Update database status to running
	if _, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "running"); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Touch activity on resume so the sandbox doesn't immediately get auto-paused again
	if err := s.repo.TouchActivity(ctx, sandbox.ID); err != nil {
		log.Printf("[WARN] Failed to touch activity on resume for %s: %v", id, err)
	}

	return nil
}

func (s *SandboxService) Info(id string) (string, error) {
	return runtime.Info(id)
}

// RefreshStatuses checks each sandbox health and updates status field in DB.
// Status values: running, paused, stopped.
func (s *SandboxService) RefreshStatuses(ctx context.Context) error {
	// Optimization 1: Fetch only necessary fields
	projection := bson.M{"_id": 1, "status": 1}
	sandboxes, err := s.repo.FindForHealth(ctx, options.FindOptions{Projection: projection})

	if err != nil {
		return fmt.Errorf("failed to list sandboxes: %w", err)
	}

	maxConc := s.cfg.Health.Concurrency
	if maxConc <= 0 {
		maxConc = 20
	}
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup

	for _, sb := range sandboxes {
		sb := sb
		id := sb.ID.Hex()

		// --- FAST PATH CHECKS ---
		client := runtime.NewAPIClientForSandbox(id)
		socketExists := client.IsSocketAvailable() // Fast os.Stat check

		// Case 1: DB says Stopped + Socket is GONE.
		// Conclusion: It is definitely stopped/dead. No need to call API.
		if sb.Status == "stopped" && !socketExists {
			continue
		}

		// Case 2: DB says Running + Socket is GONE.
		// Conclusion: It crashed. We must update DB to stopped. (Proceeds to update logic)

		// Case 3: Socket Exists (Your specific scenario).
		// Conclusion: It could be Running, Paused, or Loaded (Stopped).
		// We MUST call the API to find out.

		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer func() { <-sem; wg.Done() }()

			newState := "stopped"

			if socketExists {
				apiCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()

				sbxState, err := client.GetStateWithContext(apiCtx)
				if err == nil {
					// Map Cloud Hypervisor States to your App States
					switch strings.ToLower(sbxState) {
					case "running", "runningvirtualized":
						newState = "running"
					case "paused":
						newState = "paused"
					case "loaded":
						// 'Loaded' means Process active, but Guest not booted.
						// For your app, this is "stopped" (ready to start).
						newState = "stopped"
					default:
						newState = "stopped"
					}
				} else {
					// Socket exists, but API refused connection or timed out.
					// Process is likely zombie or unresponsive. Treat as stopped.
					fmt.Printf("[health] Sandbox %s unresponsive (socket exists): %v\n", id, err)
					newState = "killed"
				}
			}

			// Only write to DB if state actually changed
			if sb.Status != newState {
				if err := s.repo.UpdateStatusForHealth(ctx, sb.ID, newState); err != nil {
					fmt.Printf("[health] failed to update status for %s: %v\n", id, err)
				}
			}
		}()
	}

	wg.Wait()
	return nil
}

type agentNetConfig struct {
	IP          string   `json:"ip"`
	Netmask     string   `json:"netmask"`
	Gateway     string   `json:"gateway"`
	Nameservers []string `json:"nameservers"`
	Hostname    string   `json:"hostname"`
}

func waitForAgent(ctx context.Context, sbxID string, timeout time.Duration) error {
	defer util.Track("Agent Readiness Wait")()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	start := time.Now()
	attempts := 0
	var lastErr error

	for {
		err := runtime.Probe(sbxID, 1024, 50*time.Millisecond)
		attempts++
		if err == nil {
			log.Printf("   [Agent] Ready on %s after %s (%d attempts)\n", sbxID, time.Since(start), attempts)
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("agent readiness timeout after %s (%d attempts): last error: %v",
				time.Since(start), attempts, lastErr)
		case <-ticker.C:
			// next attempt
		}
	}
}

func configureAgentNetwork(sbxID string, netCfg *agentNetConfig) error {
	if netCfg == nil {
		return nil
	}

	jsonData, err := json.Marshal(netCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal network config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := AgentCommand(ctx, nil, sbxID, bytes.NewReader(jsonData), "/configure-network", http.MethodPost)
	if err != nil {
		return fmt.Errorf("configure network failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("configure network status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

func buildAgentNetConfig(cfg *config.Config, ip, name string) agentNetConfig {
	hostname := name
	if hostname == "" {
		hostname = cfg.Sandbox.DefaultHostname
	}
	return agentNetConfig{
		IP:          ip,
		Netmask:     cfg.Network.GetNetmask(),
		Gateway:     cfg.Network.GetCleanGateway(),
		Nameservers: cfg.Network.Nameservers,
		Hostname:    hostname,
	}
}

// Large files are streamed in binary mode to avoid base64 overhead
// func (s *SandboxService) UploadFile(ctx context.Context, sandboxID, filename, targetPath string, fileSize int64, fileContent io.Reader) error {
// 	// Get sandbox to verify it exists
// 	sandbox, exists := s.Get(ctx, sandboxID)
// 	if !exists {
// 		return fmt.Errorf("sandbox not found: %s", sandboxID)
// 	}

// 	// Normalize target path
// 	if !strings.HasPrefix(targetPath, "/") {
// 		targetPath = "/" + targetPath
// 	}

// 	fullPath := filepath.Join(targetPath, filename)

// 	// Use the file service to write the file via agent
// 	socketPath := filepath.Join(s.cfg.Paths.InstancesDir, sandbox.ID.Hex(), "vsock.sock")
// 	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
// 	if err != nil {
// 		return fmt.Errorf("Sandbox not reachable: %w", err)
// 	}
// 	defer conn.Close()

// 	// Handshake
// 	conn.SetDeadline(time.Now().Add(2 * time.Second))
// 	if _, err := conn.Write([]byte("CONNECT 1024\n")); err != nil {
// 		return fmt.Errorf("connection failed: %w", err)
// 	}

// 	buf := make([]byte, 32)
// 	n, err := conn.Read(buf)
// 	if err != nil {
// 		return fmt.Errorf("handshake failed: %w", err)
// 	}

// 	if !strings.HasPrefix(string(buf[:n]), "OK") {
// 		return fmt.Errorf("Sandbox agent not ready: %s", string(buf[:n]))
// 	}

// 	// Send file_write request using binary streaming (no base64)
// 	conn.SetDeadline(time.Now().Add(5 * time.Minute))
// 	req := map[string]interface{}{
// 		"action":     "file_write",
// 		"path":       fullPath,
// 		"binaryMode": true,
// 		"size":       fileSize,
// 	}
// 	if err := json.NewEncoder(conn).Encode(req); err != nil {
// 		return fmt.Errorf("failed to send request: %w", err)
// 	}

// 	// Stream the file bytes directly to the agent
// 	if fileSize > 0 {
// 		written, err := io.CopyN(conn, fileContent, fileSize)
// 		if err != nil {
// 			return fmt.Errorf("failed to stream file: %w", err)
// 		}
// 		if written != fileSize {
// 			return fmt.Errorf("short write: wrote %d of %d", written, fileSize)
// 		}
// 	} else {
// 		// Unknown size: fallback to full copy (still binary)
// 		if _, err := io.Copy(conn, fileContent); err != nil {
// 			return fmt.Errorf("failed to stream file: %w", err)
// 		}
// 	}

// 	// Read response
// 	type FileResponse struct {
// 		Success bool   `json:"success"`
// 		Error   string `json:"error,omitempty"`
// 	}
// 	var resp FileResponse
// 	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
// 		return fmt.Errorf("failed to read response: %w", err)
// 	}

// 	if !resp.Success {
// 		return fmt.Errorf("%s", resp.Error)
// 	}

// 	fmt.Printf("✓ File uploaded to Sandbox: %s -> %s (%d bytes)\n", filename, fullPath, fileSize)
// 	return nil
// }

// func (s *SandboxService) executeCommandInSandbox(sbxID, cmd string) error {
// 	socketPath := filepath.Join(s.cfg.Paths.InstancesDir, sbxID, "vsock.sock")

// 	// Connect to Sandbox socket with timeout
// 	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
// 	if err != nil {
// 		return fmt.Errorf("Sandbox not reachable: %w", err)
// 	}
// 	defer conn.Close()

// 	conn.SetDeadline(time.Now().Add(2 * time.Second))
// 	if _, err := conn.Write([]byte("CONNECT 1024\n")); err != nil {
// 		return fmt.Errorf("handshake failed: %w", err)
// 	}

// 	// Read handshake response
// 	buf := make([]byte, 32)
// 	n, err := conn.Read(buf)
// 	if err != nil {
// 		return fmt.Errorf("failed to read handshake: %w", err)
// 	}

// 	resp := string(buf[:n])
// 	if !strings.HasPrefix(resp, "OK") {
// 		return fmt.Errorf("Sandbox agent not ready: %s", resp)
// 	}

// 	// Send command to Sandbox agent
// 	conn.SetDeadline(time.Now().Add(10 * time.Second))

// 	agentReq := map[string]interface{}{
// 		"cmd":     cmd,
// 		"args":    []string{},
// 		"timeout": 30,
// 	}

// 	if err := json.NewEncoder(conn).Encode(agentReq); err != nil {
// 		return fmt.Errorf("failed to send command: %w", err)
// 	}

// 	// Read response to verify success
// 	respBuf := make([]byte, 1024)
// 	n, err = conn.Read(respBuf)
// 	if err != nil && err != io.EOF {
// 		return fmt.Errorf("failed to read response: %w", err)
// 	}

// 	respStr := string(respBuf[:n])
// 	if strings.Contains(respStr, "error") || strings.Contains(respStr, "failed") {
// 		return fmt.Errorf("Sandbox command failed: %s", respStr)
// 	}

// 	return nil
// }

// setAgentEnvVars sends environment variables to the agent for the sandbox
func setAgentEnvVars(sbxID string, envVars map[string]string) error {
	if len(envVars) == 0 {
		return nil
	}

	jsonData, err := json.Marshal(envVars)
	if err != nil {
		return fmt.Errorf("failed to marshal env vars: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := AgentCommand(ctx, nil, sbxID, bytes.NewReader(jsonData), "/env", http.MethodPost)
	if err != nil {
		return fmt.Errorf("failed to call agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned status %d: %s", resp.StatusCode, string(body))
	}

	fmt.Printf("[INFO] Environment variables set on sandbox %s: %v\n", sbxID, envVars)
	return nil
}

func (s *SandboxService) getOrgScopedSandbox(ctx context.Context, orgID primitive.ObjectID, id string) (*model.Sandbox, error) {
	objID, err := util.ParseObjectID(id)
	if err != nil {
		return nil, err
	}
	sandbox, err := s.repo.FindByIDAndOrg(ctx, orgID, objID, options.FindOneOptions{Projection: s.projection})
	if err != nil {
		return nil, err
	}
	if sandbox == nil {
		return nil, ErrSandboxNotFound
	}
	return sandbox, nil
}

// TouchActivity updates the lastActivityAt timestamp for a sandbox (called by handlers on API access).
func (s *SandboxService) TouchActivity(ctx context.Context, orgID primitive.ObjectID, id string) {
	objID, err := util.ParseObjectID(id)
	if err != nil {
		return
	}
	_ = s.repo.TouchActivity(ctx, objID)
}
