package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
	"voidrun/pkg/timer"
)

// Cloud Hypervisor API endpoint constants
const (
	// VMM endpoints
	EndpointVmmPing     = "vmm.ping"
	EndpointVmmShutdown = "vmm.shutdown"

	// VM lifecycle endpoints
	EndpointVmCreate      = "vm.create"
	EndpointVmDelete      = "vm.delete"
	EndpointVmBoot        = "vm.boot"
	EndpointVmShutdown    = "vm.shutdown"
	EndpointVmReboot      = "vm.reboot"
	EndpointVmPowerButton = "vm.power-button"
	EndpointVmPause       = "vm.pause"
	EndpointVmResume      = "vm.resume"

	// VM information endpoints
	EndpointVmInfo     = "vm.info"
	EndpointVmCounters = "vm.counters"

	// VM resize endpoints
	EndpointVmResize     = "vm.resize"
	EndpointVmResizeDisk = "vm.resize-disk"
	EndpointVmResizeZone = "vm.resize-zone"

	// Device hotplug endpoints
	EndpointVmAddDevice     = "vm.add-device"
	EndpointVmAddDisk       = "vm.add-disk"
	EndpointVmAddFs         = "vm.add-fs"
	EndpointVmAddPmem       = "vm.add-pmem"
	EndpointVmAddNet        = "vm.add-net"
	EndpointVmAddUserDevice = "vm.add-user-device"
	EndpointVmAddVdpa       = "vm.add-vdpa"
	EndpointVmAddVsock      = "vm.add-vsock"
	EndpointVmRemoveDevice  = "vm.remove-device"

	// Snapshot and migration endpoints
	EndpointVmSnapshot         = "vm.snapshot"
	EndpointVmCoredump         = "vm.coredump"
	EndpointVmRestore          = "vm.restore"
	EndpointVmReceiveMigration = "vm.receive-migration"
	EndpointVmSendMigration    = "vm.send-migration"

	// Debug endpoints
	EndpointVmNmi = "vm.nmi"
)

// CLHClient is a comprehensive wrapper for Cloud Hypervisor REST API
// Reference: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/api.md
type CLHClient struct {
	socketPath string
	timeout    time.Duration
	httpClient *http.Client
}

// NewCLHClient creates a new Cloud Hypervisor API client
func NewCLHClient(socketPath string) *CLHClient {
	return NewCLHClientWithTimeout(socketPath, 5*time.Second)
}

// NewCLHClientWithTimeout creates a new client with custom timeout
func NewCLHClientWithTimeout(socketPath string, timeout time.Duration) *CLHClient {
	client := &CLHClient{
		socketPath: socketPath,
		timeout:    timeout,
	}
	client.httpClient = client.createHTTPClient()
	return client
}

// NewCLHClientForSandbox creates a client for a specific sandbox
func NewCLHClientForSandbox(sandboxID string) *CLHClient {
	return NewCLHClient(GetSocketPath(sandboxID))
}

// createHTTPClient creates an HTTP client configured for Unix socket
func (c *CLHClient) createHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.socketPath)
			},
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		Timeout: c.timeout,
	}
}

// ============================================================================
// Low-level Request Methods
// ============================================================================

// do performs an HTTP request with the specified method
func (c *CLHClient) do(ctx context.Context, method, endpoint string, body interface{}) ([]byte, error) {
	// Use string concatenation instead of fmt.Sprintf for simple case (micro-optimization)
	url := "http://localhost/api/v1/" + endpoint

	var reqBody io.Reader
	var hasBody bool
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
		hasBody = true
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers in one pass
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		// Avoid byte-to-string conversion by using %s with byte slice directly
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, respBody)
	}

	return respBody, nil
}

// put performs a PUT request
func (c *CLHClient) put(ctx context.Context, endpoint string, body interface{}) ([]byte, error) {
	return c.do(ctx, http.MethodPut, endpoint, body)
}

// get performs a GET request
func (c *CLHClient) get(ctx context.Context, endpoint string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, endpoint, nil)
}

// ============================================================================
// VMM Operations
// ============================================================================

// VmmPing checks if the Cloud Hypervisor API is available
func (c *CLHClient) VmmPing(ctx context.Context) (*VmmPingResponse, error) {
	body, err := c.get(ctx, EndpointVmmPing)
	if err != nil {
		return nil, err
	}

	var resp VmmPingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &resp, nil
}

// VmmShutdown shuts down the VMM (Cloud Hypervisor process)
func (c *CLHClient) VmmShutdown(ctx context.Context) error {
	_, err := c.put(ctx, EndpointVmmShutdown, nil)
	return err
}

// ============================================================================
// VM Lifecycle Operations
// ============================================================================

// VmCreate creates a new VM with the given configuration
func (c *CLHClient) VmCreate(ctx context.Context, config *VmConfig) error {
	_, err := c.put(ctx, EndpointVmCreate, config)
	return err
}

// VmDelete deletes the VM
func (c *CLHClient) VmDelete(ctx context.Context) error {
	_, err := c.put(ctx, EndpointVmDelete, nil)
	return err
}

// VmBoot boots the VM
func (c *CLHClient) VmBoot(ctx context.Context) error {
	defer timer.Track("CLH API: VmBoot")()
	_, err := c.put(ctx, EndpointVmBoot, nil)
	return err
}

// VmShutdown gracefully shuts down the VM
func (c *CLHClient) VmShutdown(ctx context.Context) error {
	defer timer.Track("CLH API: VmShutdown")()
	_, err := c.put(ctx, EndpointVmShutdown, nil)
	return err
}

// VmReboot reboots the VM
func (c *CLHClient) VmReboot(ctx context.Context) error {
	_, err := c.put(ctx, EndpointVmReboot, nil)
	return err
}

// VmPowerButton triggers the power button (ACPI)
func (c *CLHClient) VmPowerButton(ctx context.Context) error {
	_, err := c.put(ctx, EndpointVmPowerButton, nil)
	return err
}

// VmPause pauses the VM
func (c *CLHClient) VmPause(ctx context.Context) error {
	_, err := c.put(ctx, EndpointVmPause, nil)
	return err
}

// VmResume resumes a paused VM
func (c *CLHClient) VmResume(ctx context.Context) error {
	_, err := c.put(ctx, EndpointVmResume, nil)
	return err
}

// ============================================================================
// VM Information & Monitoring
// ============================================================================

// VmInfo retrieves detailed information about the VM
func (c *CLHClient) VmInfo(ctx context.Context) (*VmInfo, error) {
	body, err := c.get(ctx, EndpointVmInfo)
	if err != nil {
		return nil, err
	}

	var info VmInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse vm info: %w", err)
	}

	return &info, nil
}

// VmCounters retrieves VM performance counters
func (c *CLHClient) VmCounters(ctx context.Context) (*VmCounters, error) {
	body, err := c.get(ctx, EndpointVmCounters)
	if err != nil {
		return nil, err
	}

	var counters VmCounters
	if err := json.Unmarshal(body, &counters); err != nil {
		return nil, fmt.Errorf("failed to parse counters: %w", err)
	}

	return &counters, nil
}

// GetState returns the current VM state as a string
func (c *CLHClient) GetState(ctx context.Context) (string, error) {
	info, err := c.VmInfo(ctx)
	if err != nil {
		return "", err
	}
	return info.State, nil
}

// ============================================================================
// VM Resizing Operations
// ============================================================================

// VmResize resizes VM resources (CPU, memory, balloon)
func (c *CLHClient) VmResize(ctx context.Context, resize *VmResize) error {
	_, err := c.put(ctx, EndpointVmResize, resize)
	return err
}

// VmResizeCPU resizes the number of vCPUs
func (c *CLHClient) VmResizeCPU(ctx context.Context, desiredVcpus int) error {
	return c.VmResize(ctx, &VmResize{DesiredVcpus: &desiredVcpus})
}

// VmResizeMemory resizes the VM memory
func (c *CLHClient) VmResizeMemory(ctx context.Context, desiredRam int64) error {
	return c.VmResize(ctx, &VmResize{DesiredRam: &desiredRam})
}

// VmResizeBalloon resizes the balloon device
func (c *CLHClient) VmResizeBalloon(ctx context.Context, desiredBalloon int64) error {
	return c.VmResize(ctx, &VmResize{DesiredBalloon: &desiredBalloon})
}

// VmResizeDisk resizes a specific disk
func (c *CLHClient) VmResizeDisk(ctx context.Context, diskID string, newSize int64) error {
	req := &VmResizeDisk{
		DiskID:  diskID,
		NewSize: newSize,
	}
	_, err := c.put(ctx, EndpointVmResizeDisk, req)
	return err
}

// VmResizeZone resizes a memory zone
func (c *CLHClient) VmResizeZone(ctx context.Context, zoneID string, desiredRam int64) error {
	req := &VmResizeZone{
		ID:         zoneID,
		DesiredRam: desiredRam,
	}
	_, err := c.put(ctx, EndpointVmResizeZone, req)
	return err
}

// ============================================================================
// Device Hotplug Operations
// ============================================================================

// VmAddDevice adds a VFIO device to the running VM
func (c *CLHClient) VmAddDevice(ctx context.Context, device *DeviceConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddDevice, device)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmAddDisk adds a disk device to the running VM
func (c *CLHClient) VmAddDisk(ctx context.Context, disk *DiskConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddDisk, disk)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmAddFs adds a virtio-fs device to the running VM
func (c *CLHClient) VmAddFs(ctx context.Context, fs *FsConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddFs, fs)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmAddPmem adds a pmem device to the running VM
func (c *CLHClient) VmAddPmem(ctx context.Context, pmem *PmemConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddPmem, pmem)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmAddNet adds a network device to the running VM
func (c *CLHClient) VmAddNet(ctx context.Context, net *NetConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddNet, net)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmAddUserDevice adds a userspace device to the running VM
func (c *CLHClient) VmAddUserDevice(ctx context.Context, device *UserDeviceConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddUserDevice, device)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmAddVdpa adds a vDPA device to the running VM
func (c *CLHClient) VmAddVdpa(ctx context.Context, vdpa *VdpaConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddVdpa, vdpa)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmAddVsock adds a vsock device to the running VM
func (c *CLHClient) VmAddVsock(ctx context.Context, vsock *VsockConfig) (*PciDeviceInfo, error) {
	body, err := c.put(ctx, EndpointVmAddVsock, vsock)
	if err != nil {
		return nil, err
	}

	var info PciDeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse device info: %w", err)
	}

	return &info, nil
}

// VmRemoveDevice removes a device from the running VM
func (c *CLHClient) VmRemoveDevice(ctx context.Context, deviceID string) error {
	req := &VmRemoveDevice{ID: deviceID}
	_, err := c.put(ctx, EndpointVmRemoveDevice, req)
	return err
}

// ============================================================================
// Snapshot & Migration Operations
// ============================================================================

// VmSnapshot creates a snapshot of the VM
func (c *CLHClient) VmSnapshot(ctx context.Context, destinationURL string) error {
	req := &VmSnapshotConfig{DestinationURL: destinationURL}
	_, err := c.put(ctx, EndpointVmSnapshot, req)
	return err
}

// VmCoredump creates a coredump of the VM (x86_64 only, requires guest_debug feature)
func (c *CLHClient) VmCoredump(ctx context.Context, destinationURL string) error {
	req := &VmCoredumpData{DestinationURL: destinationURL}
	_, err := c.put(ctx, EndpointVmCoredump, req)
	return err
}

// VmRestore restores a VM from a snapshot
func (c *CLHClient) VmRestore(ctx context.Context, config *RestoreConfig) error {
	_, err := c.put(ctx, EndpointVmRestore, config)
	return err
}

// VmReceiveMigration prepares the VM to receive a migration
func (c *CLHClient) VmReceiveMigration(ctx context.Context, receiverURL string) error {
	req := &ReceiveMigrationData{ReceiverURL: receiverURL}
	_, err := c.put(ctx, EndpointVmReceiveMigration, req)
	return err
}

// VmSendMigration starts sending the VM state to a migration target
func (c *CLHClient) VmSendMigration(ctx context.Context, destinationURL string, local bool) error {
	req := &SendMigrationData{
		DestinationURL: destinationURL,
		Local:          local,
	}
	_, err := c.put(ctx, EndpointVmSendMigration, req)
	return err
}

// ============================================================================
// Debug & Advanced Operations
// ============================================================================

// VmNMI injects a Non-Maskable Interrupt into the VM
func (c *CLHClient) VmNMI(ctx context.Context) error {
	_, err := c.put(ctx, EndpointVmNmi, nil)
	return err
}

// ============================================================================
// Helper Methods
// ============================================================================

// IsSocketAvailable checks if the socket exists
func (c *CLHClient) IsSocketAvailable() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitForSocket waits for the socket to become available
func (c *CLHClient) WaitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.IsSocketAvailable() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("socket not available after %v", timeout)
}

// Ping is a convenience method that checks if the API is responsive
func (c *CLHClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.VmmPing(ctx)
	return err
}
