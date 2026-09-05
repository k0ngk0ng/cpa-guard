package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

func writeACLFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "acl.json")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write ACL file: %v", err)
	}
	return path
}

func testACLProxy(t *testing.T, acl modelACL, handler http.Handler) (*httptest.Server, *httptest.Server, *Config) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	cfg := testConfig(t, upstream.URL)
	cfg.ModelACL = acl
	proxy := httptest.NewServer(newProxy(cfg))
	t.Cleanup(func() {
		proxy.Close()
		upstream.Close()
	})
	return proxy, upstream, &cfg
}

func doACLRequest(t *testing.T, proxy *httptest.Server, method, path, key, body string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, response.Header, data
}

func TestLoadModelACLConfig(t *testing.T) {
	path := writeACLFile(t, `{"key-a":["gpt-4o","claude-3"],"key-b":["*"]}`)
	t.Setenv("MODEL_ACL_FILE", path)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ModelACL == nil || !cfg.ModelACL["key-a"]["gpt-4o"] || !cfg.ModelACL["key-b"]["*"] {
		t.Fatalf("loaded model ACL = %#v", cfg.ModelACL)
	}
}

func TestLoadModelACLRejectsInvalidAndDuplicateConfig(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "not object", data: `[]`},
		{name: "malformed JSON", data: `{"key":[`},
		{name: "null models", data: `{"key":null}`},
		{name: "models not array", data: `{"key":"gpt-4o"}`},
		{name: "empty key", data: `{"": ["gpt-4o"]}`},
		{name: "key with newline", data: `{"bad\nkey": ["gpt-4o"]}`},
		{name: "empty model", data: `{"key": [""]}`},
		{name: "trimmed model", data: `{"key": [" gpt-4o"]}`},
		{name: "duplicate key", data: `{"key":["gpt-4o"],"key":["claude-3"]}`},
		{name: "trailing JSON", data: `{"key":["gpt-4o"]} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if acl, err := loadModelACL(writeACLFile(t, test.data)); err == nil || acl != nil {
				t.Fatalf("loadModelACL(%s) = %#v, want an error", test.data, acl)
			}
		})
	}
}

func TestModelACLDisabledKeepsUnauthenticatedCompatibility(t *testing.T) {
	var calls atomic.Int32
	proxy, _, _ := testACLProxy(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	status, _, _ := doACLRequest(t, proxy, http.MethodGet, "/v1/anything", "", "")
	if status != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("disabled ACL request status=%d calls=%d, want 204 and one upstream call", status, calls.Load())
	}
}

func TestModelACLAuthenticationRejectsMissingUnknownAndMultipleSources(t *testing.T) {
	acl := modelACL{"valid": modelSet{"gpt-4o": true}}
	tests := []struct {
		name  string
		setup func(*http.Request)
	}{
		{name: "missing", setup: func(*http.Request) {}},
		{name: "unknown", setup: func(r *http.Request) { r.Header.Set("Authorization", "Bearer unknown") }},
		{name: "wrong scheme", setup: func(r *http.Request) { r.Header.Set("Authorization", "Basic valid") }},
		{name: "authorization and x-api-key", setup: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer valid")
			r.Header.Set("X-API-Key", "valid")
		}},
		{name: "two authorization values", setup: func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer valid")
			r.Header.Add("Authorization", "Bearer valid")
		}},
		{name: "two alternative sources", setup: func(r *http.Request) {
			r.Header.Set("X-API-Key", "valid")
			r.Header.Set("X-Goog-API-Key", "valid")
		}},
		{name: "query key", setup: func(r *http.Request) {
			r.URL.RawQuery = "api_key=valid"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
			test.setup(request)
			allowed, err := acl.authenticate(request)
			if allowed != nil || err == nil || err.Status != http.StatusUnauthorized {
				t.Fatalf("authenticate() = allowed=%#v err=%#v, want 401", allowed, err)
			}
		})
	}
}

func TestModelACLAuthenticationAcceptsEachSupportedSingleSource(t *testing.T) {
	acl := modelACL{"valid": modelSet{"gpt-4o": true}}
	for _, test := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "authorization", header: "Authorization", value: "Bearer valid"},
		{name: "x-api-key", header: "X-API-Key", value: "valid"},
		{name: "x-goog-api-key", header: "X-Goog-API-Key", value: "valid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
			request.Header.Set(test.header, test.value)
			allowed, err := acl.authenticate(request)
			if err != nil || allowed == nil || !allowed["gpt-4o"] {
				t.Fatalf("authenticate() = allowed=%#v err=%v, want configured key", allowed, err)
			}
		})
	}
}

func TestModelACLAllowsConfiguredModelAndRejectsOtherModel(t *testing.T) {
	var calls atomic.Int32
	proxy, _, _ := testACLProxy(t, modelACL{"valid": modelSet{"gpt-4o": true}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	status, _, _ := doACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "valid", `{"model":"gpt-4o","stream":true,"messages":[]}`)
	if status != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("allowed model status=%d calls=%d, want 204 and one upstream call", status, calls.Load())
	}

	status, _, _ = doACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "valid", `{"model":"claude-3","messages":[]}`)
	if status != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("denied model status=%d calls=%d, want 403 and no additional upstream call", status, calls.Load())
	}
}

func TestModelACLRejectsMissingDuplicateAndAmbiguousModel(t *testing.T) {
	var calls atomic.Int32
	proxy, _, _ := testACLProxy(t, modelACL{"valid": modelSet{"gpt-4o": true}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{name: "missing model", body: `{"messages":[]}`, status: http.StatusBadRequest},
		{name: "null model", body: `{"model":null}`, status: http.StatusBadRequest},
		{name: "array model", body: `{"model":["gpt-4o"]}`, status: http.StatusBadRequest},
		{name: "trailing JSON", body: `{"model":"gpt-4o"} {"model":"other"}`, status: http.StatusBadRequest},
		{name: "duplicate model", body: `{"model":"gpt-4o","model":"gpt-4o"}`, status: http.StatusBadRequest},
		{name: "case ambiguous model field", body: `{"model":"gpt-4o","Model":"gpt-4o"}`, status: http.StatusBadRequest},
		{name: "case sensitive model value", body: `{"model":"GPT-4O"}`, status: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, _, _ := doACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "valid", test.body)
			if status != test.status {
				t.Fatalf("status=%d, want %d", status, test.status)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls=%d, want zero for all rejected requests", calls.Load())
	}
}

func TestModelACLRejectsUnknownPathForRestrictedKey(t *testing.T) {
	var calls atomic.Int32
	proxy, _, _ := testACLProxy(t, modelACL{"valid": modelSet{"gpt-4o": true}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	status, _, _ := doACLRequest(t, proxy, http.MethodPost, "/v1/unknown", "valid", `{"model":"gpt-4o"}`)
	if status != http.StatusForbidden || calls.Load() != 0 {
		t.Fatalf("unknown path status=%d calls=%d, want 403 and no upstream call", status, calls.Load())
	}
}

func TestModelACLListsOnlyAllowedModels(t *testing.T) {
	var calls atomic.Int32
	proxy, _, _ := testACLProxy(t, modelACL{"valid": modelSet{"z-model": true, "a-model": true}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	status, headers, body := doACLRequest(t, proxy, http.MethodGet, "/v1/models", "valid", "")
	if status != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("model list status=%d calls=%d, want 200 and no upstream call", status, calls.Load())
	}
	if got := headers.Get("X-Cpa-Guard-Attempts"); got != "0" {
		t.Fatalf("model list attempts header=%q, want 0", got)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		ids = append(ids, model.ID)
	}
	if !sort.StringsAreSorted(ids) || strings.Join(ids, ",") != "a-model,z-model" {
		t.Fatalf("model list ids=%v, want sorted allowed models", ids)
	}
}

func TestModelACLWildcardForwardsAnyEndpoint(t *testing.T) {
	var calls atomic.Int32
	proxy, _, _ := testACLProxy(t, modelACL{"valid": modelSet{"*": true}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusNoContent)
	}))

	status, headers, _ := doACLRequest(t, proxy, http.MethodGet, "/v1/unknown?anything=1", "valid", "")
	if status != http.StatusNoContent || calls.Load() != 1 || headers.Get("X-Upstream") != "yes" {
		t.Fatalf("wildcard request status=%d calls=%d upstream-header=%q, want forwarded request", status, calls.Load(), headers.Get("X-Upstream"))
	}
}

func TestModelACLSpoolToDiskAndRetryPreservesBody(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		bodies = append(bodies, string(data))
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.ModelACL = modelACL{"valid": modelSet{"gpt-4o": true}}
	cfg.MemoryBodyBytes = 1
	cfg.MaxAttempts = 2
	proxy := httptest.NewServer(newProxy(cfg))
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","payload":"body large enough for disk spool"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d, want 204 and two upstream attempts", response.StatusCode, calls.Load())
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[0] == "" {
		t.Fatalf("retried request bodies=%q, want identical non-empty bodies", bodies)
	}
	entries, err := os.ReadDir(cfg.BodyDir)
	if err != nil {
		t.Fatalf("read body spool directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("body spool directory has %d entries after request, want cleanup", len(entries))
	}
}

func TestModelACLAuthenticatesBeforeReadingBody(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	cfg.ModelACL = modelACL{"valid": {"gpt-4o": true}}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", failingReader{})
	request.ContentLength = 1
	response := httptest.NewRecorder()
	newProxy(cfg).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 before attempting to read failing body", response.Code)
	}
}

func TestModelACLEmptyPoliciesDenyAccess(t *testing.T) {
	for _, contents := range []string{`{}`, `{"valid":[]}`} {
		acl, err := loadModelACL(writeACLFile(t, contents))
		if err != nil || acl == nil {
			t.Fatalf("empty ACL: %v", err)
		}
		cfg := testConfig(t, "http://127.0.0.1:1")
		cfg.ModelACL = acl
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
		request.Header.Set("Authorization", "Bearer valid")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		newProxy(cfg).ServeHTTP(response, request)
		want := http.StatusUnauthorized
		if len(acl) > 0 {
			want = http.StatusForbidden
		}
		if response.Code != want {
			t.Fatalf("empty policy status = %d, want %d", response.Code, want)
		}
	}
}
