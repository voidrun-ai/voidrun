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

var (
	ErrSandboxNotFound   = errors.New("sandbox not found")
	ErrSandboxNotRunning = errors.New("sandbox is not running")
)

func (s *SandboxService) recordLifecycleOp(sbxID, operation string, start time.Time, err error) {
	if s.metrics == nil || sbxID == "" {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	s.metrics.RecordSandboxOperation(sbxID, "lifecycle", operation, status, time.Since(start).Seconds())
}

// SandboxService handles sandbox business logic
type SandboxService struct {
	repo           repository.ISandboxRepository
	imageRepo      repository.IImageRepository
	cfg            *config.Config
	metrics        *metrics.Manager
	monitor        *runtime.EventMonitor
	projection     primitive.M
	lifecycleLocks *SandboxLifecycleLocks // health TryAcquire; actor already serializes API ops
	actors         *ActorRegistry
}

// NewSandboxService creates a new sandbox service. lifecycleLocks is held so
// RefreshStatuses can TryAcquire without spinning up an actor per sandbox.
func NewSandboxService(
	cfg *config.Config,
	repo repository.ISandboxRepository,
	imageRepo repository.IImageRepository,
	metricsManager *metrics.Manager,
	monitor *runtime.EventMonitor,
	lifecycleLocks *SandboxLifecycleLocks,
) *SandboxService {
	if lifecycleLocks == nil {
		lifecycleLocks = NewSandboxLifecycleLocks()
	}
	return &SandboxService{
		repo:           repo,
		imageRepo:      imageRepo,
		cfg:            cfg,
		metrics:        metricsManager,
		monitor:        monitor,
		lifecycleLocks: lifecycleLocks,
		actors:         NewActorRegistry(),
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
			"nodeId":         1,
			"tapName":        1,
			"tapDeleted":     1,
			"netnsName":      1,
			"macAddress":     1,
			"publishPorts":   1,
			"labels":         1,
		},
	}
}

// SetAdmission installs the optional lifecycle plugin. Only EE calls this.
func (s *SandboxService) SetAdmission(a Admission) {
	s.actors.SetAdmission(a)
}

// UpdatePublishPorts replaces the ports exposed through the public gateway.
func (s *SandboxService) UpdatePublishPorts(ctx context.Context, orgID primitive.ObjectID, id string, ports []int) (*model.Sandbox, error) {
	if err := util.ValidatePublishPorts(ports); err != nil {
		return nil, err
	}
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if sandbox.Status == "deleted" {
		return nil, ErrSandboxNotFound
	}
	ok, err := s.repo.UpdatePublishPortsByIDAndOrg(ctx, sandbox.ID, orgID, ports)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSandboxNotFound
	}
	sandbox.PublishPorts = ports
	return sandbox, nil
}

func (s *SandboxService) ListByOrgPaginated(ctx context.Context, orgID primitive.ObjectID, page, pageSize int, labels map[string]string) ([]*model.Sandbox, int64, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = config.DefaultPageSize
	} else if pageSize > config.MaxPageSize {
		pageSize = config.MaxPageSize
	}

	filter := sandboxListFilter(orgID, labels)

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
	timeout := time.Duration(s.cfg.Sandbox.SyncTimeoutSec) * time.Second
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

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
	if req.Image == "" {
		req.Image = s.cfg.Sandbox.DefaultImage
	}

	imageName := req.Image
	img, imgErr := s.imageRepo.ResolveImage(ctx, req.OrgID, imageName)
	if imgErr == nil && img != nil {
		if img.Tag != "" {
			imageName = fmt.Sprintf("%s:%s", img.Name, img.Tag)
		}
	} else if !strings.Contains(imageName, ":") {
		if latest, err := s.imageRepo.GetLatestByNameForOrg(ctx, imageName, req.OrgID); err == nil && latest != nil && latest.Tag != "" {
			img = latest
			imageName = fmt.Sprintf("%s:%s", latest.Name, latest.Tag)
		}
	}
	diskMB := diskMBForCreate(s.cfg.Sandbox.DefaultDiskMB, img)

	if err := s.actors.beforeCreate(ctx, cpu, mem, diskMB); err != nil {
		return nil, err
	}
	created := false
	defer func() {
		if !created {
			s.actors.afterCreateFailed(cpu, mem, diskMB)
		}
	}()

	spec := model.SandboxSpec{
		ID:        instanceID,
		Type:      imageName,
		CPUs:      cpu,
		MemoryMB:  mem,
		DiskMB:    diskMB,
		IPAddress: ip,
	}

	haveVM := false
	cleanup := func() {
		fmt.Printf("   [!] Rollback: Deleting failed instance %s\n", spec.ID)
		if haveVM {
			runtime.Stop(spec.ID)
			s.actors.Unregister(spec.ID)
		}
		if spec.NetNSName != "" {
			_ = runtime.DeleteSandboxNetNS(spec.NetNSName)
		} else if spec.TapName != "" {
			_ = runtime.DeleteTap(spec.TapName)
		}
		os.RemoveAll(runtime.GetInstanceDir(spec.ID))
		if ip != "" {
			s.repo.FreeIP(context.Background(), ip)
		}
	}
	defer func() {
		if !created {
			cleanup()
		}
	}()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	overlay, err := runtime.PrepareStorage(ctx, *s.cfg, spec)
	if err != nil {
		return nil, fmt.Errorf("storage init failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := runtime.ConfigureNetwork(*s.cfg, &spec); err != nil {
		fmt.Printf("❌ CRITICAL BOOT ERROR ConfigureNetwork: %v\n", err)
		return nil, fmt.Errorf("boot failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	proc, err := runtime.CreateCLI(*s.cfg, spec, overlay)
	if err != nil {
		fmt.Printf("❌ CRITICAL BOOT ERROR: %v\n", err)
		return nil, fmt.Errorf("boot failed: %w", err)
	}
	s.actors.GetOrCreate(spec.ID).Attach(proc)
	haveVM = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	netCfg := buildAgentNetConfig(s.cfg, spec.IPAddress, req.Name)
	syncEnabled := true
	if req.Sync != nil {
		syncEnabled = *req.Sync
	}
	if syncEnabled {
		if err := waitForAgent(ctx, spec.ID, timeout); err != nil {
			return nil, fmt.Errorf("agent not ready: %w", err)
		}
	}

	if syncEnabled && len(req.EnvVars) > 0 {
		go func() {
			log.Printf("   [Agent] Setting environment variables on %s (async)...\n", spec.ID)
			if err := setAgentEnvVars(spec.ID, req.EnvVars); err != nil {
				fmt.Printf("[WARN] Failed to set env vars on agent: %v\n", err)
			}
		}()
	}

	if syncEnabled {
		log.Printf("   [Agent] Configuring network on %s (sync)...\n", spec.ID)
		if cfgErr := configureAgentNetwork(ctx, spec.ID, &netCfg); cfgErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Printf("   [Agent] network config failed on %s: %v\n", spec.ID, cfgErr)
		} else {
			log.Printf("   [Agent] network config done on %s\n", spec.ID)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
		NodeID:         s.cfg.HostID,
		PublishPorts:   req.PublishPorts,
		Labels:         req.Labels,
		TapName:        spec.TapName,
		NetNSName:      spec.NetNSName,
		MacAddress:     spec.MacAddress, // persist so Restore doesn't need to re-derive it
		LastActivityAt: &now,
		Status:         "running",
		CreatedAt:      now,
		CreatedBy:      req.UserID,
	}
	if !syncEnabled {
		sandbox.Status = "booting"
	}

	log.Printf("   [SandboxService] Created sandbox %s with IP %s\n", sandbox.ID.Hex(), sandbox.OrgID.Hex())
	err = s.repo.Create(ctx, sandbox)
	if err != nil {
		return nil, fmt.Errorf("DB save failed: %w", err)
	}
	created = true

	if s.metrics != nil {
		s.metrics.RegisterSandbox(spec.ID, sandbox.Name, runtime.GetSocketPath(spec.ID), cpu, mem, diskMB)
		if sandbox.Status == "booting" {
			s.metrics.SetSandboxStatus(spec.ID, sandbox.Name, "booting")
		}
	}

	if syncEnabled && s.monitor != nil {
		s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
	}

	s.actors.afterCreate(cpu, mem, diskMB)
	if !syncEnabled {
		a := s.actors.GetOrCreate(spec.ID)
		if err := a.Enqueue(func() error {
			s.finishAsyncCreate(sandbox, spec, netCfg, req.EnvVars, timeout)
			return nil
		}); err != nil {
			s.failAsyncCreate(sandbox, spec)
		}
	}
	return sandbox, nil
}

func (s *SandboxService) finishAsyncCreate(sandbox *model.Sandbox, spec model.SandboxSpec, netCfg agentNetConfig, envVars map[string]string, timeout time.Duration) {
	id := spec.ID
	release := s.lifecycleLocks.Acquire(id)
	defer release()

	if timeout <= 0 {
		timeout = sandboxSyncTimeout(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := waitForAgent(ctx, id, timeout); err != nil {
		log.Printf("   [SandboxService] async create agent wait failed on %s: %v\n", id, err)
		s.failAsyncCreateLocked(sandbox, spec)
		return
	}
	if cfgErr := configureAgentNetwork(ctx, id, &netCfg); cfgErr != nil {
		if ctx.Err() != nil {
			s.failAsyncCreateLocked(sandbox, spec)
			return
		}
		log.Printf("   [Agent] network config failed on %s: %v\n", id, cfgErr)
	}
	if len(envVars) > 0 {
		if err := setAgentEnvVars(id, envVars); err != nil {
			fmt.Printf("[WARN] Failed to set env vars on agent: %v\n", err)
		}
	}

	ok, err := s.repo.UpdateStatusFrom(ctx, sandbox.ID, "booting", "running")
	if err != nil {
		log.Printf("   [SandboxService] async create status update failed on %s: %v\n", id, err)
		return
	}
	if !ok {
		return
	}
	if s.metrics != nil {
		s.metrics.SetSandboxStatus(id, sandbox.Name, "running")
	}
	if s.monitor != nil {
		s.monitor.Start(context.Background(), sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
	}
	if err := s.repo.TouchActivity(context.Background(), sandbox.ID); err != nil {
		log.Printf("[WARN] Failed to touch activity after async create for %s: %v", id, err)
	}
}

func (s *SandboxService) failAsyncCreate(sandbox *model.Sandbox, spec model.SandboxSpec) {
	release := s.lifecycleLocks.Acquire(spec.ID)
	defer release()
	s.failAsyncCreateLocked(sandbox, spec)
}

// failAsyncCreateLocked CAS-transitions booting → error then stops the VM.
// No-op if the row already left booting (Delete won). Does not FreeIP,
// Cleanup, or release admission capacity: "error" is recoverable via Start
// (bootFromDiskLocked), same as any other error/killed row, so the sandbox
// keeps its IP, overlay, and packing/running reservation until an explicit
// Delete actually removes it.
// Caller must hold the lifecycle lock for spec.ID.
func (s *SandboxService) failAsyncCreateLocked(sandbox *model.Sandbox, spec model.SandboxSpec) {
	id := spec.ID
	ok, err := s.repo.UpdateStatusFrom(context.Background(), sandbox.ID, "booting", "error")
	if err != nil || !ok {
		return
	}
	runtime.Stop(id)
	if spec.NetNSName != "" {
		_ = runtime.DeleteSandboxNetNS(spec.NetNSName)
	} else if spec.TapName != "" {
		_ = runtime.DeleteTap(spec.TapName)
	}
	if s.metrics != nil {
		s.metrics.SetSandboxStatus(id, sandbox.Name, "error")
		s.metrics.UnregisterSandbox(id)
	}
}

func (s *SandboxService) waitIfBooting(ctx context.Context, orgID primitive.ObjectID, id string) error {
	timeout := sandboxSyncTimeout(s.cfg.Sandbox.SyncTimeoutSec)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		sb, err := s.getOrgScopedSandbox(ctx, orgID, id)
		if err != nil {
			return err
		}
		switch sb.Status {
		case "booting":
		case "running":
			return nil
		case "error", "killed":
			return fmt.Errorf("sandbox boot failed (status: %s)", sb.Status)
		default:
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("sandbox still booting after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func sandboxListFilter(orgID primitive.ObjectID, labels map[string]string) bson.M {
	filter := bson.M{"orgId": orgID}
	for key, value := range labels {
		filter["labels."+key] = value
	}
	return filter
}

func diskMBForCreate(defaultDiskMB int, img *model.Image) int {
	if img != nil && img.SizeGB > 0 {
		return int(img.SizeGB) * 1024
	}
	return defaultDiskMB
}

func sandboxSyncTimeout(sec int) time.Duration {
	if sec <= 0 {
		sec = config.DefaultSandboxSyncTimeoutSec
	}
	return time.Duration(sec) * time.Second
}

func (s *SandboxService) Delete(ctx context.Context, orgID primitive.ObjectID, id string) error {
	return s.actors.GetOrCreate(id).Delete(ctx, func() error {
		release := s.lifecycleLocks.Acquire(id)
		defer release()
		return s.deleteLocked(ctx, orgID, id)
	})
}

// DeleteIfSnapshotted deletes only when the row is still snapshotted.
// A skip returns (false, nil) so packing is not released.
func (s *SandboxService) DeleteIfSnapshotted(ctx context.Context, orgID primitive.ObjectID, id string) (deleted bool, err error) {
	err = s.actors.GetOrCreate(id).Delete(ctx, func() error {
		release := s.lifecycleLocks.Acquire(id)
		defer release()

		sandbox, ferr := s.getOrgScopedSandbox(ctx, orgID, id)
		if ferr != nil {
			return ferr
		}
		if sandbox.Status != "snapshotted" {
			return nil
		}
		deleted = true
		return s.deleteLockedSandbox(ctx, orgID, id, sandbox)
	})
	if err != nil {
		deleted = false
	}
	return deleted, err
}

func (s *SandboxService) deleteLocked(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}
	return s.deleteLockedSandbox(ctx, orgID, id, sandbox)
}

func (s *SandboxService) deleteLockedSandbox(ctx context.Context, orgID primitive.ObjectID, id string, sandbox *model.Sandbox) (err error) {
	start := time.Now()
	defer func() { s.recordLifecycleOp(id, "delete", start, err) }()

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
		s.metrics.SetSandboxStatus(id, sandbox.Name, "deleted")
	}

	if err := runtime.Delete(id, sandbox.TapName, sandbox.NetNSName); err != nil {
		fmt.Printf("[WARN] Failed to delete sandbox %s: %v\n", id, err)
	}

	if s.monitor != nil {
		s.monitor.Stop(ctx, id)
	}

	if err := runtime.Cleanup(id); err != nil {
		fmt.Printf("[WARN] Failed to cleanup files for %s: %v\n", id, err)
	}

	s.actors.GetOrCreate(id).afterDelete(sandbox.Status == "running" || sandbox.Status == "booting", sandbox.CPU, sandbox.Mem, sandbox.DiskMB)
	s.actors.Unregister(id)
	return nil
}

func (s *SandboxService) Snapshot(ctx context.Context, orgID primitive.ObjectID, id string) error {
	return s.actors.GetOrCreate(id).Snapshot(ctx, func() error {
		release := s.lifecycleLocks.Acquire(id)
		defer release()
		return s.snapshotLocked(ctx, orgID, id)
	})
}

func (s *SandboxService) snapshotLocked(ctx context.Context, orgID primitive.ObjectID, id string) (err error) {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	if sandbox.Status != "running" {
		return fmt.Errorf("%w (current status: %s)", ErrSandboxNotRunning, sandbox.Status)
	}

	start := time.Now()
	defer func() { s.recordLifecycleOp(id, "sleep", start, err) }()

	if err = runtime.Snapshot(id); err != nil {
		return err
	}

	if s.monitor != nil {
		s.monitor.Stop(ctx, id)
	}

	var ok bool
	for attempt := 1; attempt <= 5; attempt++ {
		ok, err = s.repo.SetSnapshottedAtAndOrg(ctx, sandbox.ID, orgID)
		if err == nil {
			break
		}
		log.Printf("[Snapshot] Warning: failed to persist snapshotted state for %s (attempt %d/5): %v", id, attempt, err)
		time.Sleep(time.Duration(attempt*50) * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("failed to persist snapshotted state for %s after retries: %w", id, err)
	}
	if !ok {
		return ErrSandboxNotFound
	}

	if s.metrics != nil {
		s.metrics.SetSandboxStatus(sandbox.ID.Hex(), sandbox.Name, "snapshotted")
		s.metrics.UnregisterSandbox(sandbox.ID.Hex())
	}

	s.actors.GetOrCreate(id).afterSnapshot(sandbox.CPU, sandbox.Mem)
	return nil
}

func (s *SandboxService) Restore(ctx context.Context, orgID primitive.ObjectID, id string) error {
	a := s.actors.GetOrCreate(id)
	return a.Restore(ctx, func() error {
		release := s.lifecycleLocks.Acquire(id)
		defer release()

		sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
		if err != nil {
			return err
		}
		if sandbox.Status != "snapshotted" {
			return fmt.Errorf("sandbox is not snapshotted (current status: %s)", sandbox.Status)
		}

		start := time.Now()
		var opErr error
		defer func() { s.recordLifecycleOp(id, "wake", start, opErr) }()

		if err := a.beforeBoot(ctx, sandbox.CPU, sandbox.Mem); err != nil {
			opErr = err
			return err
		}
		opErr = s.restoreLocked(ctx, orgID, sandbox)
		if opErr != nil {
			a.afterBootFailed(sandbox.CPU, sandbox.Mem)
			return opErr
		}
		a.afterBoot(sandbox.CPU, sandbox.Mem)
		return nil
	})
}

// Start boots a stopped sandbox back into "running". Accepts snapshotted, killed,
// or error statuses and restores from the latest on-disk snapshot. Sandboxes
// that were killed before ever being snapshotted have no recoverable state and
// must be recreated instead.
func (s *SandboxService) Start(ctx context.Context, orgID primitive.ObjectID, id string) error {
	a := s.actors.GetOrCreate(id)
	return a.Start(ctx, func() error {
		release := s.lifecycleLocks.Acquire(id)
		defer release()
		return s.startLocked(ctx, a, orgID, id)
	})
}

func (s *SandboxService) startLocked(ctx context.Context, a *Actor, orgID primitive.ObjectID, id string) (err error) {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	switch sandbox.Status {
	case "running", "booting":
		return nil
	case "snapshotted", "killed", "error":
	default:
		return fmt.Errorf("sandbox cannot be started from status: %s", sandbox.Status)
	}

	op := "start"
	if sandbox.Status == "snapshotted" {
		op = "wake"
	}
	start := time.Now()
	defer func() { s.recordLifecycleOp(id, op, start, err) }()

	if sandbox.Status == "killed" || sandbox.Status == "error" {
		if sandboxVMRunning(id) {
			if _, uerr := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "running"); uerr != nil {
				return fmt.Errorf("VM running but failed to update DB status: %w", uerr)
			}
			if err := s.repo.TouchActivity(ctx, sandbox.ID); err != nil {
				log.Printf("[WARN] Failed to touch activity on reattach for %s: %v", id, err)
			}
			if s.metrics != nil {
				s.metrics.RegisterSandbox(id, sandbox.Name, runtime.GetSocketPath(id), sandbox.CPU, sandbox.Mem, sandbox.DiskMB)
			}
			if s.monitor != nil {
				s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
			}
			return nil
		}
	}

	boot := func(fn func() error) error {
		if berr := a.beforeBoot(ctx, sandbox.CPU, sandbox.Mem); berr != nil {
			return berr
		}
		if berr := fn(); berr != nil {
			a.afterBootFailed(sandbox.CPU, sandbox.Mem)
			return berr
		}
		a.afterBoot(sandbox.CPU, sandbox.Mem)
		return nil
	}

	if runtime.GetLatestSnapshotDir(id) != "" {
		return boot(func() error { return s.restoreLocked(ctx, orgID, sandbox) })
	}

	overlayPath := runtime.GetOverlayPath(id)
	if s.cfg.Sandbox.DiskFormat == "raw" {
		overlayPath = runtime.GetRawOverlayPath(id)
	}
	if _, statErr := os.Stat(overlayPath); statErr == nil {
		return boot(func() error { return s.bootFromDiskLocked(ctx, orgID, sandbox, overlayPath) })
	}

	return fmt.Errorf("no snapshot or disk image available to start from; delete and recreate the sandbox")
}

// bootFromDiskLocked boots a sandbox from its existing overlay disk without a snapshot (memory state lost).
// The caller MUST hold the lifecycle lock for sandbox.ID.
func (s *SandboxService) bootFromDiskLocked(ctx context.Context, orgID primitive.ObjectID, sandbox *model.Sandbox, overlayPath string) error {
	id := sandbox.ID.Hex()

	macAddr := sandbox.MacAddress
	if macAddr == "" {
		macAddr = runtime.GenerateMAC(sandbox.IP)
	}

	spec := model.SandboxSpec{
		ID:         id,
		Type:       sandbox.Image,
		CPUs:       sandbox.CPU,
		MemoryMB:   sandbox.Mem,
		IPAddress:  sandbox.IP,
		TapName:    sandbox.TapName,
		MacAddress: macAddr,
		NetNSName:  sandbox.NetNSName,
	}

	proc, err := runtime.BootFromDisk(*s.cfg, spec, overlayPath)
	if err != nil {
		return fmt.Errorf("failed to boot VM from disk: %w", err)
	}
	s.actors.GetOrCreate(id).Attach(proc)

	cleanup := func() {
		log.Printf("[BootFromDisk] Rolling back: stopping VM %s", id)
		if stopErr := runtime.Stop(id); stopErr != nil {
			log.Printf("[BootFromDisk] Rollback stop failed for %s: %v", id, stopErr)
		}
	}

	if err := waitForAgent(ctx, id, 30*time.Second); err != nil {
		cleanup()
		return fmt.Errorf("agent not ready after disk boot: %w", err)
	}

	go func() {
		defer util.Track("configureAgentNetwork - " + id)()
		netCfg := buildAgentNetConfig(s.cfg, sandbox.IP, sandbox.Name)
		if cfgErr := configureAgentNetwork(context.Background(), id, &netCfg); cfgErr != nil {
			log.Printf("   [BootFromDisk] network re-config failed on %s: %v\n", id, cfgErr)
		} else {
			log.Printf("   [BootFromDisk] network re-config done on %s\n", id)
		}
		syncSandboxClock(id)
	}()

	if _, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "running"); err != nil {
		cleanup()
		return fmt.Errorf("VM booted but failed to update DB status: %w", err)
	}

	if err := s.repo.TouchActivity(ctx, sandbox.ID); err != nil {
		log.Printf("[WARN] Failed to touch activity on disk boot for %s: %v", id, err)
	}

	if s.metrics != nil {
		s.metrics.RegisterSandbox(id, sandbox.Name, runtime.GetSocketPath(id), sandbox.CPU, sandbox.Mem, sandbox.DiskMB)
	}

	if s.monitor != nil {
		s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
	}

	return nil
}

// restoreLocked performs the runtime+DB work for restoring a sandbox. The caller
// MUST hold the lifecycle lock for sandbox.ID and MUST have verified that the
// sandbox's status is "snapshotted" under that lock.
func (s *SandboxService) restoreLocked(ctx context.Context, orgID primitive.ObjectID, sandbox *model.Sandbox) (err error) {
	id := sandbox.ID.Hex()

	imageName := sandbox.Image
	if !strings.Contains(imageName, ":") {
		img, err := s.imageRepo.GetLatestByNameForOrg(ctx, imageName, orgID)
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

	proc, err := runtime.Restore(*s.cfg, spec, overlayPath, snapshotDir)
	if err != nil {
		return fmt.Errorf("failed to restore VM: %w", err)
	}
	s.actors.GetOrCreate(id).Attach(proc)

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

	go func() {
		defer util.Track("configureAgentNetwork - " + id)()
		netCfg := buildAgentNetConfig(s.cfg, sandbox.IP, sandbox.Name)
		if cfgErr := configureAgentNetwork(context.Background(), id, &netCfg); cfgErr != nil {
			log.Printf("   [Restore] network re-config failed on %s: %v\n", id, cfgErr)
		} else {
			log.Printf("   [Restore] network re-config done on %s\n", id)
		}
		syncSandboxClock(id)
	}()

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

// EnsureRunning restores a snapshotted sandbox. The actor inbox serializes
// concurrent callers; status is re-read under the lifecycle lock.
func (s *SandboxService) EnsureRunning(ctx context.Context, orgID primitive.ObjectID, id string) error {
	if err := s.waitIfBooting(ctx, orgID, id); err != nil {
		return err
	}
	bgCtx := context.WithoutCancel(ctx)
	a := s.actors.GetOrCreate(id)
	return a.Restore(bgCtx, func() error {
		release := s.lifecycleLocks.Acquire(id)
		defer release()

		cur, err := s.getOrgScopedSandbox(bgCtx, orgID, id)
		if err != nil {
			return err
		}
		if cur.Status == "running" {
			return nil
		}
		if cur.Status != "snapshotted" {
			return fmt.Errorf("sandbox in unexpected state for auto-restore: %s", cur.Status)
		}

		if err := a.beforeBoot(bgCtx, cur.CPU, cur.Mem); err != nil {
			return err
		}

		log.Printf("[Auto-Restore] Sandbox %s is snapshotted, restoring...\n", id)
		if err := s.restoreLocked(bgCtx, orgID, cur); err != nil {
			a.afterBootFailed(cur.CPU, cur.Mem)
			return fmt.Errorf("failed to auto-restore sandbox: %w", err)
		}
		a.afterBoot(cur.CPU, cur.Mem)
		log.Printf("[Auto-Restore] Sandbox %s restored and ready\n", id)
		return nil
	})
}

func (s *SandboxService) Info(id string) (string, error) {
	return runtime.Info(id)
}

// RefreshStatuses checks each sandbox health and updates status field in DB.
// Status values: running, snapshotted, killed, deleted.
// Scoped to this node's HostID. Also resurrects false-killed rows when the local VM is still up.
func (s *SandboxService) RefreshStatuses(ctx context.Context) error {
	projection := bson.M{"_id": 1, "status": 1, "name": 1}
	sandboxes, err := s.repo.FindForHealth(ctx, s.cfg.HostID, options.FindOptions{Projection: projection})

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

		switch sb.Status {
		case "running", "killed", "error":
		default:
			continue
		}

		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer func() { <-sem; wg.Done() }()

			release := s.lifecycleLocks.TryAcquire(id)
			if release == nil {
				return
			}
			defer release()

			cur, err := s.repo.FindByID(ctx, sb.ID, options.FindOneOptions{})
			if err != nil || cur == nil {
				return
			}

			alive := sandboxVMRunning(id)

			switch cur.Status {
			case "running":
				if alive {
					return
				}
				if err := s.repo.UpdateStatusForHealth(ctx, sb.ID, "killed"); err != nil {
					fmt.Printf("[health] failed to update status for %s: %v\n", id, err)
				} else if s.metrics != nil {
					s.metrics.SetSandboxStatus(id, cur.Name, "killed")
					s.metrics.UnregisterSandbox(id)
				}
			case "killed", "error":
				if !alive {
					return
				}
				if _, err := s.repo.UpdateStatusFrom(ctx, sb.ID, cur.Status, "running"); err != nil {
					fmt.Printf("[health] failed to resurrect status for %s: %v\n", id, err)
					return
				}
				if s.metrics != nil {
					s.metrics.RegisterSandbox(id, cur.Name, runtime.GetSocketPath(id), cur.CPU, cur.Mem, cur.DiskMB)
				}
				if s.monitor != nil {
					s.monitor.Start(ctx, cur.ID, cur.OrgID, cur.CreatedBy)
				}
				fmt.Printf("[health] resurrected sandbox %s (%s) — VM still running\n", cur.Name, id)
			}
		}()
	}

	wg.Wait()
	return nil
}

// sandboxVMRunning reports whether cloud-hypervisor on this host says the VM is up.
func sandboxVMRunning(id string) bool {
	client := runtime.NewAPIClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return false
	}
	apiCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sbxState, err := client.GetStateWithContext(apiCtx)
	if err != nil {
		return false
	}
	return isRunningVMState(sbxState)
}

func isRunningVMState(state string) bool {
	switch strings.ToLower(state) {
	case "running", "runningvirtualized":
		return true
	default:
		return false
	}
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

	if timeout <= 0 {
		timeout = sandboxSyncTimeout(0)
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
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
			return fmt.Errorf("agent readiness timeout after %s (%d attempts): last error: %v: %w",
				time.Since(start), attempts, lastErr, ctx.Err())
		case <-ticker.C:
			// next attempt
		}
	}
}

func configureAgentNetwork(ctx context.Context, sbxID string, netCfg *agentNetConfig) error {
	if netCfg == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	jsonData, err := json.Marshal(netCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal network config: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := AgentCommand(attemptCtx, nil, sbxID, bytes.NewReader(jsonData), "/configure-network", http.MethodPost)
		cancel()

		if err != nil {
			lastErr = fmt.Errorf("configure network failed: %w", err)
			if sleepErr := sleepCtx(ctx, 50*time.Millisecond); sleepErr != nil {
				return sleepErr
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("configure network status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			if sleepErr := sleepCtx(ctx, 50*time.Millisecond); sleepErr != nil {
				return sleepErr
			}
			continue
		}

		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil
	}

	return lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
func (s *SandboxService) TouchActivity(ctx context.Context, id string) {
	objID, err := util.ParseObjectID(id)
	if err != nil {
		return
	}
	_ = s.repo.TouchActivity(ctx, objID)
}
