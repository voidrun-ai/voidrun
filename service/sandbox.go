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
	errIdleNotDue        = errors.New("idle snapshot not due")
	errAutoSleepOff      = errors.New("auto-sleep disabled")
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
	repo        repository.ISandboxRepository
	imageRepo   repository.IImageRepository
	cfg         *config.Config
	metrics     *metrics.Manager
	monitor     *runtime.EventMonitor
	projection  primitive.M
	supervisors *SupervisorRegistry
	autoLifeSem chan struct{}
	bootMu      sync.Mutex
	bootWait    map[string]*bootWait
}

const (
	activityTouchGap         = 5 * time.Second
	ensureRunningTimeout     = 10 * time.Minute
	asyncCreateCommitTimeout = 10 * time.Second
)

type bootWait struct {
	done chan struct{}
	err  error
}

// NewSandboxService creates a new sandbox service.
func NewSandboxService(
	cfg *config.Config,
	repo repository.ISandboxRepository,
	imageRepo repository.IImageRepository,
	metricsManager *metrics.Manager,
	monitor *runtime.EventMonitor,
) *SandboxService {
	conc := 10
	if cfg != nil && cfg.AutoLifecycle.Concurrency > 0 {
		conc = cfg.AutoLifecycle.Concurrency
	}
	s := &SandboxService{
		repo:        repo,
		imageRepo:   imageRepo,
		cfg:         cfg,
		metrics:     metricsManager,
		monitor:     monitor,
		supervisors: NewSupervisorRegistry(),
		autoLifeSem: make(chan struct{}, conc),
		bootWait:    make(map[string]*bootWait),
		projection: bson.M{
			"_id":               1,
			"name":              1,
			"image":             1,
			"ip":                1,
			"cpu":               1,
			"mem":               1,
			"diskMB":            1,
			"status":            1,
			"autoSleep":         1,
			"consoleLogEnabled": 1,
			"lastActivityAt":    1,
			"snapshottedAt":     1,
			"packed":            1,
			"packPath":          1,
			"archiveKey":        1,
			"archivedAt":        1,
			"coldCleared":       1,
			"createdAt":         1,
			"orgId":             1,
			"createdBy":         1,
			"region":            1,
			"nodeId":            1,
			"tapName":           1,
			"tapDeleted":        1,
			"netnsName":         1,
			"macAddress":        1,
			"publishPorts":      1,
			"labels":            1,
		},
	}
	s.supervisors.SetOnProcessExit(s.onSupervisorProcessExit)
	s.supervisors.SetObservers(func(n int) {
		if s.metrics != nil {
			s.metrics.SetSupervisorCount(n)
		}
	}, func(kind string, d time.Duration) {
		if s.metrics != nil {
			s.metrics.ObserveSupervisorQueueWait(kind, d.Seconds())
		}
	})
	return s
}

// AddLifecycleListener registers a subscriber (gateway, billing, audit).
func (s *SandboxService) AddLifecycleListener(fn LifecycleListener) {
	s.supervisors.AddLifecycleListener(fn)
}

func lifeFrom(sb *model.Sandbox, op, phase, from, to string) model.LifecycleEvent {
	ev := model.LifecycleEvent{Op: op, Phase: phase, FromStatus: from, ToStatus: to}
	if sb == nil {
		return ev
	}
	ev.OrgID = sb.OrgID.Hex()
	if !sb.CreatedBy.IsZero() {
		ev.UserID = sb.CreatedBy.Hex()
	}
	ev.SandboxName = sb.Name
	ev.IP = sb.IP
	ev.PublishPorts = sb.PublishPorts
	return ev
}

func (s *SandboxService) emitLife(ctx context.Context, id string, ev model.LifecycleEvent) {
	s.supervisors.Emit(ctx, id, ev)
}

// attachOwned gives proc to the supervisor. On error the process is killed and reaped
// here, because the caller no longer has a live VM to clean up.
func attachOwned(a *Supervisor, proc *os.Process) error {
	if err := a.Attach(proc); err != nil {
		_ = proc.Kill()
		_, _ = proc.Wait()
		return err
	}
	return nil
}

// SetAdmission installs the optional lifecycle plugin. Only EE calls this.
func (s *SandboxService) SetAdmission(a Admission) {
	s.supervisors.SetAdmission(a)
}

// onSupervisorProcessExit CAS-writes running→killed. Snapshotted/deleted rows are left alone.
func (s *SandboxService) onSupervisorProcessExit(id string) {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return
	}
	ctx := context.Background()
	ok, err := s.repo.UpdateStatusFrom(ctx, oid, "running", "killed")
	if err != nil {
		log.Printf("[supervisor] %s wait status: %v", id, err)
		return
	}
	if !ok {
		return
	}
	cur, err := s.repo.FindByID(ctx, oid, options.FindOneOptions{Projection: bson.M{"name": 1, "orgId": 1, "createdBy": 1, "ip": 1, "publishPorts": 1}})
	if err != nil {
		cur = nil
	}
	s.emitLife(ctx, id, lifeFrom(cur, model.OpKill, model.PhaseCommitted, "running", "killed"))
	if s.metrics == nil {
		return
	}
	name := id
	if cur != nil && cur.Name != "" {
		name = cur.Name
	}
	s.metrics.SetSandboxStatus(id, name, "killed")
	s.metrics.UnregisterSandbox(id)
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
	s.emitLife(ctx, id, lifeFrom(sandbox, model.OpPorts, model.PhaseCommitted, sandbox.Status, sandbox.Status))
	return sandbox, nil
}

func sandboxUpdateSet(req model.UpdateSandboxRequest) bson.M {
	set := bson.M{}
	if req.AutoSleep != nil {
		set["autoSleep"] = *req.AutoSleep
	}
	return set
}

func (s *SandboxService) Update(ctx context.Context, orgID primitive.ObjectID, id string, req model.UpdateSandboxRequest) (*model.Sandbox, error) {
	if err := util.ValidateUpdateSandboxRequest(req.AutoSleep); err != nil {
		return nil, err
	}
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if sandbox.Status == "deleted" {
		return nil, ErrSandboxNotFound
	}
	fields := sandboxUpdateSet(req)
	ok, err := s.repo.UpdateFieldsByIDAndOrg(ctx, sandbox.ID, orgID, fields)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSandboxNotFound
	}
	if req.AutoSleep != nil {
		sandbox.AutoSleep = *req.AutoSleep
	}
	if a := s.supervisors.Get(id); a != nil {
		_ = a.Coalesce(func() error {
			fresh, ferr := s.getOrgScopedSandbox(context.Background(), orgID, id)
			if ferr != nil {
				return ferr
			}
			s.syncAutoTimers(fresh)
			return nil
		})
	}
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

	if err := s.supervisors.BeforeCreate(ctx, cpu, mem, diskMB); err != nil {
		return nil, err
	}
	created := false
	defer func() {
		if !created {
			s.supervisors.AfterCreateFailed(cpu, mem, diskMB)
		}
	}()

	consoleLogEnabled := s.cfg.Sandbox.ConsoleLogEnabled

	spec := model.SandboxSpec{
		ID:                instanceID,
		Type:              imageName,
		CPUs:              cpu,
		MemoryMB:          mem,
		DiskMB:            diskMB,
		IPAddress:         ip,
		ConsoleLogEnabled: consoleLogEnabled,
	}

	haveVM := false
	cleanup := func() {
		fmt.Printf("   [!] Rollback: Deleting failed instance %s\n", spec.ID)
		if haveVM {
			runtime.Stop(spec.ID)
			s.supervisors.Unregister(spec.ID)
		}
		if spec.NetNSName != "" {
			_ = runtime.DeleteSandboxNetNS(spec.NetNSName)
		} else if spec.TapName != "" {
			_ = runtime.DeleteTap(spec.TapName)
		}
		runtime.StopConsolePump(spec.ID)
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
	if err := attachOwned(s.supervisors.GetOrCreate(spec.ID), proc); err != nil {
		s.supervisors.Unregister(spec.ID)
		return nil, fmt.Errorf("attach vm: %w", err)
	}
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
		ID:                objID,
		Name:              req.Name,
		Image:             imageName,
		IP:                ip,
		CPU:               cpu,
		Mem:               mem,
		DiskMB:            diskMB,
		OrgID:             req.OrgID,
		EnvVars:           req.EnvVars,
		AutoSleep:         autoSleep,
		ConsoleLogEnabled: consoleLogEnabled,
		Region:            req.Region,
		NodeID:            s.cfg.HostID,
		PublishPorts:      req.PublishPorts,
		Labels:            req.Labels,
		TapName:           spec.TapName,
		NetNSName:         spec.NetNSName,
		MacAddress:        spec.MacAddress, // persist so Restore doesn't need to re-derive it
		LastActivityAt:    &now,
		Status:            "running",
		CreatedAt:         now,
		CreatedBy:         req.UserID,
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
	s.emitLife(ctx, spec.ID, lifeFrom(sandbox, model.OpCreate, model.PhaseCommitted, "", sandbox.Status))

	if s.metrics != nil {
		s.metrics.RegisterSandbox(spec.ID, sandbox.Name, runtime.GetSocketPath(spec.ID), cpu, mem, diskMB)
		if sandbox.Status == "booting" {
			s.metrics.SetSandboxStatus(spec.ID, sandbox.Name, "booting")
		}
	}

	if syncEnabled && s.monitor != nil {
		s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
	}

	s.supervisors.AfterCreate(cpu, mem, diskMB)
	s.syncAutoTimers(sandbox)
	if !syncEnabled {
		a := s.supervisors.GetOrCreate(spec.ID)
		go s.finishAsyncCreate(a, sandbox, spec, netCfg, req.EnvVars, timeout)
	}
	return sandbox, nil
}

func (s *SandboxService) finishAsyncCreate(a *Supervisor, sandbox *model.Sandbox, spec model.SandboxSpec, netCfg agentNetConfig, envVars map[string]string, timeout time.Duration) {
	id := spec.ID

	if timeout <= 0 {
		timeout = sandboxSyncTimeout(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	waitErr := waitForAgent(ctx, id, timeout)
	var cfgErr error
	if waitErr == nil {
		cfgErr = configureAgentNetwork(ctx, id, &netCfg)
		if cfgErr == nil && len(envVars) > 0 {
			if err := setAgentEnvVars(id, envVars); err != nil {
				fmt.Printf("[WARN] Failed to set env vars on agent: %v\n", err)
			}
		}
	}

	err := a.Do(context.Background(), func() error {
		if waitErr != nil {
			log.Printf("   [SandboxService] async create agent wait failed on %s: %v\n", id, waitErr)
			s.failAsyncCreateLocked(sandbox, spec)
			return nil
		}
		if cfgErr != nil {
			if ctx.Err() != nil {
				s.failAsyncCreateLocked(sandbox, spec)
				return nil
			}
			log.Printf("   [Agent] network config failed on %s: %v\n", id, cfgErr)
		}
		commitCtx, commitCancel := context.WithTimeout(context.Background(), asyncCreateCommitTimeout)
		defer commitCancel()
		ok, uerr := s.repo.UpdateStatusFrom(commitCtx, sandbox.ID, "booting", "running")
		if uerr != nil {
			log.Printf("   [SandboxService] async create status update failed on %s: %v\n", id, uerr)
			return nil
		}
		if !ok {
			return nil
		}
		s.emitLife(commitCtx, id, lifeFrom(sandbox, model.OpCreate, model.PhaseCommitted, "booting", "running"))
		if s.metrics != nil {
			s.metrics.SetSandboxStatus(id, sandbox.Name, "running")
		}
		if s.monitor != nil {
			s.monitor.Start(context.Background(), sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
		}
		if err := s.repo.TouchActivity(context.Background(), sandbox.ID); err != nil {
			log.Printf("[WARN] Failed to touch activity after async create for %s: %v", id, err)
		}
		now := time.Now()
		sandbox.Status = "running"
		sandbox.LastActivityAt = &now
		s.syncAutoTimers(sandbox)
		return nil
	})
	if err != nil && !errors.Is(err, errSupervisorStopped) {
		log.Printf("   [SandboxService] async create commit failed on %s: %v\n", id, err)
	}
}

// failAsyncCreateLocked CAS-transitions booting → error then stops the VM.
// No-op if the row already left booting (Delete won). Does not FreeIP,
// Cleanup, or release admission capacity: "error" is recoverable via Start
// (bootFromDiskLocked), same as any other error/killed row, so the sandbox
// keeps its IP, overlay, and packing/running reservation until an explicit
// Delete actually removes it.
// Caller runs on the supervisor inbox, or the supervisor has already stopped.
func (s *SandboxService) failAsyncCreateLocked(sandbox *model.Sandbox, spec model.SandboxSpec) {
	id := spec.ID
	ok, err := s.repo.UpdateStatusFrom(context.Background(), sandbox.ID, "booting", "error")
	if err != nil || !ok {
		return
	}
	s.emitLife(context.Background(), id, lifeFrom(sandbox, model.OpCreate, model.PhaseFailed, "booting", "error"))
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
	s.bootMu.Lock()
	if w, ok := s.bootWait[id]; ok {
		s.bootMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
			return w.err
		}
	}
	w := &bootWait{done: make(chan struct{})}
	s.bootWait[id] = w
	s.bootMu.Unlock()
	go s.pollBooting(orgID, id, w)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return w.err
	}
}

func (s *SandboxService) pollBooting(orgID primitive.ObjectID, id string, w *bootWait) {
	defer func() {
		close(w.done)
		s.bootMu.Lock()
		if s.bootWait[id] == w {
			delete(s.bootWait, id)
		}
		s.bootMu.Unlock()
	}()

	sec := 0
	if s.cfg != nil {
		sec = s.cfg.Sandbox.SyncTimeoutSec
	}
	timeout := sandboxSyncTimeout(sec)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		sb, err := s.getOrgScopedSandbox(ctx, orgID, id)
		if err != nil {
			w.err = err
			return
		}
		switch sb.Status {
		case "booting":
		case "running":
			return
		case "error", "killed":
			w.err = fmt.Errorf("sandbox boot failed (status: %s)", sb.Status)
			return
		default:
			return
		}
		if !time.Now().Before(deadline) {
			w.err = fmt.Errorf("sandbox still booting after %s", timeout)
			return
		}
		select {
		case <-ctx.Done():
			w.err = ctx.Err()
			return
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

func (s *SandboxService) supervisorForSandbox(ctx context.Context, orgID primitive.ObjectID, id string) (*Supervisor, error) {
	if _, err := s.getOrgScopedSandbox(ctx, orgID, id); err != nil {
		return nil, err
	}
	return s.supervisors.GetOrCreate(id), nil
}

func (s *SandboxService) Delete(ctx context.Context, orgID primitive.ObjectID, id string) error {
	a, err := s.supervisorForSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}
	return a.Delete(ctx, func() error {
		return s.deleteLocked(ctx, orgID, id)
	})
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

	from := sandbox.Status
	s.emitLife(ctx, id, lifeFrom(sandbox, model.OpDelete, model.PhaseStarting, from, "deleted"))

	// Commit the status before releasing the IP. Freeing first would return the
	// address to the pool while the row still reads snapshotted, so a failed
	// status write could hand the same IP to another sandbox.
	ok, err := s.repo.UpdateStatusByIDAndOrg(ctx, sandbox.ID, orgID, "deleted")
	if err != nil {
		s.emitLife(ctx, id, lifeFrom(sandbox, model.OpDelete, model.PhaseFailed, from, from))
		return err
	}
	if !ok {
		s.emitLife(ctx, id, lifeFrom(sandbox, model.OpDelete, model.PhaseFailed, from, from))
		return ErrSandboxNotFound
	}
	s.repo.FreeIP(ctx, sandbox.IP)
	s.emitLife(ctx, id, lifeFrom(sandbox, model.OpDelete, model.PhaseCommitted, from, "deleted"))

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

	s.supervisors.GetOrCreate(id).AfterDelete(sandbox.Status == "running" || sandbox.Status == "booting", sandbox.CPU, sandbox.Mem, sandbox.DiskMB)
	s.supervisors.Unregister(id)
	return nil
}

func (s *SandboxService) Snapshot(ctx context.Context, orgID primitive.ObjectID, id string) error {
	a, err := s.supervisorForSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}
	return a.Snapshot(ctx, func() error {
		return s.snapshotLocked(ctx, orgID, id)
	})
}

func (s *SandboxService) snapshotIfStillIdle(ctx context.Context, orgID primitive.ObjectID, id string) error {
	sb, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}
	if sb.Status != "running" {
		return fmt.Errorf("%w (current status: %s)", ErrSandboxNotRunning, sb.Status)
	}
	if !sb.AutoSleep {
		return errAutoSleepOff
	}
	idle := time.Duration(0)
	if s.cfg != nil {
		idle = time.Duration(s.cfg.AutoLifecycle.SnapshotAfterIdleSec) * time.Second
	}
	if activityFresh(sb.LastActivityAt, idle, time.Now()) {
		return errIdleNotDue
	}
	return s.snapshotLocked(ctx, orgID, id)
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

	s.emitLife(ctx, id, lifeFrom(sandbox, model.OpSleep, model.PhaseStarting, "running", "snapshotted"))

	if err = runtime.Snapshot(id); err != nil {
		s.emitLife(ctx, id, lifeFrom(sandbox, model.OpSleep, model.PhaseFailed, "running", "running"))
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
		s.emitLife(ctx, id, lifeFrom(sandbox, model.OpSleep, model.PhaseFailed, "running", "running"))
		return fmt.Errorf("failed to persist snapshotted state for %s after retries: %w", id, err)
	}
	if !ok {
		s.emitLife(ctx, id, lifeFrom(sandbox, model.OpSleep, model.PhaseFailed, "running", "running"))
		return ErrSandboxNotFound
	}
	s.emitLife(ctx, id, lifeFrom(sandbox, model.OpSleep, model.PhaseCommitted, "running", "snapshotted"))

	if s.metrics != nil {
		s.metrics.SetSandboxStatus(sandbox.ID.Hex(), sandbox.Name, "snapshotted")
		s.metrics.UnregisterSandbox(sandbox.ID.Hex())
	}

	s.supervisors.GetOrCreate(id).AfterSnapshot(sandbox.CPU, sandbox.Mem)
	now := time.Now()
	sandbox.Status = "snapshotted"
	sandbox.SnapshottedAt = &now
	s.syncAutoTimers(sandbox)
	return nil
}

func (s *SandboxService) Restore(ctx context.Context, orgID primitive.ObjectID, id string) error {
	a := s.supervisors.GetOrCreate(id)
	err := a.Restore(ctx, func() error {
		sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
		if err != nil {
			return err
		}
		if sandbox.Status != "snapshotted" && sandbox.Status != "archived" {
			return fmt.Errorf("sandbox is not snapshotted (current status: %s)", sandbox.Status)
		}

		start := time.Now()
		var opErr error
		defer func() { s.recordLifecycleOp(id, "wake", start, opErr) }()

		if err := a.BeforeBoot(ctx, sandbox.CPU, sandbox.Mem); err != nil {
			opErr = err
			return err
		}
		from := sandbox.Status
		s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseStarting, from, "running"))
		opErr = s.restoreLocked(ctx, orgID, sandbox)
		if opErr != nil {
			s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseFailed, from, from))
			a.AfterBootFailed(sandbox.CPU, sandbox.Mem)
			return opErr
		}
		a.AfterBoot(sandbox.CPU, sandbox.Mem)
		return nil
	})
	if errors.Is(err, ErrSandboxNotFound) {
		s.supervisors.Unregister(id)
	}
	return err
}

// Start boots a stopped sandbox back into "running". Accepts snapshotted, killed,
// or error statuses and restores from the latest on-disk snapshot. Sandboxes
// that were killed before ever being snapshotted have no recoverable state and
// must be recreated instead.
func (s *SandboxService) Start(ctx context.Context, orgID primitive.ObjectID, id string) error {
	a, err := s.supervisorForSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}
	return a.Start(ctx, func() error {
		return s.startLocked(ctx, a, orgID, id)
	})
}

func (s *SandboxService) startLocked(ctx context.Context, a *Supervisor, orgID primitive.ObjectID, id string) (err error) {
	sandbox, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}

	switch sandbox.Status {
	case "running", "booting":
		s.adoptRunning(sandbox)
		return nil
	case "snapshotted", "archived", "killed", "error":
	default:
		return fmt.Errorf("sandbox cannot be started from status: %s", sandbox.Status)
	}

	op := "start"
	if sandbox.Status == "snapshotted" || sandbox.Status == "archived" {
		op = "wake"
	}
	start := time.Now()
	defer func() { s.recordLifecycleOp(id, op, start, err) }()

	if sandbox.Status == "killed" || sandbox.Status == "error" {
		if sandboxVMRunning(id) {
			from := sandbox.Status
			if _, uerr := s.repo.SetRunning(ctx, sandbox.ID, orgID); uerr != nil {
				s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseFailed, from, from))
				return fmt.Errorf("VM running but failed to update DB status: %w", uerr)
			}
			s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseCommitted, from, "running"))
			now := time.Now()
			sandbox.Status = "running"
			sandbox.LastActivityAt = &now
			s.adoptRunning(sandbox)
			if s.metrics != nil {
				s.metrics.RegisterSandbox(id, sandbox.Name, runtime.GetSocketPath(id), sandbox.CPU, sandbox.Mem, sandbox.DiskMB)
			}
			if s.monitor != nil {
				s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
			}
			return nil
		}
	}

	from := sandbox.Status
	boot := func(fn func() error) error {
		if berr := a.BeforeBoot(ctx, sandbox.CPU, sandbox.Mem); berr != nil {
			return berr
		}
		s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseStarting, from, "running"))
		if berr := fn(); berr != nil {
			s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseFailed, from, from))
			a.AfterBootFailed(sandbox.CPU, sandbox.Mem)
			return berr
		}
		a.AfterBoot(sandbox.CPU, sandbox.Mem)
		return nil
	}

	claimedFrom, err := s.claimWake(ctx, sandbox)
	if err != nil {
		return err
	}
	if sandbox.Status == "running" || (sandbox.Status == "booting" && claimedFrom == "") {
		s.adoptRunning(sandbox)
		return nil
	}
	if claimedFrom != "" {
		defer func() {
			if err != nil {
				_, _ = s.repo.UpdateStatusFrom(context.Background(), sandbox.ID, "booting", claimedFrom)
			}
		}()
	}

	if err := a.BeforeWake(ctx, sandbox); err != nil {
		return err
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
// Caller runs on the supervisor inbox.
func (s *SandboxService) bootFromDiskLocked(ctx context.Context, orgID primitive.ObjectID, sandbox *model.Sandbox, overlayPath string) error {
	id := sandbox.ID.Hex()

	macAddr := sandbox.MacAddress
	if macAddr == "" {
		macAddr = runtime.GenerateMAC(sandbox.IP)
	}

	spec := model.SandboxSpec{
		ID:                id,
		Type:              sandbox.Image,
		CPUs:              sandbox.CPU,
		MemoryMB:          sandbox.Mem,
		IPAddress:         sandbox.IP,
		TapName:           sandbox.TapName,
		MacAddress:        macAddr,
		NetNSName:         sandbox.NetNSName,
		ConsoleLogEnabled: sandbox.ConsoleLogEnabled,
	}

	proc, err := runtime.BootFromDisk(*s.cfg, spec, overlayPath)
	if err != nil {
		return fmt.Errorf("failed to boot VM from disk: %w", err)
	}
	if err := attachOwned(s.supervisors.GetOrCreate(id), proc); err != nil {
		return fmt.Errorf("attach vm: %w", err)
	}

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

	if _, err := s.repo.SetRunning(ctx, sandbox.ID, orgID); err != nil {
		cleanup()
		return fmt.Errorf("VM booted but failed to update DB status: %w", err)
	}
	s.supervisors.GetOrCreate(id).AfterWake(ctx, sandbox)
	s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseCommitted, sandbox.Status, "running"))
	now := time.Now()
	sandbox.Status = "running"
	sandbox.LastActivityAt = &now
	s.syncAutoTimers(sandbox)

	if s.metrics != nil {
		s.metrics.RegisterSandbox(id, sandbox.Name, runtime.GetSocketPath(id), sandbox.CPU, sandbox.Mem, sandbox.DiskMB)
	}

	if s.monitor != nil {
		s.monitor.Start(ctx, sandbox.ID, sandbox.OrgID, sandbox.CreatedBy)
	}

	return nil
}

// restoreLocked performs the runtime+DB work for restoring a sandbox. Caller
// runs on the supervisor inbox and has verified that status is still "snapshotted".
func (s *SandboxService) restoreLocked(ctx context.Context, orgID primitive.ObjectID, sandbox *model.Sandbox) (err error) {
	id := sandbox.ID.Hex()
	var claimedFrom string
	if !localSnapshotReady(sandbox, id) {
		claimedFrom, err = s.claimWake(ctx, sandbox)
		if err != nil {
			return err
		}
	}
	if claimedFrom != "" {
		defer func() {
			if err != nil {
				_, _ = s.repo.UpdateStatusFrom(context.Background(), sandbox.ID, "booting", claimedFrom)
			}
		}()
	}
	if err := s.supervisors.GetOrCreate(id).BeforeWake(ctx, sandbox); err != nil {
		return err
	}

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
		ID:                id,
		Type:              imageName,
		CPUs:              sandbox.CPU,
		MemoryMB:          sandbox.Mem,
		IPAddress:         sandbox.IP,
		TapName:           sandbox.TapName,
		MacAddress:        macAddr,
		NetNSName:         sandbox.NetNSName,
		ConsoleLogEnabled: sandbox.ConsoleLogEnabled,
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
	if err := attachOwned(s.supervisors.GetOrCreate(id), proc); err != nil {
		return fmt.Errorf("attach vm: %w", err)
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

	if _, err := s.repo.SetRunning(ctx, sandbox.ID, orgID); err != nil {
		cleanup()
		return fmt.Errorf("VM restored but failed to update DB status: %w", err)
	}
	s.supervisors.GetOrCreate(id).AfterWake(ctx, sandbox)
	s.emitLife(ctx, id, lifeFrom(sandbox, model.OpWake, model.PhaseCommitted, sandbox.Status, "running"))
	now := time.Now()
	sandbox.Status = "running"
	sandbox.LastActivityAt = &now
	s.syncAutoTimers(sandbox)

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

// EnsureRunning restores a snapshotted sandbox. The supervisor inbox serializes
// concurrent callers; status is re-read inside the inbox fn.
func (s *SandboxService) EnsureRunning(ctx context.Context, orgID primitive.ObjectID, id string) error {
	if err := s.waitIfBooting(ctx, orgID, id); err != nil {
		return err
	}
	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ensureRunningTimeout)
	defer cancel()
	a := s.supervisors.GetOrCreate(id)
	err := a.Restore(bgCtx, func() error {
		cur, err := s.getOrgScopedSandbox(bgCtx, orgID, id)
		if err != nil {
			return err
		}
		if cur.Status == "running" {
			s.adoptRunning(cur)
			return nil
		}
		if cur.Status != "snapshotted" && cur.Status != "archived" {
			return fmt.Errorf("sandbox in unexpected state for auto-restore: %s", cur.Status)
		}

		if err := a.BeforeBoot(bgCtx, cur.CPU, cur.Mem); err != nil {
			return err
		}

		log.Printf("[Auto-Restore] Sandbox %s is snapshotted, restoring...\n", id)
		if err := s.restoreLocked(bgCtx, orgID, cur); err != nil {
			a.AfterBootFailed(cur.CPU, cur.Mem)
			return fmt.Errorf("failed to auto-restore sandbox: %w", err)
		}
		a.AfterBoot(cur.CPU, cur.Mem)
		log.Printf("[Auto-Restore] Sandbox %s restored and ready\n", id)
		return nil
	})
	if errors.Is(err, ErrSandboxNotFound) {
		s.supervisors.Unregister(id)
	}
	return err
}

func (s *SandboxService) Info(id string) (string, error) {
	return runtime.Info(id)
}

// RefreshStatuses is a dead-supervisor sweeper. Sandboxes whose supervisor already
// supervises a process are skipped. After a host restart, VMs with no watcher
// yet are still polled (Has(id) is not enough — GetOrCreate without WatchPID
// must not hide a live VM from health).
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

		if s.supervisors.SupervisesProcess(id) {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer func() { <-sem; wg.Done() }()

			if s.supervisors.SupervisesProcess(id) {
				return
			}

			cur, err := s.repo.FindByID(ctx, sb.ID, options.FindOneOptions{})
			if err != nil || cur == nil {
				return
			}

			alive := sandboxVMRunning(id)

			switch cur.Status {
			case "running":
				if alive {
					s.adoptRunning(cur)
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
				now := time.Now()
				cur.Status = "running"
				cur.LastActivityAt = &now
				s.adoptRunning(cur)
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

// localSnapshotReady is true when wake can use files already on disk.
// The packer deletes those files only after archive, which requires packed,
// so this path skips the booting claim write.
func localSnapshotReady(sb *model.Sandbox, id string) bool {
	if sb == nil || sb.Status != "snapshotted" || sb.Packed || sb.ColdCleared {
		return false
	}
	return runtime.GetLatestSnapshotDir(id) != ""
}

// claimWake moves snapshotted or archived to booting before files are used.
// claimedFrom is set only when this call won, so a failure can revert that update.
func (s *SandboxService) claimWake(ctx context.Context, sandbox *model.Sandbox) (string, error) {
	if sandbox == nil || (sandbox.Status != "snapshotted" && sandbox.Status != "archived") {
		return "", nil
	}
	from := sandbox.Status
	ok, err := s.repo.ClaimForWake(ctx, sandbox.ID, sandbox.OrgID)
	if err != nil {
		return "", err
	}
	if ok {
		sandbox.Status = "booting"
		return from, nil
	}
	cur, err := s.getOrgScopedSandbox(ctx, sandbox.OrgID, sandbox.ID.Hex())
	if err != nil {
		return "", err
	}
	sandbox.Status = cur.Status
	sandbox.ColdCleared = cur.ColdCleared
	sandbox.ArchiveKey = cur.ArchiveKey
	sandbox.Packed = cur.Packed
	sandbox.PackPath = cur.PackPath
	return "", nil
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
	if a := s.supervisors.Get(id); a != nil && !a.NoteActivity(activityTouchGap) {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.repo.TouchActivity(bg, objID)
	s.resetIdleFromActivity(id)
}

// AdoptLocal attaches supervisors to this node's running sandboxes after a process restart.
// Live cloud-hypervisor pids are watched with pidfd (not Wait). Sleeping rows
// are left to the lifecycle sweeper.
func (s *SandboxService) AdoptLocal(ctx context.Context) error {
	if s == nil || s.repo == nil || s.supervisors == nil {
		return nil
	}
	hostID := ""
	if s.cfg != nil {
		hostID = s.cfg.HostID
	}
	sandboxes, err := s.repo.FindForHealth(ctx, hostID, options.FindOptions{})
	if err != nil {
		return fmt.Errorf("adopt local: %w", err)
	}

	n := 0
	for _, sb := range sandboxes {
		if sb == nil {
			continue
		}
		id := sb.ID.Hex()
		switch sb.Status {
		case "running", "booting":
			s.adoptRunning(sb)
			n++
		case "killed", "error":
			if _, ok := runtime.LiveCHPID(id); !ok {
				continue
			}
			from := sb.Status
			matched, uerr := s.repo.UpdateStatusFrom(ctx, sb.ID, from, "running")
			if uerr != nil {
				log.Printf("[adopt] %s status: %v", id, uerr)
				continue
			}
			if !matched {
				continue
			}
			s.emitLife(ctx, id, lifeFrom(sb, model.OpWake, model.PhaseCommitted, from, "running"))
			if err := s.repo.TouchActivity(ctx, sb.ID); err != nil {
				log.Printf("[adopt] %s touch: %v", id, err)
			}
			now := time.Now()
			sb.Status = "running"
			sb.LastActivityAt = &now
			s.adoptRunning(sb)
			if s.metrics != nil {
				s.metrics.RegisterSandbox(id, sb.Name, runtime.GetSocketPath(id), sb.CPU, sb.Mem, sb.DiskMB)
			}
			if s.monitor != nil {
				s.monitor.Start(ctx, sb.ID, sb.OrgID, sb.CreatedBy)
			}
			n++
		}
	}
	if n > 0 {
		log.Printf("[adopt] adopted %d local sandboxes", n)
	}
	return nil
}

// adoptRunning creates the supervisor, watches a live CLH pid if present, and arms
// idle timers from the Mongo row. Safe from health, Start, and EnsureRunning.
func (s *SandboxService) adoptRunning(sb *model.Sandbox) {
	if s == nil || s.supervisors == nil || sb == nil {
		return
	}
	id := sb.ID.Hex()
	a := s.supervisors.GetOrCreate(id)
	if pid, ok := runtime.LiveCHPID(id); ok {
		a.WatchPID(pid)
	}
	s.syncAutoTimers(sb)
}

// activityFresh reports whether last is still inside the idle window.
// The idle timer only wakes the supervisor; the row decides whether to snapshot.
func activityFresh(last *time.Time, idle time.Duration, now time.Time) bool {
	if last == nil || idle <= 0 {
		return false
	}
	return now.Sub(*last) < idle
}

func remainingSince(t *time.Time, sec int, now time.Time) time.Duration {
	if sec <= 0 {
		return -1
	}
	if t == nil {
		return 0
	}
	d := t.Add(time.Duration(sec) * time.Second).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

func (s *SandboxService) autoLifeOn() bool {
	return s != nil && s.cfg != nil && s.cfg.AutoLifecycle.Enabled
}

func (s *SandboxService) acquireAutoLife(ctx context.Context) error {
	if s == nil || s.autoLifeSem == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case s.autoLifeSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SandboxService) releaseAutoLife() {
	if s == nil || s.autoLifeSem == nil {
		return
	}
	select {
	case <-s.autoLifeSem:
	default:
	}
}

func (s *SandboxService) fireIdleSnapshot(id string, orgID primitive.ObjectID) func() {
	return func() {
		a := s.supervisors.Get(id)
		if a == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.acquireAutoLife(ctx); err != nil {
			s.rearmIdle(id, orgID)
			return
		}
		defer s.releaseAutoLife()
		if err := a.Snapshot(ctx, func() error {
			return s.snapshotIfStillIdle(ctx, orgID, id)
		}); err != nil {
			switch {
			case errors.Is(err, errIdleNotDue):
				s.rearmIdle(id, orgID)
				return
			case errors.Is(err, errAutoSleepOff), errors.Is(err, ErrSandboxNotFound), errors.Is(err, ErrSandboxNotRunning), errors.Is(err, errSupervisorStopped):
			default:
				log.Printf("[lifecycle] auto-snapshot failed for %s: %v", id, err)
				s.rearmIdle(id, orgID)
			}
			return
		}
		log.Printf("[lifecycle] auto-snapshotted sandbox %s after idle", id)
	}
}

func (s *SandboxService) rearmIdle(id string, orgID primitive.ObjectID) {
	if s == nil || !s.autoLifeOn() {
		return
	}
	a := s.supervisors.Get(id)
	if a == nil {
		return
	}
	_, autoSleep := a.AutoLifeMeta()
	if !autoSleep {
		return
	}
	sec := s.cfg.AutoLifecycle.SnapshotAfterIdleSec
	if sec <= 0 {
		return
	}
	a.SetIdleAfter(time.Duration(sec)*time.Second, s.fireIdleSnapshot(id, orgID))
}

// ApplySleep deletes a sleeping sandbox once its retention timer is due.
func (s *SandboxService) ApplySleep(ctx context.Context, orgID primitive.ObjectID, id string) error {
	if s == nil || s.supervisors == nil || s.cfg == nil {
		return nil
	}
	sb, err := s.getOrgScopedSandbox(ctx, orgID, id)
	if err != nil {
		return err
	}
	if dueSleepAction(sb, s.cfg.AutoLifecycle.DeleteAfterSnapshottedSec, 0, 0, false, time.Now()) != sleepDelete {
		return nil
	}
	a := s.supervisors.GetOrCreate(id)
	return a.Delete(ctx, func() error {
		cur, ferr := s.getOrgScopedSandbox(ctx, orgID, id)
		if ferr != nil {
			return ferr
		}
		if cur.Status != "snapshotted" && cur.Status != "archived" {
			return nil
		}
		return s.deleteLockedSandbox(ctx, orgID, id, cur)
	})
}

func (s *SandboxService) resetIdleFromActivity(id string) {
	if s == nil || s.supervisors == nil || !s.autoLifeOn() {
		return
	}
	a := s.supervisors.Get(id)
	if a == nil || !a.IdleArmed() {
		return
	}
	sec := s.cfg.AutoLifecycle.SnapshotAfterIdleSec
	if sec <= 0 {
		a.StopIdleTimer()
		return
	}
	orgID, _ := a.AutoLifeMeta()
	a.SetIdleAfter(time.Duration(sec)*time.Second, s.fireIdleSnapshot(id, orgID))
}

// syncAutoTimers arms or clears supervisor timers from the sandbox row.
// No-op if no supervisor exists — do not GetOrCreate here (health skips only
// SupervisesProcess; an unsupervised supervisor must still be visible to health).
func (s *SandboxService) syncAutoTimers(sb *model.Sandbox) {
	if s == nil || s.supervisors == nil || sb == nil {
		return
	}
	id := sb.ID.Hex()
	a := s.supervisors.Get(id)
	if a == nil {
		return
	}
	a.SetAutoLifeMeta(sb.OrgID, sb.AutoSleep)
	if !s.autoLifeOn() {
		a.StopIdleTimer()
		return
	}
	switch sb.Status {
	case "running":
		sec := s.cfg.AutoLifecycle.SnapshotAfterIdleSec
		if !sb.AutoSleep || sec <= 0 {
			a.StopIdleTimer()
			return
		}
		d := time.Duration(sec) * time.Second
		if sb.LastActivityAt != nil {
			d = remainingSince(sb.LastActivityAt, sec, time.Now())
		}
		a.SetIdleAfter(d, s.fireIdleSnapshot(id, sb.OrgID))
	default:
		a.StopIdleTimer()
	}
}
