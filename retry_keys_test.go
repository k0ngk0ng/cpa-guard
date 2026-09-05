package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLoadACLConfigRetryKeyModes(t *testing.T) {
	const model = "gpt-4o"
	tests := []struct {
		name       string
		contents   string
		wantKeys   map[string]bool
		wantAbsent bool
	}{
		{
			name:       "retry table absent keeps legacy behavior",
			contents:   "[models.\"" + model + "\"]\nallow_keys = [\"key-a\"]\n",
			wantAbsent: true,
		},
		{
			name:     "explicit empty list disables all retries",
			contents: "[retry]\nallow_keys = []\n",
			wantKeys: map[string]bool{},
		},
		{
			name:     "explicit key list",
			contents: "[retry]\nallow_keys = [\"key-a\", \"key-b\"]\n",
			wantKeys: map[string]bool{"key-a": true, "key-b": true},
		},
		{
			name:     "wildcard key list",
			contents: "[retry]\nallow_keys = [\"*\"]\n",
			wantKeys: map[string]bool{"*": true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, got, err := loadACLConfig(writeTOMLACLFile(t, test.contents))
			if err != nil {
				t.Fatalf("loadACLConfig: %v", err)
			}
			if test.wantAbsent {
				if got != nil {
					t.Fatalf("RetryKeys = %#v, want nil when [retry] is absent", got)
				}
				return
			}
			if got == nil || len(got) != len(test.wantKeys) {
				t.Fatalf("RetryKeys = %#v, want %#v", got, test.wantKeys)
			}
			for key := range test.wantKeys {
				if !got[key] {
					t.Fatalf("RetryKeys[%q] is false, want true", key)
				}
			}
		})
	}
}

func TestLoadACLConfigRejectsInvalidRetryConfiguration(t *testing.T) {
	const secret = "secret-retry-key"
	tests := []struct {
		name     string
		contents string
	}{
		{
			name:     "missing allow_keys",
			contents: "[retry]\n",
		},
		{
			name:     "unknown field",
			contents: "[retry]\nallow_keys = [\"" + secret + "\"]\nmax_attempts = 2\n",
		},
		{
			name:     "scalar value",
			contents: "[retry]\nallow_keys = \"" + secret + "\"\n",
		},
		{
			name:     "wrong element type",
			contents: "[retry]\nallow_keys = [123]\n",
		},
		{
			name:     "empty key",
			contents: "[retry]\nallow_keys = [\"\"]\n",
		},
		{
			name:     "key with whitespace",
			contents: "[retry]\nallow_keys = [\"" + secret + " key\"]\n",
		},
		{
			name:     "duplicate allow_keys field",
			contents: "[retry]\nallow_keys = [\"" + secret + "\"]\nallow_keys = [\"other\"]\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, retryKeys, err := loadACLConfig(writeTOMLACLFile(t, test.contents))
			if err == nil || retryKeys != nil {
				t.Fatalf("loadACLConfig() = retryKeys=%#v err=%v, want an error", retryKeys, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("configuration error leaks retry API key: %q", err)
			}
		})
	}
}

func newRetryKeyProxy(t *testing.T, retryKeys map[string]bool, handler http.Handler) (*httptest.Server, *Config) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	cfg := testConfig(t, upstream.URL)
	cfg.MaxAttempts = 3
	cfg.InitialBackoff = 0
	cfg.MaxBackoff = 0
	cfg.RetryAfterCap = 0
	cfg.RetryKeys = retryKeys
	cfg.MetricsEnabled = true
	proxy := httptest.NewServer(newProxy(cfg))
	t.Cleanup(func() {
		proxy.Close()
		upstream.Close()
	})
	return proxy, &cfg
}

func readRetryMetrics(t *testing.T, proxy *httptest.Server) string {
	t.Helper()
	response, err := http.Get(proxy.URL + "/metrics")
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", response.StatusCode, body)
	}
	return string(body)
}

func TestRetryKey503RetryVsUnlistedSingleAttempt(t *testing.T) {
	newSequenceHandler := func(t *testing.T, calls *atomic.Int32, bodies *[]string, headers *[][]string, recordsMu *sync.Mutex) http.Handler {
		t.Helper()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upstream body: %v", err)
			}
			recordsMu.Lock()
			*bodies = append(*bodies, string(body))
			*headers = append(*headers, []string{r.Header.Get("Authorization"), r.Header.Get("X-Request-Trace")})
			recordsMu.Unlock()
			attempt := calls.Add(1)
			w.Header().Set("X-Upstream-Attempt", fmt.Sprint(attempt))
			w.Header().Set("Retry-After", "0")
			if attempt == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"temporary"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		})
	}

	t.Run("listed key retries", func(t *testing.T) {
		var calls atomic.Int32
		var bodies []string
		var headers [][]string
		var recordsMu sync.Mutex
		proxy, _ := newRetryKeyProxy(t, map[string]bool{"retry-key": true}, newSequenceHandler(t, &calls, &bodies, &headers, &recordsMu))
		request, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer retry-key")
		request.Header.Set("X-Request-Trace", "trace-1")
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
			t.Fatalf("status=%d body=%s, want retried 200", response.StatusCode, body)
		}
		recordsMu.Lock()
		gotBodies := append([]string(nil), bodies...)
		gotHeaders := append([][]string(nil), headers...)
		recordsMu.Unlock()
		if calls.Load() != 2 || len(gotBodies) != 2 || gotBodies[0] != gotBodies[1] {
			t.Fatalf("calls=%d bodies=%q, want two identical request bodies", calls.Load(), gotBodies)
		}
		for index, header := range gotHeaders {
			if header[0] != "Bearer retry-key" || header[1] != "trace-1" {
				t.Fatalf("attempt %d headers=%q, want auth and trace preserved", index+1, header)
			}
		}
		if got := response.Header.Get("X-Cpa-Guard-Attempts"); got != "2" {
			t.Fatalf("attempt header=%q, want 2", got)
		}
		metrics := readRetryMetrics(t, proxy)
		for _, want := range []string{
			"cpa_guard_attempts_total 2",
			"cpa_guard_retries_total 1",
			`cpa_guard_retry_responses_total{status="503"} 1`,
		} {
			if !strings.Contains(metrics, want) {
				t.Fatalf("metrics missing %q: %s", want, metrics)
			}
		}
	})

	t.Run("unlisted key forwards first 503 only", func(t *testing.T) {
		var calls atomic.Int32
		var bodies []string
		var headers [][]string
		var recordsMu sync.Mutex
		proxy, _ := newRetryKeyProxy(t, map[string]bool{"retry-key": true}, newSequenceHandler(t, &calls, &bodies, &headers, &recordsMu))
		request, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer other-key")
		request.Header.Set("X-Request-Trace", "trace-2")
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusServiceUnavailable || string(body) != `{"error":"temporary"}` {
			t.Fatalf("status=%d body=%s, want original first 503", response.StatusCode, body)
		}
		recordsMu.Lock()
		gotBodies := append([]string(nil), bodies...)
		gotHeaders := append([][]string(nil), headers...)
		recordsMu.Unlock()
		if calls.Load() != 1 || len(gotBodies) != 1 || gotHeaders[0][0] != "Bearer other-key" || gotHeaders[0][1] != "trace-2" {
			t.Fatalf("calls=%d bodies=%q headers=%q, want one unchanged upstream request", calls.Load(), gotBodies, gotHeaders)
		}
		if got := response.Header.Get("X-Cpa-Guard-Attempts"); got != "1" {
			t.Fatalf("attempt header=%q, want 1", got)
		}
		metrics := readRetryMetrics(t, proxy)
		for _, want := range []string{"cpa_guard_attempts_total 1", "cpa_guard_retries_total 0", `cpa_guard_retry_responses_total{status="503"} 0`} {
			if !strings.Contains(metrics, want) {
				t.Fatalf("metrics missing %q: %s", want, metrics)
			}
		}
	})
}

type retryKeyRoundTripper struct {
	calls atomic.Int32
}

func (t *retryKeyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.calls.Add(1) == 1 {
		return nil, errors.New("simulated transient network error")
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(`{"ok":true}`)),
		ContentLength: int64(len(`{"ok":true}`)),
		Request:       r,
	}, nil
}

func newNetworkRetryKeyProxy(t *testing.T, retryKeys map[string]bool, transport http.RoundTripper) *httptest.Server {
	t.Helper()
	cfg := testConfig(t, "http://upstream.invalid")
	cfg.MaxAttempts = 2
	cfg.InitialBackoff = 0
	cfg.MaxBackoff = 0
	cfg.RetryAfterCap = 0
	cfg.RetryKeys = retryKeys
	cfg.MetricsEnabled = true
	proxy := newProxy(cfg)
	proxy.client.Transport = transport
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server
}

func TestRetryKeyNetworkErrorRetryVsUnlistedSingleAttempt(t *testing.T) {
	t.Run("listed key retries network error", func(t *testing.T) {
		transport := &retryKeyRoundTripper{}
		proxy := newNetworkRetryKeyProxy(t, map[string]bool{"retry-key": true}, transport)
		request, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/ping", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer retry-key")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || transport.calls.Load() != 2 || response.Header.Get("X-Cpa-Guard-Attempts") != "2" {
			t.Fatalf("status=%d calls=%d attempts=%q, want 200, two calls, attempts=2", response.StatusCode, transport.calls.Load(), response.Header.Get("X-Cpa-Guard-Attempts"))
		}
		metricsResponse := mustHTTPGet(t, proxy.URL+"/metrics")
		metrics, _ := io.ReadAll(metricsResponse.Body)
		_ = metricsResponse.Body.Close()
		if !strings.Contains(string(metrics), "cpa_guard_retry_network_errors_total 1") {
			t.Fatalf("network retry metric missing: %s", metrics)
		}
	})

	t.Run("unlisted key does not retry network error", func(t *testing.T) {
		transport := &retryKeyRoundTripper{}
		proxy := newNetworkRetryKeyProxy(t, map[string]bool{"retry-key": true}, transport)
		request, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/ping", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer other-key")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable || transport.calls.Load() != 1 || response.Header.Get("X-Cpa-Guard-Attempts") != "1" {
			t.Fatalf("status=%d calls=%d attempts=%q, want 503, one call, attempts=1", response.StatusCode, transport.calls.Load(), response.Header.Get("X-Cpa-Guard-Attempts"))
		}
	})
}

func mustHTTPGet(t *testing.T, target string) *http.Response {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestRetryKeyEmptyAndWildcardPolicies(t *testing.T) {
	tests := []struct {
		name       string
		keys       map[string]bool
		want       int
		wantAllKey string
	}{
		{name: "empty list", keys: map[string]bool{}, want: 1},
		{name: "wildcard", keys: map[string]bool{"*": true}, want: 3, wantAllKey: "any-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://example.test/v1/ping", nil)
			request.Header.Set("Authorization", "Bearer "+test.wantAllKey)
			cfg := testConfig(t, "http://127.0.0.1:1")
			cfg.MaxAttempts = 3
			cfg.RetryKeys = test.keys
			if got := cfg.requestMaxAttempts(request); got != test.want {
				t.Fatalf("requestMaxAttempts()=%d, want %d", got, test.want)
			}
		})
	}
}

func TestRetryKeyConcurrentBudgetsStayRequestLocal(t *testing.T) {
	type keyState struct {
		calls int
	}
	var mu sync.Mutex
	states := make(map[string]*keyState)
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		key, _ := requestAPIKey(r)
		mu.Lock()
		state := states[key]
		if state == nil {
			state = &keyState{}
			states[key] = state
		}
		state.calls++
		attempt := state.calls
		mu.Unlock()

		status := http.StatusServiceUnavailable
		body := `{"error":"temporary"}`
		if attempt > 1 {
			status = http.StatusOK
			body = `{"ok":true}`
		}
		return &http.Response{
			StatusCode:    status,
			Header:        http.Header{"Retry-After": []string{"0"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       r,
		}, nil
	})
	cfg := testConfig(t, "http://upstream.invalid")
	cfg.MaxAttempts = 3
	cfg.InitialBackoff = 0
	cfg.MaxBackoff = 0
	cfg.RetryAfterCap = 0
	cfg.RetryKeys = map[string]bool{"retry-key": true}
	p := newProxy(cfg)
	p.client.Transport = transport
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	type result struct {
		key      string
		status   int
		attempts string
	}
	results := make(chan result, 2)
	for _, key := range []string{"retry-key", "other-key"} {
		go func(key string) {
			request, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/ping", nil)
			if err != nil {
				results <- result{key: key, status: 0}
				return
			}
			request.Header.Set("Authorization", "Bearer "+key)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				results <- result{key: key, status: 0}
				return
			}
			_ = response.Body.Close()
			results <- result{key: key, status: response.StatusCode, attempts: response.Header.Get("X-Cpa-Guard-Attempts")}
		}(key)
	}
	got := make(map[string]result, 2)
	for range 2 {
		entry := <-results
		got[entry.key] = entry
	}
	if got["retry-key"].status != http.StatusOK || got["retry-key"].attempts != "2" {
		t.Fatalf("listed key result=%+v, want 200 after two attempts", got["retry-key"])
	}
	if got["other-key"].status != http.StatusServiceUnavailable || got["other-key"].attempts != "1" {
		t.Fatalf("unlisted key result=%+v, want first 503 after one attempt", got["other-key"])
	}
	mu.Lock()
	defer mu.Unlock()
	if states["retry-key"].calls != 2 || states["other-key"].calls != 1 {
		t.Fatalf("per-key transport calls=%+v, want retry-key=2 other-key=1", states)
	}
	if p.cfg.MaxAttempts != 3 {
		t.Fatalf("shared MaxAttempts=%d changed during concurrent requests", p.cfg.MaxAttempts)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRetryOnlyConfigCredentialHandling(t *testing.T) {
	t.Setenv("MODEL_ACL_FILE", writeTOMLACLFile(t, "[retry]\nallow_keys = [\"retry-key\"]\n"))
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelRules != nil || cfg.ModelACL != nil {
		t.Fatal("retry-only config enabled model ACL")
	}
	cfg.MaxAttempts = 2
	cfg.MaxElapsed = 0
	cfg.InitialBackoff = 0
	cfg.MaxBackoff = 0
	cfg.BodyDir = t.TempDir()
	proxy := newProxy(cfg)
	var calls atomic.Int32
	proxy.client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream response")), Request: r}, nil
	})
	for _, scenario := range []struct {
		name     string
		headers  http.Header
		attempts int32
	}{
		{"missing", http.Header{}, 1},
		{"bearer", http.Header{"Authorization": {"Bearer retry-key"}}, 2},
		{"api key", http.Header{"X-Api-Key": {"retry-key"}}, 2},
		{"google key", http.Header{"X-Goog-Api-Key": {"retry-key"}}, 2},
		{"ambiguous", http.Header{"Authorization": {"Bearer retry-key"}, "X-Api-Key": {"retry-key"}}, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before := calls.Load()
			request := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
			request.Header = scenario.headers
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != 503 || response.Body.String() != "upstream response" || calls.Load()-before != scenario.attempts {
				t.Fatalf("status=%d calls=%d, want original 503 and %d attempts", response.Code, calls.Load()-before, scenario.attempts)
			}
		})
	}
}
