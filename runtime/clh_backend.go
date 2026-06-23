package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"voidrun/config"
	"voidrun/model"
)

// CLHBackend implements Hypervisor for Cloud Hypervisor.
//
// The implementation reuses the existing top-level helper functions in this
// package (Create, CreateCLI, Stop, Start, Pause, Resume, Delete, Info,
// path helpers, CLH HTTP clients). It also provides the CLH-specific
// EventSource and Counters adapters.
type CLHBackend struct {
	cfg *config.Config
}

// NewCLHBackend constructs the Cloud Hypervisor backend.
func NewCLHBackend(cfg *config.Config) *CLHBackend {
	return &CLHBackend{cfg: cfg}
}

func (b *CLHBackend) Name() string { return string(HypervisorCloudHypervisor) }

func (b *CLHBackend) Capabilities() Capabilities {
	return Capabilities{
		SupportsHotplugDisk:    true,
		SupportsHotplugNetwork: true,
		SupportsSnapshot:       true,
		SupportsCoreDump:       true,
		SupportsQcow2Disks:     true,
		SupportsCtrlAltDel:     true,
	}
}

// Boot spawns the CLH process for a fresh sandbox. We use the CLI mode
// (CreateCLI) which applies landlock rules.
//
// Cold restarts re-use the existing instance directory, so any stale
// management/vsock socket files from a previous (killed) CH process must be
// removed first or CH will fail with "Address in use" when binding the API
// socket. The previous owner is presumed dead by the time we get here.
func (b *CLHBackend) Boot(ctx context.Context, cfg config.Config, spec model.SandboxSpec, overlayPath string) error {
	_ = os.Remove(GetSocketPath(spec.ID))
	_ = os.Remove(GetVsockPath(spec.ID))
	return CreateCLI(cfg, spec, overlayPath)
}

// Start triggers vm.boot on an already-running hypervisor process.
func (b *CLHBackend) Start(ctx context.Context, id string) error {
	return Start(id)
}

func (b *CLHBackend) Stop(ctx context.Context, id string) error {
	return Stop(id)
}

func (b *CLHBackend) Pause(ctx context.Context, id string) error {
	return Pause(id)
}

func (b *CLHBackend) Resume(ctx context.Context, id string) error {
	return Resume(id)
}

func (b *CLHBackend) Delete(ctx context.Context, id, tapName, nsName string) error {
	return Delete(id, tapName, nsName)
}

// State maps CLH-native states ("Running", "Paused", "Loaded", ...) to the
// normalised set surfaced to the rest of the application.
func (b *CLHBackend) State(ctx context.Context, id string) (NormalizedState, error) {
	client := NewAPIClientForSandbox(id)
	if !client.IsSocketAvailable() {
		return StateStopped, nil
	}
	native, err := client.GetStateWithContext(ctx)
	if err != nil {
		// Socket exists but the API refuses — treat as zombie.
		return StateKilled, err
	}
	switch strings.ToLower(strings.TrimSpace(native)) {
	case "running", "runningvirtualized":
		return StateRunning, nil
	case "paused":
		return StatePaused, nil
	case "created":
		return StateCreated, nil
	case "loaded", "shutdown":
		return StateStopped, nil
	default:
		return StateUnknown, nil
	}
}

func (b *CLHBackend) Info(ctx context.Context, id string) (string, error) {
	return Info(id)
}

func (b *CLHBackend) IsSocketAvailable(id string) bool {
	return NewCLHClientForSandbox(id).IsSocketAvailable()
}

// EventSource returns a CLH event file tailer.
func (b *CLHBackend) EventSource(id string) EventSource {
	return &clhEventSource{id: id}
}

// Counters fetches /vm.counters from the CLH API socket and normalises the
// response into a CountersSnapshot.
func (b *CLHBackend) Counters(ctx context.Context, id string) (*CountersSnapshot, error) {
	socketPath := GetSocketPath(id)
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

	snap := &CountersSnapshot{
		Disks: map[string]DiskCountersSnapshot{},
		Nets:  map[string]NetCountersSnapshot{},
	}

	if cpuRaw, ok := raw["cpu"]; ok {
		var v struct {
			Usage float64 `json:"usage"`
		}
		if err := json.Unmarshal(cpuRaw, &v); err == nil {
			snap.CPUUsage = v.Usage
		}
	}
	if memRaw, ok := raw["memory"]; ok {
		var v struct {
			Usage float64 `json:"usage"`
		}
		if err := json.Unmarshal(memRaw, &v); err == nil {
			snap.MemoryUsedBytes = uint64(v.Usage)
		}
	}

	for key, payload := range raw {
		switch {
		case strings.HasPrefix(key, "_disk"):
			var d struct {
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
			if err := json.Unmarshal(payload, &d); err == nil {
				snap.Disks[key] = DiskCountersSnapshot{
					ReadBytes:       d.ReadBytes,
					WriteBytes:      d.WriteBytes,
					ReadOps:         d.ReadOps,
					WriteOps:        d.WriteOps,
					ReadLatencyMin:  d.ReadLatencyMin,
					ReadLatencyMax:  d.ReadLatencyMax,
					ReadLatencyAvg:  d.ReadLatencyAvg,
					WriteLatencyMin: d.WriteLatencyMin,
					WriteLatencyMax: d.WriteLatencyMax,
					WriteLatencyAvg: d.WriteLatencyAvg,
				}
			}
		case strings.HasPrefix(key, "_net"):
			var n struct {
				RxBytes  uint64 `json:"rx_bytes"`
				TxBytes  uint64 `json:"tx_bytes"`
				RxFrames uint64 `json:"rx_frames"`
				TxFrames uint64 `json:"tx_frames"`
			}
			if err := json.Unmarshal(payload, &n); err == nil {
				snap.Nets[key] = NetCountersSnapshot{
					RxBytes:  n.RxBytes,
					TxBytes:  n.TxBytes,
					RxFrames: n.RxFrames,
					TxFrames: n.TxFrames,
				}
			}
		}
	}
	return snap, nil
}

// clhEventSource tails the CLH --event-monitor JSONL file.
type clhEventSource struct {
	id string
}

func (s *clhEventSource) Source() string     { return "clh" }
func (s *clhEventSource) OffsetPath() string { return GetEventOffsetPath(s.id) }

func (s *clhEventSource) Poll(ctx context.Context, offset int64) ([]EventRecord, int64, error) {
	path := GetEventPath(s.id)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, fmt.Errorf("open event file: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seek to %d: %w", offset, err)
	}

	decoder := json.NewDecoder(f)
	var out []EventRecord
	for {
		var payload struct {
			Timestamp struct {
				Secs  int64 `json:"secs"`
				Nanos int64 `json:"nanos"`
			} `json:"timestamp"`
			Source     string `json:"source"`
			Event      string `json:"event"`
			Properties any    `json:"properties,omitempty"`
		}
		if err := decoder.Decode(&payload); err == io.EOF {
			break
		} else if err != nil {
			log.Printf("[event_monitor] CLH decode error for %s at %d: %v",
				s.id, offset+decoder.InputOffset(), err)
			break
		}
		uptime := time.Duration(payload.Timestamp.Secs)*time.Second +
			time.Duration(payload.Timestamp.Nanos)*time.Nanosecond

		props := map[string]any{}
		if payload.Properties != nil {
			if m, ok := payload.Properties.(map[string]any); ok {
				for k, v := range m {
					props[k] = v
				}
			}
		}
		out = append(out, EventRecord{
			Event:      payload.Event,
			UptimeNs:   uptime.Nanoseconds(),
			Properties: props,
		})
	}
	return out, offset + decoder.InputOffset(), nil
}

// unixGet performs an HTTP GET over a Unix domain socket. Used by the CLH
// backend's Counters implementation.
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
