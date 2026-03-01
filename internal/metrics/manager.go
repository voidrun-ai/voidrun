package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"voidrun/internal/config"
	"voidrun/internal/sandboxclient"
	"voidrun/pkg/machine"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gin-gonic/gin"
)

type Manager struct {
	interval     time.Duration
	diskInterval time.Duration
	concurrency  int
	host         string

	mu             sync.RWMutex
	vms            map[string]string
	sbxNames       map[string]string
	lastDisk       map[string]time.Time
	alloc          map[string]allocSpec
	allocVcpu      int64
	allocMemBytes  int64
	allocDiskBytes int64
	diskDevs       map[string]map[string]struct{}
	netDevs        map[string]map[string]struct{}
	once           sync.Once

	registry           *prometheus.Registry
	cpuUsageGauge      *prometheus.GaugeVec
	memUsedGauge       *prometheus.GaugeVec
	diskUsedGauge      *prometheus.GaugeVec
	scrapeUpGauge      *prometheus.GaugeVec
	scrapeTime         prometheus.Observer
	httpReqsTotal      *prometheus.CounterVec
	httpReqDur         *prometheus.HistogramVec
	diskReadBytes      *prometheus.GaugeVec
	diskWriteBytes     *prometheus.GaugeVec
	diskReadOps        *prometheus.GaugeVec
	diskWriteOps       *prometheus.GaugeVec
	diskReadLatMin     *prometheus.GaugeVec
	diskReadLatMax     *prometheus.GaugeVec
	diskReadLatAvg     *prometheus.GaugeVec
	diskWriteLatMin    *prometheus.GaugeVec
	diskWriteLatMax    *prometheus.GaugeVec
	diskWriteLatAvg    *prometheus.GaugeVec
	netRxBytes         *prometheus.GaugeVec
	netTxBytes         *prometheus.GaugeVec
	netRxFrames        *prometheus.GaugeVec
	netTxFrames        *prometheus.GaugeVec
	hostAllocVcpu      *prometheus.GaugeVec
	hostAllocMemBytes  *prometheus.GaugeVec
	hostAllocDiskBytes *prometheus.GaugeVec
}

type allocSpec struct {
	vcpu      int64
	memBytes  int64
	diskBytes int64
}

type countersResponse struct {
	CPU    *usageField `json:"cpu"`
	Memory *usageField `json:"memory"`
	Disks  map[string]diskCounters
	Nets   map[string]netCounters
}

type usageField struct {
	Usage float64 `json:"usage"`
}

type agentMetrics struct {
	CPUUsagePercent   float64 `json:"cpuUsagePercent"`
	MemTotalBytes     uint64  `json:"memTotalBytes"`
	MemAvailableBytes uint64  `json:"memAvailableBytes"`
	MemUsedBytes      uint64  `json:"memUsedBytes"`
	DiskTotalBytes    uint64  `json:"diskTotalBytes"`
	DiskFreeBytes     uint64  `json:"diskFreeBytes"`
	DiskUsedBytes     uint64  `json:"diskUsedBytes"`
	CPUError          string  `json:"cpuError"`
	MemError          string  `json:"memError"`
	DiskError         string  `json:"diskError"`
}

type diskCounters struct {
	ReadBytes       uint64  `json:"read_bytes"`
	WriteBytes      uint64  `json:"write_bytes"`
	ReadOps         uint64  `json:"read_ops"`
	WriteOps        uint64  `json:"write_ops"`
	ReadLatencyMin  float64 `json:"read_latency_min"`
	ReadLatencyMax  float64 `json:"read_latency_max"`
	ReadLatencyAvg  float64 `json:"read_latency_avg"`
	WriteLatencyMin float64 `json:"write_latency_min"`
	WriteLatencyMax float64 `json:"write_latency_max"`
	WriteLatencyAvg float64 `json:"write_latency_avg"`
}

type netCounters struct {
	RxBytes  uint64 `json:"rx_bytes"`
	TxBytes  uint64 `json:"tx_bytes"`
	RxFrames uint64 `json:"rx_frames"`
	TxFrames uint64 `json:"tx_frames"`
}

func NewManager(cfg config.MetricsConfig) *Manager {
	interval := time.Duration(cfg.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	diskInterval := time.Duration(cfg.DiskIntervalSec) * time.Second
	if diskInterval <= 0 {
		diskInterval = 60 * time.Second
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 16
	}
	hostname := "unknown"
	if host, err := os.Hostname(); err == nil && host != "" {
		hostname = host
	}

	registry := prometheus.NewRegistry()

	cpuUsage := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_cpu_usage",
			Help: "Sandbox CPU usage from guest agent",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host"},
	)
	memUsed := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_mem_used_bytes",
			Help: "Sandbox memory usage from guest agent",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host"},
	)
	scrapeUp := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_metrics_up",
			Help: "Whether vm.counters was scraped successfully (1 = up)",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host"},
	)
	diskUsed := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_used_bytes",
			Help: "Disk usage of the sandbox overlay on the host",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host"},
	)
	scrapeTime := prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "voidrun_sbx_scrape_duration_seconds",
			Help:    "Duration of vm.counters scrape and disk stat per sandbox",
			Buckets: prometheus.DefBuckets,
		},
	)
	httpReqs := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "voidrun_http_requests_total",
			Help: "Total HTTP requests handled by the VoidRun API",
		},
		[]string{"method", "path", "status", "voidrun_host"},
	)
	httpDur := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "voidrun_http_request_duration_seconds",
			Help:    "HTTP request latency for the VoidRun API",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "status", "voidrun_host"},
	)

	hostAllocVcpu := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_host_allocated_vcpus",
			Help: "Total allocated vCPUs across running sandboxes on this host",
		},
		[]string{"voidrun_host"},
	)
	hostAllocMemBytes := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_host_allocated_mem_bytes",
			Help: "Total allocated memory across running sandboxes on this host",
		},
		[]string{"voidrun_host"},
	)
	hostAllocDiskBytes := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_host_allocated_disk_bytes",
			Help: "Total allocated disk across running sandboxes on this host",
		},
		[]string{"voidrun_host"},
	)

	diskReadBytes := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_read_bytes",
			Help: "Disk read bytes from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskWriteBytes := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_write_bytes",
			Help: "Disk write bytes from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskReadOps := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_read_ops",
			Help: "Disk read ops from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskWriteOps := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_write_ops",
			Help: "Disk write ops from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskReadLatMin := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_read_latency_min",
			Help: "Disk read latency min (us) from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskReadLatMax := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_read_latency_max",
			Help: "Disk read latency max (us) from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskReadLatAvg := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_read_latency_avg",
			Help: "Disk read latency avg (us) from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskWriteLatMin := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_write_latency_min",
			Help: "Disk write latency min (us) from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskWriteLatMax := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_write_latency_max",
			Help: "Disk write latency max (us) from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	diskWriteLatAvg := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_disk_write_latency_avg",
			Help: "Disk write latency avg (us) from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)

	netRxBytes := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_net_rx_bytes",
			Help: "Network RX bytes from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	netTxBytes := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_net_tx_bytes",
			Help: "Network TX bytes from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	netRxFrames := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_net_rx_frames",
			Help: "Network RX frames from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)
	netTxFrames := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voidrun_sbx_net_tx_frames",
			Help: "Network TX frames from vm.counters",
		},
		[]string{"sbx_id", "sbx_name", "voidrun_host", "device"},
	)

	registry.MustRegister(
		cpuUsage,
		memUsed,
		scrapeUp,
		diskUsed,
		scrapeTime,
		httpReqs,
		httpDur,
		hostAllocVcpu,
		hostAllocMemBytes,
		hostAllocDiskBytes,
		diskReadBytes,
		diskWriteBytes,
		diskReadOps,
		diskWriteOps,
		diskReadLatMin,
		diskReadLatMax,
		diskReadLatAvg,
		diskWriteLatMin,
		diskWriteLatMax,
		diskWriteLatAvg,
		netRxBytes,
		netTxBytes,
		netRxFrames,
		netTxFrames,
	)
	registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	return &Manager{
		interval:           interval,
		diskInterval:       diskInterval,
		concurrency:        concurrency,
		host:               hostname,
		vms:                map[string]string{},
		sbxNames:           map[string]string{},
		lastDisk:           map[string]time.Time{},
		alloc:              map[string]allocSpec{},
		diskDevs:           map[string]map[string]struct{}{},
		netDevs:            map[string]map[string]struct{}{},
		registry:           registry,
		cpuUsageGauge:      cpuUsage,
		memUsedGauge:       memUsed,
		diskUsedGauge:      diskUsed,
		scrapeUpGauge:      scrapeUp,
		scrapeTime:         scrapeTime,
		httpReqsTotal:      httpReqs,
		httpReqDur:         httpDur,
		diskReadBytes:      diskReadBytes,
		diskWriteBytes:     diskWriteBytes,
		diskReadOps:        diskReadOps,
		diskWriteOps:       diskWriteOps,
		diskReadLatMin:     diskReadLatMin,
		diskReadLatMax:     diskReadLatMax,
		diskReadLatAvg:     diskReadLatAvg,
		diskWriteLatMin:    diskWriteLatMin,
		diskWriteLatMax:    diskWriteLatMax,
		diskWriteLatAvg:    diskWriteLatAvg,
		netRxBytes:         netRxBytes,
		netTxBytes:         netTxBytes,
		netRxFrames:        netRxFrames,
		netTxFrames:        netTxFrames,
		hostAllocVcpu:      hostAllocVcpu,
		hostAllocMemBytes:  hostAllocMemBytes,
		hostAllocDiskBytes: hostAllocDiskBytes,
	}
}

func (m *Manager) Start(ctx context.Context) {
	m.once.Do(func() {
		go m.loop(ctx)
	})
}

func (m *Manager) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Manager) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		method := c.Request.Method
		status := fmt.Sprintf("%d", c.Writer.Status())

		m.httpReqsTotal.WithLabelValues(method, path, status, m.host).Inc()
		m.httpReqDur.WithLabelValues(method, path, status, m.host).Observe(time.Since(start).Seconds())
	}
}

func (m *Manager) RegisterSandbox(vmID, sbxName, socketPath string, cpu, memMB, diskMB int) {
	if vmID == "" || socketPath == "" {
		return
	}
	if sbxName == "" {
		sbxName = "unknown"
	}
	vcpu := int64(cpu)
	if vcpu < 0 {
		vcpu = 0
	}
	memBytes := int64(memMB) * 1024 * 1024
	if memBytes < 0 {
		memBytes = 0
	}
	diskBytes := int64(diskMB) * 1024 * 1024
	if diskBytes < 0 {
		diskBytes = 0
	}
	m.mu.Lock()
	m.vms[vmID] = socketPath
	m.sbxNames[vmID] = sbxName
	if prev, ok := m.alloc[vmID]; ok {
		m.allocVcpu -= prev.vcpu
		m.allocMemBytes -= prev.memBytes
		m.allocDiskBytes -= prev.diskBytes
	}
	m.alloc[vmID] = allocSpec{vcpu: vcpu, memBytes: memBytes, diskBytes: diskBytes}
	m.allocVcpu += vcpu
	m.allocMemBytes += memBytes
	m.allocDiskBytes += diskBytes
	allocVcpu := m.allocVcpu
	allocMemBytes := m.allocMemBytes
	allocDiskBytes := m.allocDiskBytes
	m.mu.Unlock()

	m.hostAllocVcpu.WithLabelValues(m.host).Set(float64(allocVcpu))
	m.hostAllocMemBytes.WithLabelValues(m.host).Set(float64(allocMemBytes))
	m.hostAllocDiskBytes.WithLabelValues(m.host).Set(float64(allocDiskBytes))
}

func (m *Manager) UnregisterSandbox(vmID string) {
	if vmID == "" {
		return
	}
	labelID, labelName, labelHost := m.sandboxLabels(vmID)

	m.mu.Lock()
	delete(m.vms, vmID)
	delete(m.lastDisk, vmID)
	delete(m.diskDevs, vmID)
	delete(m.netDevs, vmID)
	delete(m.sbxNames, vmID)
	if prev, ok := m.alloc[vmID]; ok {
		m.allocVcpu -= prev.vcpu
		m.allocMemBytes -= prev.memBytes
		m.allocDiskBytes -= prev.diskBytes
		delete(m.alloc, vmID)
	}
	allocVcpu := m.allocVcpu
	allocMemBytes := m.allocMemBytes
	allocDiskBytes := m.allocDiskBytes
	m.mu.Unlock()

	m.hostAllocVcpu.WithLabelValues(m.host).Set(float64(allocVcpu))
	m.hostAllocMemBytes.WithLabelValues(m.host).Set(float64(allocMemBytes))
	m.hostAllocDiskBytes.WithLabelValues(m.host).Set(float64(allocDiskBytes))

	m.cpuUsageGauge.DeleteLabelValues(labelID, labelName, labelHost)
	m.memUsedGauge.DeleteLabelValues(labelID, labelName, labelHost)
	m.diskUsedGauge.DeleteLabelValues(labelID, labelName, labelHost)
	m.scrapeUpGauge.DeleteLabelValues(labelID, labelName, labelHost)
	m.deleteDeviceMetrics(vmID, labelID, labelName, labelHost)
}

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollOnce(ctx)
		}
	}
}

func (m *Manager) pollOnce(ctx context.Context) {
	vms := m.snapshotVMs()
	if len(vms) == 0 {
		return
	}

	sem := make(chan struct{}, m.concurrency)
	var wg sync.WaitGroup

	for vmID, socketPath := range vms {
		vmID := vmID
		socketPath := socketPath

		wg.Add(1)
		sem <- struct{}{}

		go func() {
			start := time.Now()
			defer func() {
				m.scrapeTime.Observe(time.Since(start).Seconds())
			}()
			defer func() {
				<-sem
				wg.Done()
			}()

			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			labelID, labelName, labelHost := m.sandboxLabels(vmID)
			up := false

			if stats, err := fetchAgentMetrics(ctx, vmID); err == nil {
				up = true
				if stats.CPUError == "" {
					m.cpuUsageGauge.WithLabelValues(labelID, labelName, labelHost).Set(stats.CPUUsagePercent)
				}
				if stats.MemError == "" {
					m.memUsedGauge.WithLabelValues(labelID, labelName, labelHost).Set(float64(stats.MemUsedBytes))
				}
			}

			stats, err := fetchCounters(ctx, socketPath)
			if err == nil {
				up = true
				if stats.CPU != nil {
					m.cpuUsageGauge.WithLabelValues(labelID, labelName, labelHost).Set(stats.CPU.Usage)
				}
				if stats.Memory != nil {
					m.memUsedGauge.WithLabelValues(labelID, labelName, labelHost).Set(stats.Memory.Usage)
				}
				for device, disk := range stats.Disks {
					m.trackDiskDevice(vmID, device)
					m.diskReadBytes.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(disk.ReadBytes))
					m.diskWriteBytes.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(disk.WriteBytes))
					m.diskReadOps.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(disk.ReadOps))
					m.diskWriteOps.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(disk.WriteOps))
					m.diskReadLatMin.WithLabelValues(labelID, labelName, labelHost, device).Set(disk.ReadLatencyMin)
					m.diskReadLatMax.WithLabelValues(labelID, labelName, labelHost, device).Set(disk.ReadLatencyMax)
					m.diskReadLatAvg.WithLabelValues(labelID, labelName, labelHost, device).Set(disk.ReadLatencyAvg)
					m.diskWriteLatMin.WithLabelValues(labelID, labelName, labelHost, device).Set(disk.WriteLatencyMin)
					m.diskWriteLatMax.WithLabelValues(labelID, labelName, labelHost, device).Set(disk.WriteLatencyMax)
					m.diskWriteLatAvg.WithLabelValues(labelID, labelName, labelHost, device).Set(disk.WriteLatencyAvg)
				}
				for device, netc := range stats.Nets {
					m.trackNetDevice(vmID, device)
					m.netRxBytes.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(netc.RxBytes))
					m.netTxBytes.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(netc.TxBytes))
					m.netRxFrames.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(netc.RxFrames))
					m.netTxFrames.WithLabelValues(labelID, labelName, labelHost, device).Set(float64(netc.TxFrames))
				}
			}

			if m.shouldScrapeDisk(vmID) {
				if sizeBytes, err := overlaySizeBytes(socketPath); err == nil {
					m.diskUsedGauge.WithLabelValues(labelID, labelName, labelHost).Set(float64(sizeBytes))
				}
				m.markDiskScraped(vmID)
			}

			if up {
				m.scrapeUpGauge.WithLabelValues(labelID, labelName, labelHost).Set(1)
			} else {
				m.scrapeUpGauge.WithLabelValues(labelID, labelName, labelHost).Set(0)
			}
		}()
	}

	wg.Wait()
}

func (m *Manager) snapshotVMs() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	copyMap := make(map[string]string, len(m.vms))
	for k, v := range m.vms {
		copyMap[k] = v
	}
	return copyMap
}

func (m *Manager) sandboxLabels(vmID string) (string, string, string) {
	m.mu.RLock()
	name := m.sbxNames[vmID]
	m.mu.RUnlock()
	if name == "" {
		name = "unknown"
	}
	return vmID, name, m.host
}

func fetchCounters(ctx context.Context, socketPath string) (*countersResponse, error) {
	body, status, err := unixGet(ctx, socketPath, "/vm.counters")
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		body, status, err = unixGet(ctx, socketPath, "/api/v1/vm.counters")
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("vm.counters status %d", status)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode vm.counters: %w", err)
	}

	resp := &countersResponse{
		Disks: map[string]diskCounters{},
		Nets:  map[string]netCounters{},
	}
	if cpuRaw, ok := raw["cpu"]; ok {
		var cpu usageField
		if err := json.Unmarshal(cpuRaw, &cpu); err == nil {
			resp.CPU = &cpu
		}
	}
	if memRaw, ok := raw["memory"]; ok {
		var mem usageField
		if err := json.Unmarshal(memRaw, &mem); err == nil {
			resp.Memory = &mem
		}
	}
	for key, payload := range raw {
		switch {
		case strings.HasPrefix(key, "_disk"):
			var disk diskCounters
			if err := json.Unmarshal(payload, &disk); err == nil {
				resp.Disks[key] = disk
			}
		case strings.HasPrefix(key, "_net"):
			var netc netCounters
			if err := json.Unmarshal(payload, &netc); err == nil {
				resp.Nets[key] = netc
			}
		}
	}

	return resp, nil
}

func fetchAgentMetrics(ctx context.Context, sbxID string) (*agentMetrics, error) {
	client := sandboxclient.GetSandboxHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+sbxID+"/metrics", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent metrics status %d", resp.StatusCode)
	}

	var metrics agentMetrics
	if err := json.Unmarshal(body, &metrics); err != nil {
		return nil, fmt.Errorf("decode agent metrics: %w", err)
	}

	return &metrics, nil
}

func unixGet(ctx context.Context, socketPath, urlPath string) ([]byte, int, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		DisableKeepAlives: true,
	}

	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+urlPath, nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return body, resp.StatusCode, nil
}

func (m *Manager) shouldScrapeDisk(vmID string) bool {
	if m.diskInterval == 0 {
		return false
	}
	if m.diskInterval <= m.interval {
		return true
	}

	now := time.Now()
	m.mu.RLock()
	last := m.lastDisk[vmID]
	m.mu.RUnlock()

	return now.Sub(last) >= m.diskInterval
}

func (m *Manager) markDiskScraped(vmID string) {
	m.mu.Lock()
	m.lastDisk[vmID] = time.Now()
	m.mu.Unlock()
}

func overlaySizeBytes(socketPath string) (int64, error) {
	// Extract sandbox ID from socket path to use centralized helper
	instanceDir := filepath.Dir(socketPath)
	sbxID := filepath.Base(instanceDir)
	path := machine.GetOverlayPath(sbxID)

	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (m *Manager) trackDiskDevice(vmID, device string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.diskDevs[vmID]
	if !ok {
		set = map[string]struct{}{}
		m.diskDevs[vmID] = set
	}
	set[device] = struct{}{}
}

func (m *Manager) trackNetDevice(vmID, device string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.netDevs[vmID]
	if !ok {
		set = map[string]struct{}{}
		m.netDevs[vmID] = set
	}
	set[device] = struct{}{}
}

func (m *Manager) deleteDeviceMetrics(vmID, labelID, labelName, labelHost string) {
	m.mu.RLock()
	disks := m.diskDevs[vmID]
	nets := m.netDevs[vmID]
	m.mu.RUnlock()

	for device := range disks {
		m.diskReadBytes.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskWriteBytes.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskReadOps.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskWriteOps.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskReadLatMin.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskReadLatMax.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskReadLatAvg.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskWriteLatMin.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskWriteLatMax.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.diskWriteLatAvg.DeleteLabelValues(labelID, labelName, labelHost, device)
	}
	for device := range nets {
		m.netRxBytes.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.netTxBytes.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.netRxFrames.DeleteLabelValues(labelID, labelName, labelHost, device)
		m.netTxFrames.DeleteLabelValues(labelID, labelName, labelHost, device)
	}
}
