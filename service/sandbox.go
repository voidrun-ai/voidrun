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
	"golang.org/x/sync/singleflight"
)

var ErrSandboxNotFound = errors.New("sandbox not found")

// SandboxService handles sandbox business logic
type SandboxService struct {
	repo         repository.ISandboxRepository
	imageRepo    repository.IImageRepository
	cfg          *config.Config
	metrics      *metrics.Manager
	monitor      *runtime.EventMonitor
	projection   primitive.M
	restoreGroup singleflight.Group // deduplicates concurrent auto-restore calls per sandbox
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
			"ip":             1,
			"cpu":            1,
			"mem":            1,
			"diskMB":         1,
			"status":         1,
			"autoSleep":      1,
			"lastActivityAt": 1,
			"snapshottedAt":  1,
			"createdAt":      1,
			"orgId":          1,
			"createdBy":      1,
			"region":         1,
			"refId":          1,
			"tapName":        1,
			"tapDeleted":     1,
			"netnsName":      1,
			"macAddress":     1,
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

	imageName := req.Image
	if !strings.Contains(imageName, ":") {
		img, err := s.imageRepo.GetLatestByNameForOrg(imageName, req.OrgID)
		if err == nil && img != nil && img.Tag != "" {
			imageName = fmt.Sprintf("%s:%s", img.Name, img.Tag)
		}
	}

	spec := model.SandboxSpec{
		ID:        instanceID,
		Type:      imageName,
		CPUs:      cpu,
		MemoryMB:  mem,
		DiskMB:    diskMB,
		IPAddress: ip,
	}

	// Rollback function for cleanup on failure
	cleanup := func() {
		fmt.Printf("   [!] Rollback: Deleting failed instance %s\n", spec.ID)
		// If NetNS was already created, tear it down atomically
		if spec.NetNSName != "" {
			_ = runtime.DeleteSandboxNetNS(spec.NetNSName)
		} else if spec.TapName != "" {
			_ = runtime.DeleteTap(spec.TapName)
		}
		os.RemoveAll(runtime.GetInstanceDir(spec.ID))
	}

	overlay, err := runtime.PrepareStorage(ctx, *s.cfg, spec)
	if err != nil {
		return nil, fmt.Errorf("storage init failed: %w", err)
	}

	if err := runtime.ConfigureNetwork(*s.cfg, &spec); err != nil {
		fmt.Printf("❌ CRITICAL BOOT ERROR ConfigureNetwork: %v\n", err)
		cleanup()
		return nil, fmt.Errorf("boot failed: %w", err)
	}

	if err := runtime.CreateCLI(*s.cfg, spec, overlay); err != nil {
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

	if syncEnabled {
		log.Printf("   [Agent] Configuring network on %s (sync)...\n", spec.ID)
		if cfgErr := configureAgentNetwork(spec.ID, &netCfg); cfgErr != nil {
			log.Printf("   [Agent] network config failed on %s: %v\n", spec.ID, cfgErr)
		} else {
			log.Printf("   [Agent] network config done on %s\n", spec.ID)
		}
	} else {
		go func() {
			log.Printf("   [Agent] Configuring network on %s (async)...\n", spec.ID)
			if cfgErr := configureAgentNetwork(spec.ID, &netCfg); cfgErr != nil {
				log.Printf("   [Agent] network config failed on %s: %v\n", spec.ID, cfgErr)
			} else {
				log.Printf("   [Agent] network config done on %s\n", spec.ID)
			}
		}()
	}

	autoSleep := true
	if req.AutoSleep != nil {
		autoSleep = *req.AutoSleep
	}

	now := time.Now()
	sandbox := &model.Sandbox{
		ID:             objID,
		Name:           req.Name,
		Image:          imageName,
		IP:             ip,
		CPU:            cpu,
		Mem:            mem,
		DiskMB:         diskMB,
		OrgID:          req.OrgID,
		EnvVars:        req.EnvVars,
		AutoSleep:      autoSleep,
		Region:         req.Region,
		RefID:          req.RefID,
		TapName:        spec.TapName,
		NetNSName:      spec.NetNSName,
		MacAddress:     spec.MacAddress, // persist so Restore doesn't need to re-derive it
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

	if err := runtime.Delete(id, sandbox.TapName, sandbox.NetNSName); err != nil {
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

func (s *SandboxService) Snapshot(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	if sandbox.Status != "running" {
		return fmt.Errorf("sandbox is not running (current status: %s)", sandbox.Status)
	}

	// Stop event monitor BEFORE snapshot so it can do a final sync while the CLH socket is alive.
	if s.monitor != nil {
		s.monitor.Stop(ctx, id)
	}

	if err := runtime.Snapshot(id); err != nil {
		return err
	}

	// Update database status to snapshotted and set snapshottedAt
	if _, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "snapshotted"); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}
	if err := s.repo.SetSnapshottedAt(ctx, sandbox.ID); err != nil {
		log.Printf("[WARN] Failed to set snapshottedAt for %s: %v", id, err)
	}

	if s.metrics != nil {
		s.metrics.UnregisterSandbox(sandbox.ID.Hex())
	}

	return nil
}

func (s *SandboxService) Restore(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	// Verify it's snapshotted
	if sandbox.Status != "snapshotted" {
		return fmt.Errorf("sandbox is not snapshotted (current status: %s)", sandbox.Status)
	}

	imageName := sandbox.Image
	if !strings.Contains(imageName, ":") {
		img, err := s.imageRepo.GetLatestByNameForOrg(imageName, orgID)
		if err == nil && img != nil && img.Tag != "" {
			imageName = fmt.Sprintf("%s:%s", img.Name, img.Tag)
		}
	}

	// Resolve MAC: prefer stored value, fall back to deterministic derivation for
	// sandboxes created before this field was added.
	macAddr := sandbox.MacAddress
	if macAddr == "" {
		macAddr = runtime.GenerateMAC(sandbox.IP)
	}

	spec := model.SandboxSpec{
		ID:         id,
		Type:       imageName,
		CPUs:       sandbox.CPU,
		MemoryMB:   sandbox.Mem,
		IPAddress:  sandbox.IP,
		TapName:    sandbox.TapName,
		MacAddress: macAddr,
		NetNSName:  sandbox.NetNSName,
	}

	var overlayPath string
	if s.cfg.Sandbox.DiskFormat == "raw" {
		overlayPath = runtime.GetRawOverlayPath(id)
	} else {
		overlayPath = runtime.GetOverlayPath(id)
	}
	snapshotDir := runtime.GetLatestSnapshotDir(id)
	if snapshotDir == "" {
		return fmt.Errorf("no valid snapshot found for sandbox %s", id)
	}

	if err := runtime.Restore(*s.cfg, spec, overlayPath, snapshotDir); err != nil {
		return fmt.Errorf("failed to restore VM: %w", err)
	}

	// From this point, the VMM is running. Any failure must clean it up.
	cleanup := func() {
		log.Printf("[Restore] Rolling back: stopping VM %s", id)
		if stopErr := runtime.Stop(id); stopErr != nil {
			log.Printf("[Restore] Rollback stop failed for %s: %v", id, stopErr)
		}
	}

	timeout := 30 * time.Second
	if err := waitForAgent(ctx, id, timeout); err != nil {
		cleanup()
		return fmt.Errorf("agent not ready after restore: %w", err)
	}

	// Sync the guest clock — after a snapshot restore the VM clock is frozen at
	// the time the snapshot was taken. Inject the current wall-clock time via the
	// agent so that `date`, cron jobs, TLS expiry checks, etc. see the right time.
	syncSandboxClock(id)

	// After a snapshot restore, the virtio-net device inside the guest comes back
	// with eth0 DOWN (cloud-hypervisor resets the virtio-net device on restore).
	// Re-apply the network config to bring eth0 up and restore IP/routes/DNS.
	netCfg := buildAgentNetConfig(s.cfg, sandbox.IP, sandbox.Name)
	if cfgErr := configureAgentNetwork(id, &netCfg); cfgErr != nil {
		log.Printf("   [Restore] network re-config failed on %s: %v\n", id, cfgErr)
	} else {
		log.Printf("   [Restore] network re-config done on %s\n", id)
	}

	// Update status to running
	if _, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "running"); err != nil {
		cleanup()
		return fmt.Errorf("VM restored but failed to update DB status: %w", err)
	}

	// Touch activity on restore so the sandbox doesn't immediately get auto-snapshotted again
	if err := s.repo.TouchActivity(ctx, sandbox.ID); err != nil {
		log.Printf("[WARN] Failed to touch activity on restore for %s: %v", id, err)
	}

	// Register with metrics
	if s.metrics != nil {
		s.metrics.RegisterSandbox(id, sandbox.Name, runtime.GetSocketPath(id), sandbox.CPU, sandbox.Mem, sandbox.DiskMB)
	}

	// Restart CLH event monitor so restored sandboxes get event tracking
	if s.monitor != nil {
		s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
	}

	return nil
}

// EnsureRunning checks if sandbox is running and restores it if snapshotted (auto-restore feature).
// Uses singleflight to deduplicate concurrent restore calls — if 100 exec requests arrive for the
// same snapshotted sandbox, only 1 will actually call Restore(); the other 99 block and share the result.
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

	// If snapshotted, restore it via singleflight to prevent thundering herd
	if sandbox.Status == "snapshotted" {
		_, err, shared := s.restoreGroup.Do(id, func() (interface{}, error) {
			log.Printf("[Auto-Restore] Sandbox %s is snapshotted, restoring...\n", id)
			if err := s.Restore(ctx, orgID, id); err != nil {
				return nil, fmt.Errorf("failed to auto-restore sandbox: %w", err)
			}
			log.Printf("[Auto-Restore] Sandbox %s restored and ready\n", id)
			return nil, nil
		})
		if shared {
			log.Printf("[Auto-Restore] Sandbox %s restore was shared with concurrent caller\n", id)
		}
		return err
	}

	// Other states
	return fmt.Errorf("sandbox in unexpected state for auto-restore: %s", sandbox.Status)
}

func (s *SandboxService) Info(id string) (string, error) {
	return runtime.Info(id)
}

// RefreshStatuses checks each sandbox health and updates status field in DB.
// Status values: running, snapshotted, killed, deleted.
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

		// Case 1: DB says Snapshotted.
		// Conclusion: It is either snapshotted (socket gone) or in the process of restoring (socket exists).
		// In either case, the health check should not touch its status.
		if sb.Status == "snapshotted" {
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

			newState := "killed"

			if socketExists {
				apiCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()

				sbxState, err := client.GetStateWithContext(apiCtx)
				if err == nil {
					// Map Cloud Hypervisor States to your App States
					switch strings.ToLower(sbxState) {
					case "running", "runningvirtualized":
						newState = "running"
					default:
						// If the socket is somehow still there but state is not running
						// it might be a zombie, so map it to killed.
						newState = "killed"
					}
				} else {
					// Socket exists, but API refused connection or timed out.
					// Process is likely zombie or unresponsive. Treat as killed.
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

	start := time.Now()
	attempts := 0
	var lastErr error

	// Tight 10ms polling interval with 15ms probe timeout.
	// The vsock needs ~350ms to synchronize after restore regardless of
	// how often we poll. Using 10ms interval ensures we catch the exact
	// moment it becomes ready (at most 25ms overshoot).
	const pollInterval = 10 * time.Millisecond
	const probeTimeout = 15 * time.Millisecond // CONNECT+OK takes <5ms once ready

	// Use a Ticker (not time.After) to avoid allocating a new timer object
	// every iteration — time.After leaks ~3000 timers over a 30s timeout.
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		err := runtime.Probe(sbxID, 1024, probeTimeout)
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
	io.Copy(io.Discard, resp.Body)

	return nil
}

// syncSandboxClock injects the current wall-clock time into a restored sandbox
// guest via `date -s @<unix_epoch>`.  After a VM snapshot/restore the guest
// clock is frozen at the snapshot timestamp; this call corrects it so the
// guest sees the real current time immediately after restore.
//
// The agent vsock health-check can pass a split-second before the /exec HTTP
// handler is fully initialised (EOF on handshake), so we retry a few times
// with a short back-off before giving up.
// This is best-effort: a failure is logged but never causes the restore to fail.
func syncSandboxClock(sbxID string) {
	now := time.Now().Unix()
	cmd := fmt.Sprintf("sudo date -s @%d", now)

	payload := map[string]interface{}{
		"cmd":     cmd,
		"timeout": 5,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[Restore] syncSandboxClock: marshal error for %s: %v", sbxID, err)
		return
	}

	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		resp, err := ExecAgentCommand(ctx, nil, sbxID, bytes.NewReader(body))
		cancel()

		if err != nil {
			log.Printf("[Restore] syncSandboxClock: attempt %d/%d exec error for %s: %v", attempt, maxAttempts, sbxID, err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("[Restore] syncSandboxClock: attempt %d/%d agent returned %d for %s", attempt, maxAttempts, resp.StatusCode, sbxID)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		log.Printf("   [Restore] clock synced to epoch %d on %s (attempt %d)", now, sbxID, attempt)
		return
	}
	log.Printf("[WARN] syncSandboxClock: gave up syncing clock for %s after %d attempts", sbxID, maxAttempts)
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
	io.Copy(io.Discard, resp.Body)

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
