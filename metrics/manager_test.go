package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"voidrun/config"

	"github.com/gin-gonic/gin"
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

func TestDefaultDiskIntervalMatchesGuestScrape(t *testing.T) {
	m := NewManager(config.MetricsConfig{})
	if m.diskInterval != m.interval {
		t.Fatalf("diskInterval=%v interval=%v; disk should scrape every guest poll by default", m.diskInterval, m.interval)
	}
	if !m.shouldScrapeDisk("any") {
		t.Fatal("shouldScrapeDisk must be true every tick when diskInterval <= interval")
	}
}

func TestConfigDefaultDiskIntervalSecMatchesMetrics(t *testing.T) {
	if config.DefaultMetricsDiskIntervalSec != config.DefaultMetricsIntervalSec {
		t.Fatalf("DefaultMetricsDiskIntervalSec=%d DefaultMetricsIntervalSec=%d",
			config.DefaultMetricsDiskIntervalSec, config.DefaultMetricsIntervalSec)
	}
}

func TestScrapeDurationHistogramPerSandboxLabels(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
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
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.RegisterSandbox("vm-1", "my-sbx", "/tmp/sock", 1, 512, 1024)
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

func TestRegisterSandboxExposesDiskLimitBytes(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.RegisterSandbox("sbx-1", "demo", "/tmp/sock", 1, 512, 1024)

	body := scrapeMetrics(t, m)
	want := `voidrun_sbx_disk_limit_bytes{sbx_id="sbx-1",sbx_name="demo",voidrun_host="` + m.host + `"} 1.073741824e+09`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing disk limit gauge %q\n%s", want, body)
	}
}

func TestUnregisterSandboxRemovesDiskLimitBytes(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.RegisterSandbox("sbx-1", "demo", "/tmp/sock", 1, 512, 1024)
	m.UnregisterSandbox("sbx-1")

	body := scrapeMetrics(t, m)
	if strings.Contains(body, "voidrun_sbx_disk_limit_bytes") && strings.Contains(body, `sbx_id="sbx-1"`) {
		t.Fatal("expected disk limit series removed after unregister")
	}
}

func TestClassifySandboxOperation(t *testing.T) {
	tests := []struct {
		method    string
		path      string
		category  string
		operation string
		ok        bool
	}{
		// Lifecycle is recorded in the service layer (covers auto-sleep + activator).
		{http.MethodDelete, "/api/sandboxes/:id", "", "", false},
		{http.MethodPost, "/api/sandboxes/:id/sleep", "", "", false},
		{http.MethodPost, "/api/sandboxes/:id/wake", "", "", false},
		{http.MethodPost, "/api/sandboxes/:id/start", "", "", false},
		{http.MethodPost, "/api/sandboxes/:id/exec", "command", "exec", true},
		{http.MethodPost, "/api/sandboxes/:id/exec-stream", "command", "exec_stream", true},
		{http.MethodPost, "/api/sandboxes/:id/session-exec", "command", "session_exec", true},
		{http.MethodPost, "/api/sandboxes/:id/session-exec-stream", "command", "session_exec_stream", true},
		{http.MethodPost, "/api/sandboxes/:id/commands/run", "command", "run", true},
		{http.MethodGet, "/api/sandboxes/:id/commands/list", "command", "list", true},
		{http.MethodPost, "/api/sandboxes/:id/commands/kill", "command", "kill", true},
		{http.MethodPost, "/api/sandboxes/:id/commands/attach", "command", "attach", true},
		{http.MethodPost, "/api/sandboxes/:id/commands/wait", "command", "wait", true},
		{http.MethodGet, "/api/sandboxes/:id", "", "", false},
		{http.MethodPost, "/api/sandboxes", "", "", false},
		{http.MethodGet, "/api/sandboxes/:id/start", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			category, operation, ok := classifySandboxOperation(tt.method, tt.path)
			if category != tt.category || operation != tt.operation || ok != tt.ok {
				t.Fatalf("classifySandboxOperation() = (%q, %q, %v), want (%q, %q, %v)",
					category, operation, ok, tt.category, tt.operation, tt.ok)
			}
		})
	}
}

func TestMiddlewareRecordsBoundedSandboxOperationMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	router := gin.New()
	router.Use(m.Middleware())
	router.POST("/api/sandboxes/:id/exec", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/sandboxes/sbx-123/exec", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)

	body := scrapeMetrics(t, m)
	labels := `category="command",operation="exec",sbx_id="sbx-123",status="200",voidrun_host="` + m.host + `"`
	if !strings.Contains(body, `voidrun_sbx_operation_requests_total{`+labels+`} 1`) {
		t.Fatalf("metrics body missing operation counter labels %q", labels)
	}
	if !strings.Contains(body, `voidrun_sbx_operation_duration_seconds_bucket{`+labels+`,le="`) {
		t.Fatalf("metrics body missing operation duration labels %q", labels)
	}
	if strings.Contains(body, `path="/api/sandboxes/sbx-123/exec"`) {
		t.Fatal("operation metrics must use bounded operation labels, not raw request paths")
	}
}

func TestRecordSandboxOperationExposesLifecycleMetrics(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.RecordSandboxOperation("sbx-1", "lifecycle", "sleep", "ok", 0.12)
	m.RecordSandboxOperation("sbx-1", "lifecycle", "start", "ok", 0.45)

	body := scrapeMetrics(t, m)
	sleepLabels := `category="lifecycle",operation="sleep",sbx_id="sbx-1",status="ok",voidrun_host="` + m.host + `"`
	startLabels := `category="lifecycle",operation="start",sbx_id="sbx-1",status="ok",voidrun_host="` + m.host + `"`
	if !strings.Contains(body, `voidrun_sbx_operation_requests_total{`+sleepLabels+`} 1`) {
		t.Fatalf("missing sleep counter\n%s", body)
	}
	if !strings.Contains(body, `voidrun_sbx_operation_requests_total{`+startLabels+`} 1`) {
		t.Fatalf("missing start counter\n%s", body)
	}
	if !strings.Contains(body, `voidrun_sbx_operation_duration_seconds_bucket{`+sleepLabels+`,le="`) {
		t.Fatalf("missing sleep duration\n%s", body)
	}
}

func TestMiddlewareDoesNotRecordLifecycleHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	router := gin.New()
	router.Use(m.Middleware())
	router.POST("/api/sandboxes/:id/sleep", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/sandboxes/sbx-1/sleep", nil))

	body := scrapeMetrics(t, m)
	if strings.Contains(body, `voidrun_sbx_operation_requests_total{`) && strings.Contains(body, `operation="sleep"`) {
		t.Fatal("lifecycle sleep must not be double-counted by HTTP middleware")
	}
}

func TestSetSandboxStatusExposesLifecycleState(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.SetSandboxStatus("sbx-1", "demo", "running")
	m.SetSandboxStatus("sbx-1", "demo", "snapshotted")

	body := scrapeMetrics(t, m)
	if strings.Contains(body, `status="running"`) && strings.Contains(body, `sbx_id="sbx-1"`) {
		t.Fatal("expected previous running status series removed after transition")
	}
	want := `voidrun_sbx_status{sbx_id="sbx-1",sbx_name="demo",status="snapshotted",voidrun_host="` + m.host + `"} 2`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing status gauge %q\n%s", want, body)
	}
}

func TestUnregisterSandboxKeepsStatusMetric(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.RegisterSandbox("sbx-1", "demo", "/tmp/sock", 1, 512, 1024)
	m.SetSandboxStatus("sbx-1", "demo", "snapshotted")
	m.UnregisterSandbox("sbx-1")

	body := scrapeMetrics(t, m)
	if !strings.Contains(body, `voidrun_sbx_status{sbx_id="sbx-1",sbx_name="demo",status="snapshotted"`) {
		t.Fatal("status metric must survive UnregisterSandbox (sleep keeps state)")
	}
	if strings.Contains(body, "voidrun_sbx_cpu_usage") && strings.Contains(body, `sbx_name="demo"`) {
		t.Fatal("resource metrics should be removed on unregister")
	}
}

func TestClearSandboxStatusRemovesSeries(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.SetSandboxStatus("sbx-1", "demo", "running")
	m.ClearSandboxStatus("sbx-1")
	body := scrapeMetrics(t, m)
	if strings.Contains(body, `sbx_id="sbx-1"`) && strings.Contains(body, "voidrun_sbx_status") {
		t.Fatal("expected status series removed after ClearSandboxStatus")
	}
}

func TestDeleteLeavesDeletedStatusTombstone(t *testing.T) {
	m := NewManager(config.MetricsConfig{IntervalSec: 10})
	m.RegisterSandbox("sbx-1", "demo", "/tmp/sock", 1, 512, 1024)
	m.UnregisterSandbox("sbx-1")
	m.SetSandboxStatus("sbx-1", "demo", "deleted")

	body := scrapeMetrics(t, m)
	want := `voidrun_sbx_status{sbx_id="sbx-1",sbx_name="demo",status="deleted",voidrun_host="` + m.host + `"} 0`
	if !strings.Contains(body, want) {
		t.Fatalf("expected deleted status tombstone %q\n%s", want, body)
	}
	if strings.Contains(body, "voidrun_sbx_cpu_usage") && strings.Contains(body, `sbx_id="sbx-1"`) {
		t.Fatal("resource metrics should be removed on unregister before deleted tombstone")
	}
}

func scrapeMetrics(t *testing.T, m *Manager) string {
	t.Helper()
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rr.Body.String()
}
