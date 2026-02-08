package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/model"
	"voidrun/internal/repository"
	"voidrun/pkg/machine"
	"voidrun/pkg/storage"
	"voidrun/pkg/timer"
	"voidrun/pkg/util"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SandboxService handles sandbox business logic
type SandboxService struct {
	repo      repository.ISandboxRepository
	imageRepo repository.IImageRepository
	cfg       *config.Config
}

// NewSandboxService creates a new sandbox service
func NewSandboxService(cfg *config.Config, repo repository.ISandboxRepository, imageRepo repository.IImageRepository) *SandboxService {
	return &SandboxService{
		repo:      repo,
		imageRepo: imageRepo,
		cfg:       cfg,
	}
}

// List returns all VMs
func (s *SandboxService) List(ctx context.Context) ([]*model.Sandbox, error) {
	return s.repo.Find(ctx, nil, options.FindOptions{})
}

// ListByOrgPaginated returns paginated VMs for a specific org and the actual page size used
func (s *SandboxService) ListByOrgPaginated(ctx context.Context, orgIDHex string, page, pageSize int) ([]*model.Sandbox, int64, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = config.DefaultPageSize
	} else if pageSize > config.MaxPageSize {
		pageSize = config.MaxPageSize
	}

	orgID, err := util.ParseObjectID(orgIDHex)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("invalid org id: %w", err)
	}

	filter := bson.M{"orgId": orgID}

	// Get total count
	total, err := s.repo.Count(ctx, filter)
	if err != nil {
		return nil, 0, 0, err
	}

	// Use projection to fetch only essential fields for list view
	skip := int64((page - 1) * pageSize)
	opts := options.FindOptions{}
	opts.SetSkip(skip)
	opts.SetLimit(int64(pageSize))
	opts.SetProjection(bson.M{
		"_id":       1,
		"name":      1,
		"imageId":   1,
		"ip":        1,
		"cpu":       1,
		"mem":       1,
		"status":    1,
		"createdAt": 1,
	})
	vms, err := s.repo.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, 0, err
	}

	if vms == nil {
		vms = []*model.Sandbox{}
	}
	return vms, total, pageSize, nil
}

// ListByOrg returns VMs for a specific org
func (s *SandboxService) ListByOrg(ctx context.Context, orgIDHex string) ([]*model.Sandbox, error) {
	orgID, err := util.ParseObjectID(orgIDHex)
	if err != nil {
		return nil, fmt.Errorf("invalid org id: %w", err)
	}

	filter := bson.M{"orgId": orgID}
	// Use projection to fetch only essential fields for list view
	opts := options.FindOptions{}
	opts.SetProjection(bson.M{
		"_id":       1,
		"name":      1,
		"imageId":   1,
		"ip":        1,
		"cpu":       1,
		"mem":       1,
		"status":    1,
		"createdAt": 1,
	})
	vms, err := s.repo.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}

	if vms == nil {
		vms = []*model.Sandbox{}
	}
	return vms, nil
}

// Get returns a VM by ID
func (s *SandboxService) Get(ctx context.Context, id string) (*model.Sandbox, bool) {
	sandbox, err := s.repo.FindByID(ctx, id)
	if err != nil || sandbox == nil {
		return nil, false
	}
	return sandbox, true
}

// Exists checks if a VM exists
func (s *SandboxService) Exists(ctx context.Context, id string) bool {
	return s.repo.Exists(ctx, id)
}

// Create creates a new VM
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

	spec := model.SandboxSpec{
		ID:        instanceID,
		Type:      req.TemplateID,
		CPUs:      cpu,
		MemoryMB:  mem,
		DiskMB:    s.cfg.Sandbox.DefaultDiskMB,
		IPAddress: ip,
	}

	// Prepare storage (pass config by value, not pointer)
	overlay, err := storage.PrepareInstance(*s.cfg, spec)
	if err != nil {
		return nil, fmt.Errorf("storage init failed: %w", err)
	}

	// Rollback function for cleanup on failure
	cleanup := func() {
		fmt.Printf("   [!] Rollback: Deleting failed instance %s\n", spec.ID)
		os.RemoveAll(filepath.Dir(overlay))
	}

	// Start VM
	if err := machine.Start(*s.cfg, spec, overlay, ""); err != nil {
		fmt.Printf("❌ CRITICAL BOOT ERROR: %v\n", err)
		cleanup()
		return nil, fmt.Errorf("boot failed: %w", err)
	}

	// Optional synchronous readiness gate: ensure the agent is reachable before returning.
	syncEnabled := true
	if req.Sync != nil {
		syncEnabled = *req.Sync
	}
	if syncEnabled {
		if err := waitForAgent(spec.ID, 2*time.Second); err != nil {
			machine.Stop(spec.ID)
			cleanup()
			return nil, fmt.Errorf("agent not ready: %w", err)
		}
	}

	// Save to DB as pointer with OrgID and CreatedBy
	orID, _ := util.ParseObjectID(req.OrgID)

	// Set environment variables on the agent if provided
	if len(req.EnvVars) > 0 {
		if err := setAgentEnvVars(spec.ID, req.EnvVars); err != nil {
			fmt.Printf("[WARN] Failed to set env vars on agent: %v\n", err)
			// Don't fail the creation, just log the warning
		}
	}

	sandbox := &model.Sandbox{
		ID:        objID,
		Name:      req.Name, // Store the VM instance name
		ImageId:   req.TemplateID,
		IP:        ip,
		CPU:       cpu,
		Mem:       mem,
		OrgID:     orID,
		EnvVars:   req.EnvVars, // Store env vars in the sandbox record
		Status:    "running",
		CreatedAt: time.Now(),
	}
	err = s.repo.Create(ctx, sandbox)
	if err != nil {
		machine.Stop(spec.ID)
		cleanup()
		return nil, fmt.Errorf("DB save failed: %w", err)
	}

	return sandbox, nil
}

// Restore restores a VM from a snapshot
func (s *SandboxService) Restore(ctx context.Context, req model.RestoreSandboxRequest) (string, error) {
	// Auto-assign IP if not provided
	ip := req.NewIP
	if ip == "" {
		var err error
		ip, err = s.repo.NextAvailableIP()
		if err != nil {
			return "", fmt.Errorf("IP allocation failed: %w", err)
		}
	}

	// Generate ObjectID for filesystem-safe directory name
	objID := util.GenerateObjectID()
	instanceID := objID.Hex()

	// Apply defaults
	cpu := req.CPU
	if cpu == 0 {
		cpu = 1
	}
	mem := req.Mem
	if mem == 0 {
		mem = 1024
	}

	// Perform restore
	if err := machine.Restore(*s.cfg, instanceID, req.SnapshotPath, ip, req.Cold); err != nil {
		return "", fmt.Errorf("restore failed: %w", err)
	}

	// Save to DB using Create with context
	orID, _ := util.ParseObjectID(req.OrgID)
	var createdBy primitive.ObjectID
	if req.UserID != "" {
		createdBy, _ = util.ParseObjectID(req.UserID)
	}
	sandbox := &model.Sandbox{
		ID:        objID,
		Name:      req.NewID, // Store the user-provided name
		ImageId:   "snapshot",
		IP:        ip,
		CPU:       cpu,
		Mem:       mem,
		OrgID:     orID,
		CreatedBy: createdBy,
		Status:    "running",
		CreatedAt: time.Now(),
	}
	err := s.repo.Create(ctx, sandbox)
	if err != nil {
		return "", fmt.Errorf("failed to save restored sandbox: %w", err)
	}

	return ip, nil
}

// Delete stops and removes a VM
func (s *SandboxService) Delete(ctx context.Context, id string) error {
	// Delete the VM using its ObjectID (which is the directory name)
	if err := machine.Delete(id); err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}

	// Delete from database using ObjectID
	objID, err := util.ParseObjectID(id)
	if err != nil {
		return fmt.Errorf("invalid ID format: %w", err)
	}
	return s.repo.Delete(ctx, objID)
}

// Stop stops a VM
func (s *SandboxService) Stop(id string) error {
	return machine.Stop(id)
}

// Pause pauses a VM
func (s *SandboxService) Pause(id string) error {
	return machine.Pause(id)
}

// Resume resumes a VM
func (s *SandboxService) Resume(id string) error {
	return machine.Resume(id)
}

// Info returns VM info
func (s *SandboxService) Info(id string) (string, error) {
	return machine.Info(id)
}

// CreateSnapshot creates a snapshot of a VM
func (s *SandboxService) CreateSnapshot(id string) error {
	return machine.CreateSnapshot(id)
}

// ListSnapshots lists all snapshots for a VM
func (s *SandboxService) ListSnapshots(id string) ([]model.Snapshot, error) {
	basePath := filepath.Join(s.cfg.Paths.InstancesDir, id, "snapshots")

	files, err := os.ReadDir(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []model.Snapshot{}, nil
		}
		return nil, fmt.Errorf("failed to scan snapshots: %w", err)
	}

	var snaps []model.Snapshot
	for _, f := range files {
		if f.IsDir() {
			snaps = append(snaps, model.Snapshot{
				ID:        f.Name(),
				CreatedAt: f.Name(),
				FullPath:  filepath.Join(basePath, f.Name()),
			})
		}
	}
	return snaps, nil
}

// RefreshStatuses checks each sandbox health and updates status field in DB.
// Status values: running, paused, stopped.
func (s *SandboxService) RefreshStatuses(ctx context.Context) error {
	// Optimization 1: Fetch only necessary fields
	projection := bson.M{"_id": 1, "status": 1}
	sandboxes, err := s.repo.Find(ctx, bson.M{}, options.FindOptions{Projection: projection})
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
		client := machine.NewAPIClientForVM(id)
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
				// Use timeout to prevent hanging on stuck VMMs
				apiCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()

				vmState, err := client.GetStateWithContext(apiCtx)
				if err == nil {
					// Map Cloud Hypervisor States to your App States
					switch strings.ToLower(vmState) {
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
					fmt.Printf("[health] VM %s unresponsive (socket exists): %v\n", id, err)
					newState = "stopped"
				}
			}

			// Only write to DB if state actually changed
			if sb.Status != newState {
				if err := s.repo.UpdateStatus(ctx, sb.ID, newState); err != nil {
					fmt.Printf("[health] failed to update status for %s: %v\n", id, err)
				}
			}
		}()
	}

	wg.Wait()
	return nil
}

// GetSnapshotsBasePath returns the base path for snapshots
func (s *SandboxService) GetSnapshotsBasePath(id string) string {
	pwd, _ := filepath.Abs(".")
	return filepath.Join(pwd, "instances", id, "snapshots")
}

func waitForAgent(vmID string, timeout time.Duration) error {
	defer timer.Track("Agent Readiness Wait")()
	deadline := time.Now().Add(timeout)
	sleep := 50 * time.Millisecond

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("agent readiness timeout after %v", timeout)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		resp, err := AgentCommand(ctx, nil, vmID, nil, "", http.MethodGet)
		cancel()

		if err == nil {
			resp.Body.Close()
			log.Printf("   [Agent] Ready on %s\n", vmID)
			return nil
		}

		// log.Printf("   [Agent] VSOCK dial %s: err=%v\n", vmID, err)
		time.Sleep(sleep)
	}
}

// UploadFile uploads a file directly into a running sandbox VM
// Large files are streamed in binary mode to avoid base64 overhead
func (s *SandboxService) UploadFile(ctx context.Context, sandboxID, filename, targetPath string, fileSize int64, fileContent io.Reader) error {
	// Get sandbox to verify it exists
	sandbox, exists := s.Get(ctx, sandboxID)
	if !exists {
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	// Normalize target path
	if !strings.HasPrefix(targetPath, "/") {
		targetPath = "/" + targetPath
	}

	// Create destination path inside VM
	fullPath := filepath.Join(targetPath, filename)

	// Use the file service to write the file via agent
	socketPath := filepath.Join(s.cfg.Paths.InstancesDir, sandbox.ID.Hex(), "vsock.sock")
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("VM not reachable: %w", err)
	}
	defer conn.Close()

	// Handshake
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("CONNECT 1024\n")); err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}

	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}

	if !strings.HasPrefix(string(buf[:n]), "OK") {
		return fmt.Errorf("VM agent not ready: %s", string(buf[:n]))
	}

	// Send file_write request using binary streaming (no base64)
	conn.SetDeadline(time.Now().Add(5 * time.Minute))
	req := map[string]interface{}{
		"action":     "file_write",
		"path":       fullPath,
		"binaryMode": true,
		"size":       fileSize,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	// Stream the file bytes directly to the agent
	if fileSize > 0 {
		written, err := io.CopyN(conn, fileContent, fileSize)
		if err != nil {
			return fmt.Errorf("failed to stream file: %w", err)
		}
		if written != fileSize {
			return fmt.Errorf("short write: wrote %d of %d", written, fileSize)
		}
	} else {
		// Unknown size: fallback to full copy (still binary)
		if _, err := io.Copy(conn, fileContent); err != nil {
			return fmt.Errorf("failed to stream file: %w", err)
		}
	}

	// Read response
	type FileResponse struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	var resp FileResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("%s", resp.Error)
	}

	fmt.Printf("✓ File uploaded to VM: %s -> %s (%d bytes)\n", filename, fullPath, fileSize)
	return nil
}

// executeCommandInVM executes a command inside a running VM via its agent socket
func (s *SandboxService) executeCommandInVM(vmID, cmd string) error {
	socketPath := filepath.Join(s.cfg.Paths.InstancesDir, vmID, "vsock.sock")

	// Connect to VM socket with timeout
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return fmt.Errorf("VM not reachable: %w", err)
	}
	defer conn.Close()

	// Handshake with VM agent
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("CONNECT 1024\n")); err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}

	// Read handshake response
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("failed to read handshake: %w", err)
	}

	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("VM agent not ready: %s", resp)
	}

	// Send command to VM agent
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	agentReq := map[string]interface{}{
		"cmd":     cmd,
		"args":    []string{},
		"timeout": 30,
	}

	if err := json.NewEncoder(conn).Encode(agentReq); err != nil {
		return fmt.Errorf("failed to send command: %w", err)
	}

	// Read response to verify success
	respBuf := make([]byte, 1024)
	n, err = conn.Read(respBuf)
	if err != nil && err != io.EOF {
		return fmt.Errorf("failed to read response: %w", err)
	}

	respStr := string(respBuf[:n])
	if strings.Contains(respStr, "error") || strings.Contains(respStr, "failed") {
		return fmt.Errorf("VM command failed: %s", respStr)
	}

	return nil
}

// setAgentEnvVars sends environment variables to the agent for the sandbox
func setAgentEnvVars(vmID string, envVars map[string]string) error {
	if len(envVars) == 0 {
		return nil
	}

	// Marshal env vars to JSON
	jsonData, err := json.Marshal(envVars)
	if err != nil {
		return fmt.Errorf("failed to marshal env vars: %w", err)
	}

	// Make HTTP request to agent's /env endpoint
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Call agent's /env endpoint
	resp, err := AgentCommand(ctx, nil, vmID, bytes.NewReader(jsonData), "/env", http.MethodPost)
	if err != nil {
		return fmt.Errorf("failed to call agent /env endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent returned status %d: %s", resp.StatusCode, string(body))
	}

	fmt.Printf("[INFO] Environment variables set on sandbox %s: %v\n", vmID, envVars)
	return nil
}
