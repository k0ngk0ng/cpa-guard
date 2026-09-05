package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func writeTOMLACLFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model-acl.toml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write TOML ACL file: %v", err)
	}
	return path
}

func loadTOMLRulesForTest(t *testing.T, contents string) modelRules {
	t.Helper()
	acl, rules, err := loadACLConfig(writeTOMLACLFile(t, contents))
	if err != nil {
		t.Fatalf("loadACLConfig: %v", err)
	}
	if acl != nil {
		t.Fatalf("TOML config returned legacy JSON ACL %#v", acl)
	}
	return rules
}

func newTOMLACLProxy(t *testing.T, rules modelRules, handler http.Handler) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	cfg := testConfig(t, upstream.URL)
	cfg.ModelRules = rules
	proxy := httptest.NewServer(newProxy(cfg))
	t.Cleanup(func() {
		proxy.Close()
		upstream.Close()
	})
	return proxy
}

func newTOMLACLProxyWithMaxBody(t *testing.T, rules modelRules, maxBody int64, handler http.Handler) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	cfg := testConfig(t, upstream.URL)
	cfg.ModelRules = rules
	cfg.MaxBodyBytes = maxBody
	proxy := httptest.NewServer(newProxy(cfg))
	t.Cleanup(func() {
		proxy.Close()
		upstream.Close()
	})
	return proxy
}

func doTOMLACLRequest(t *testing.T, proxy *httptest.Server, method, path, key, body string) (int, http.Header, []byte) {
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

func TestLoadACLConfigTOMLModelRules(t *testing.T) {
	rules := loadTOMLRulesForTest(t, `
[models."gpt-4o"]
allow_keys = [
  "key-a", # Comments and trailing commas are supported.
]

[models."open-model"]
allow_keys = ["*"]

[models."private-model"]
allow_keys = []
`)
	if len(rules) != 3 {
		t.Fatalf("loaded TOML rules = %#v, want three models", rules)
	}
	if got := rules["gpt-4o"].AllowKeys; len(got) != 1 || got[0] != "key-a" {
		t.Fatalf("gpt-4o allow_keys = %#v, want [key-a]", got)
	}
	if got := rules["open-model"].AllowKeys; len(got) != 1 || got[0] != "*" {
		t.Fatalf("open-model allow_keys = %#v, want [*]", got)
	}
	if got := rules["private-model"].AllowKeys; got == nil || len(got) != 0 {
		t.Fatalf("private-model allow_keys = %#v, want an explicit empty list", got)
	}
}

func TestLoadACLConfigTOMLRejectsUnknownDuplicateMissingAndWrongTypes(t *testing.T) {
	const secret = "secret-api-key-should-not-appear"
	tests := []struct {
		name string
		data string
	}{
		{
			name: "unknown root field",
			data: "unexpected = true\n[models.\"secret-model\"]\nallow_keys = [\"" + secret + "\"]\n",
		},
		{
			name: "unknown model field",
			data: "[models.\"secret-model\"]\nallow_keys = [\"" + secret + "\"]\nunexpected = true\n",
		},
		{
			name: "duplicate field",
			data: "[models.\"secret-model\"]\nallow_keys = [\"" + secret + "\"]\nallow_keys = [\"other\"]\n",
		},
		{
			name: "duplicate model table",
			data: "[models.\"secret-model\"]\nallow_keys = [\"" + secret + "\"]\n[models.\"secret-model\"]\nallow_keys = [\"other\"]\n",
		},
		{
			name: "missing allow_keys",
			data: "[models.\"secret-model\"]\n",
		},
		{
			name: "allow_keys scalar",
			data: "[models.\"secret-model\"]\nallow_keys = \"" + secret + "\"\n",
		},
		{
			name: "allow_keys wrong element type",
			data: "[models.\"secret-model\"]\nallow_keys = [123]\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, rules, err := loadACLConfig(writeTOMLACLFile(t, test.data))
			if err == nil || rules != nil {
				t.Fatalf("loadACLConfig() = rules=%#v err=%v, want an error", rules, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("configuration error leaks API key: %q", err)
			}
		})
	}
}

func TestTOMLModelACLDefaultAllowAndPerModelKeys(t *testing.T) {
	rules := loadTOMLRulesForTest(t, `
[models."restricted-model"]
allow_keys = ["key-a"]

[models."wildcard-model"]
allow_keys = ["*"]

[models."blocked-model"]
allow_keys = []
`)
	var calls atomic.Int32
	proxy := newTOMLACLProxy(t, rules, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer invalid-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	status, _, _ := doTOMLACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "key-a", `{"model":"restricted-model"}`)
	if status != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("allowed key status=%d calls=%d, want 204 and one upstream call", status, calls.Load())
	}

	status, _, _ = doTOMLACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "key-b", `{"model":"restricted-model"}`)
	if status != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("unlisted key status=%d calls=%d, want 403 and no upstream call", status, calls.Load())
	}

	status, _, _ = doTOMLACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "unknown-key", `{"model":"wildcard-model"}`)
	if status != http.StatusNoContent || calls.Load() != 2 {
		t.Fatalf("wildcard key status=%d calls=%d, want 204 and another upstream call", status, calls.Load())
	}

	status, _, _ = doTOMLACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "key-a", `{"model":"blocked-model"}`)
	if status != http.StatusForbidden || calls.Load() != 2 {
		t.Fatalf("empty allow_keys status=%d calls=%d, want 403 and no upstream call", status, calls.Load())
	}

	// The guard has no rule for this model. The request must reach the
	// upstream, which remains responsible for deciding whether the key is valid.
	status, _, _ = doTOMLACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "invalid-key", `{"model":"unconfigured-model"}`)
	if status != http.StatusUnauthorized || calls.Load() != 3 {
		t.Fatalf("unconfigured model status=%d calls=%d, want upstream 401 and three calls", status, calls.Load())
	}
	status, _, _ = doTOMLACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "key-b", `{"model":"unconfigured-model"}`)
	if status != http.StatusNoContent || calls.Load() != 4 {
		t.Fatalf("unconfigured model status=%d calls=%d, want 204 and four calls", status, calls.Load())
	}
}

func TestTOMLModelACLFiltersModelsListAndRetainsUnconfiguredModels(t *testing.T) {
	rules := loadTOMLRulesForTest(t, `
[models."restricted-model"]
allow_keys = ["key-a"]

[models."blocked-model"]
allow_keys = []
`)
	var calls atomic.Int32
	proxy := newTOMLACLProxy(t, rules, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"restricted-model","object":"model"},{"id":"blocked-model","object":"model"},{"id":"unconfigured-model","object":"model"}]}`)
	}))

	status, headers, body := doTOMLACLRequest(t, proxy, http.MethodGet, "/v1/models", "key-a", "")
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("allowed list status=%d calls=%d, want 200 and one upstream call", status, calls.Load())
	}
	if got := headers.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("filtered list Cache-Control=%q, want private, no-store", got)
	}
	assertTOMLModelListIDs(t, body, []string{"restricted-model", "unconfigured-model"})

	status, _, body = doTOMLACLRequest(t, proxy, http.MethodGet, "/v1/models", "key-b", "")
	if status != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("restricted list status=%d calls=%d, want 200 and two upstream calls", status, calls.Load())
	}
	assertTOMLModelListIDs(t, body, []string{"unconfigured-model"})
}

func assertTOMLModelListIDs(t *testing.T, body []byte, want []string) {
	t.Helper()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode filtered model list: %v; body=%s", err, body)
	}
	got := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		got = append(got, model.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("filtered model IDs=%v, want %v", got, want)
	}
}

func TestTOMLModelACLRejectsMalformedAndOversizedListsWithoutLeak(t *testing.T) {
	const secretModel = "secret-model-never-return"
	tests := []struct {
		name       string
		body       string
		maxBody    int64
		wantStatus int
	}{
		{
			name:       "malformed JSON",
			body:       `{"object":"list","data":[{"id":"` + secretModel + `"}`,
			maxBody:    1 << 20,
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "oversized body",
			body:       `{"object":"list","data":[{"id":"` + secretModel + `","padding":"this makes the list too large"}]}`,
			maxBody:    16,
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rules := loadTOMLRulesForTest(t, "[models.\""+secretModel+"\"]\nallow_keys = [\"allowed-key\"]\n")
			proxy := newTOMLACLProxyWithMaxBody(t, rules, test.maxBody, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			status, _, body := doTOMLACLRequest(t, proxy, http.MethodGet, "/v1/models", "other-key", "")
			if status != test.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", status, test.wantStatus, body)
			}
			if strings.Contains(string(body), secretModel) {
				t.Fatalf("error response leaks model name: %s", body)
			}
		})
	}
}

func TestTOMLEmptyFileAllowsModels(t *testing.T) {
	rules := loadTOMLRulesForTest(t, "# No restricted models yet.\n")
	if rules == nil {
		t.Fatal("empty TOML must still enable credential checks")
	}
	proxy := newTOMLACLProxy(t, rules, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	status, _, _ := doTOMLACLRequest(t, proxy, http.MethodPost, "/v1/chat/completions", "any-key", `{"model":"any-model"}`)
	if status != http.StatusNoContent {
		t.Fatalf("empty TOML status=%d", status)
	}
}

func TestTOMLModelListDoesNotReuseConditionalResponse(t *testing.T) {
	rules := loadTOMLRulesForTest(t, "[models.\"private\"]\nallow_keys = []\n")
	proxy := newTOMLACLProxy(t, rules, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" || r.Header.Get("Range") != "" {
			t.Error("model list forwarded a conditional or range request")
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("model list did not request identity encoding")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	request, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("If-None-Match", "old-list")
	request.Header.Set("If-Modified-Since", "Mon, 01 Jan 2024 00:00:00 GMT")
	request.Header.Set("Range", "bytes=0-100")
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("model list status=%d, want 502", response.StatusCode)
	}
}
