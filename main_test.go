package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("simulated body read failure") }

func testConfig(t *testing.T, upstreamURL string) Config {
	t.Helper()
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	return Config{
		ListenAddr:            ":0",
		Upstream:              parsed,
		MaxAttempts:           4,
		MaxElapsed:            0,
		MaxBodyBytes:          1 << 20,
		MemoryBodyBytes:       1 << 20,
		InitialBackoff:        0,
		MaxBackoff:            0,
		BackoffJitter:         0,
		RetryAfterCap:         time.Second,
		MaxResponseDrainBytes: 1 << 20,
		MaxInflight:           0,
		ConnectTimeout:        time.Second,
		HeaderTimeout:         0,
		BodyDir:               t.TempDir(),
		RetryStatuses:         map[int]struct{}{502: {}, 503: {}, 504: {}},
	}
}

func TestRequestLogPathExcludesQuery(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://example.test/v1/models?token=do-not-log", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestLogPath(request); got != "/v1/models" {
		t.Fatalf("request log path = %q, want /v1/models", got)
	}
}

func TestSpoolBodyAddsReadContext(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/v1/responses", failingReader{})
	request.ContentLength = 1
	cfg := testConfig(t, "http://127.0.0.1:1")

	_, err := spoolBody(request, cfg)
	if err == nil || !strings.Contains(err.Error(), "read request body in memory: simulated body read failure") {
		t.Fatalf("spoolBody error = %v", err)
	}
}

func TestLoadConfigDebug(t *testing.T) {
	t.Setenv("DEBUG", "true")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Debug {
		t.Fatal("DEBUG=true was not loaded")
	}
}

func TestRetries503AndReplaysPostBody(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		attempts++
		bodies = append(bodies, string(body))
		current := attempts
		mu.Unlock()
		if current < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "temporary")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	proxy := newProxy(testConfig(t, upstream.URL))
	guard := httptest.NewServer(proxy)
	defer guard.Close()

	request, err := http.NewRequest(http.MethodPost, guard.URL+"/v1/chat/completions?stream=false", strings.NewReader(`{"model":"test","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-secret")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", body)
	}
	if got := response.Header.Get("X-Cpa-Guard-Attempts"); got != "3" {
		t.Fatalf("attempt header = %q, want 3", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("upstream attempts = %d, want 3", attempts)
	}
	if len(bodies) != 3 || bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("request bodies were not replayed: %#v", bodies)
	}
}

func TestDoesNotRetryAfterStreamingResponseStarts(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("test response does not support flushing")
		}
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.MaxAttempts = 10
	proxy := newProxy(cfg)
	guard := httptest.NewServer(proxy)
	defer guard.Close()

	response, err := http.Get(guard.URL + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "data: first") || !strings.Contains(string(body), "data: second") {
		t.Fatalf("stream body = %q", body)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want 1", got)
	}
}

func TestFinal503IsPassedThrough(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"still unavailable"}`)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.MaxAttempts = 2
	proxy := newProxy(cfg)
	guard := httptest.NewServer(proxy)
	defer guard.Close()

	response, err := http.Get(guard.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if string(body) != `{"error":"still unavailable"}` {
		t.Fatalf("body = %q", body)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2", got)
	}
	if got := response.Header.Get("X-Cpa-Guard-Attempts"); got != "2" {
		t.Fatalf("attempt header = %q, want 2", got)
	}
}

func TestHealthAndMetricsEndpoints(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	cfg.MetricsEnabled = true
	proxy := newProxy(cfg)
	guard := httptest.NewServer(proxy)
	defer guard.Close()

	health, err := http.Get(guard.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", health.StatusCode)
	}
	_ = health.Body.Close()

	metricsResponse, err := http.Get(guard.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer metricsResponse.Body.Close()
	metricsBody, _ := io.ReadAll(metricsResponse.Body)
	if metricsResponse.StatusCode != http.StatusOK || !strings.Contains(string(metricsBody), "cpa_guard_requests_total") {
		t.Fatalf("metrics response = %d %q", metricsResponse.StatusCode, metricsBody)
	}
}
