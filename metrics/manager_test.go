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

func TestClassifySandboxOperation(t *testing.T) {
	tests := []struct {
		method    string
		path      string
		category  string
		operation string
		ok        bool
	}{
		{http.MethodDelete, "/api/sandboxes/:id", "lifecycle", "delete", true},
		{http.MethodPost, "/api/sandboxes/:id/sleep", "lifecycle", "sleep", true},
		{http.MethodPost, "/api/sandboxes/:id/wake", "lifecycle", "wake", true},
		{http.MethodPost, "/api/sandboxes/:id/start", "lifecycle", "start", true},
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
	router.POST("/api/sandboxes/:id/start", func(c *gin.Context) {
		c.Status(http.StatusAccepted)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/sandboxes/sbx-123/start", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	labels := `category="lifecycle",operation="start",sbx_id="sbx-123",status="202",voidrun_host="` + m.host + `"`
	if !strings.Contains(body, `voidrun_sbx_operation_requests_total{`+labels+`} 1`) {
		t.Fatalf("metrics body missing operation counter labels %q", labels)
	}
	if !strings.Contains(body, `voidrun_sbx_operation_duration_seconds_bucket{`+labels+`,le="`) {
		t.Fatalf("metrics body missing operation duration labels %q", labels)
	}
	if strings.Contains(body, `path="/api/sandboxes/sbx-123/start"`) {
		t.Fatal("operation metrics must use bounded operation labels, not raw request paths")
	}
}
