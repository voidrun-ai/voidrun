package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"voidrun/config"
)

func TestNormalizeCPUUsagePercent(t *testing.T) {
	tests := []struct {
		name  string
		usage float64
		want  float64
	}{
		{"agent percent", 42.5, 42.5},
		{"ch fraction", 0.15, 15},
		{"ch percent", 15, 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeCPUUsagePercent(tt.usage); got != tt.want {
				t.Fatalf("normalizeCPUUsagePercent(%v) = %v, want %v", tt.usage, got, tt.want)
			}
		})
	}
}

func TestScrapeDurationHistogramPerSandboxLabels(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10}, nil)
	m.scrapeTime.WithLabelValues("sbx-1", "stress-test", "test-host").Observe(0.05)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rr, req)

	body := rr.Body.String()
	want := `voidrun_sbx_scrape_duration_seconds_bucket{sbx_id="sbx-1",sbx_name="stress-test",voidrun_host="test-host"`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing labeled scrape histogram; want substring %q", want)
	}
}

func TestUnregisterSandboxDeletesScrapeDurationLabels(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10}, nil)
	m.RegisterSandbox("vm-1", "my-sbx", "/tmp/sock", "cloud-hypervisor", 1, 512, 1024)
	m.scrapeTime.WithLabelValues("vm-1", "my-sbx", m.host).Observe(0.02)

	m.UnregisterSandbox("vm-1")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rr, req)

	if strings.Contains(rr.Body.String(), `sbx_name="my-sbx"`) &&
		strings.Contains(rr.Body.String(), "voidrun_sbx_scrape_duration_seconds_bucket") {
		t.Fatal("expected scrape duration series removed after unregister")
	}
}
