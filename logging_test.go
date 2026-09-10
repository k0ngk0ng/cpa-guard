package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRequestLogIdentity(t *testing.T) {
	for _, header := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		t.Run(header, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			key := "sk-1234567890abcd"
			value := key
			if header == "Authorization" {
				value = "Bearer " + key
			}
			r.Header.Set(header, value)
			r.RemoteAddr = "[2001:db8::1]:1234"
			r.Header.Add("Forwarded", "for=192.0.2.1")
			r.Header.Add("Forwarded", "for=192.0.2.2")
			r.Header.Add("X-Forwarded-For", "192.0.2.1, 192.0.2.2")
			r.Header.Add("X-Forwarded-For", "192.0.2.3")
			r.Header.Set("X-Real-IP", "192.0.2.1")
			entry := map[string]any{}
			addRequestLogIdentity(entry, r)
			for field, want := range map[string]string{
				"remote_ip": "2001:db8::1", "forwarded": "for=192.0.2.1, for=192.0.2.2",
				"x_forwarded_for": "192.0.2.1, 192.0.2.2, 192.0.2.3", "x_real_ip": "192.0.2.1", "api_key": "sk-1****abcd",
			} {
				if entry[field] != want {
					t.Errorf("%s = %q, want %q", field, entry[field], want)
				}
			}
			r.Header.Add(header, value)
			addRequestLogIdentity(entry, r)
			if entry["api_key"] != "" {
				t.Fatal("ambiguous credentials were logged")
			}
		})
	}
	for _, peer := range []string{"192.0.2.1:1234", "192.0.2.1"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = peer
		entry := map[string]any{}
		addRequestLogIdentity(entry, r)
		if entry["remote_ip"] != "192.0.2.1" || entry["api_key"] != "" {
			t.Fatalf("unexpected identity: %v", entry)
		}
	}
}

func TestMaskAPIKey(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"", ""}, {"a", "****"}, {"12345678", "****"}, {"123456789", "1234****6789"}, {"abcdefghijklmnop", "abcd****mnop"},
	} {
		if got := maskAPIKey(tc.key); got != tc.want {
			t.Errorf("mask for length %d = %q, want %q", len(tc.key), got, tc.want)
		}
	}
}

func TestLogEventsIncludeMaskedIdentity(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "logs")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = old }()
	r := httptest.NewRequest("POST", "/v1/responses?secret=query-secret", nil)
	r.Header.Set("Authorization", "Bearer sk-1234567890abcd")
	r.Header.Set("X-Real-IP", "192.0.2.5")
	p := &Proxy{cfg: Config{Debug: true}}
	p.logRequest("id", r, 400, 0, "request_body_error", time.Now())
	p.logRequestBodyDebug("id", r, errors.New("read failed"))
	os.Stdout = old
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-1234567890abcd") || strings.Contains(string(data), "query-secret") || strings.Contains(string(data), "Bearer") {
		t.Fatal("logs contain credentials or query")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines", len(lines))
	}
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["api_key"] != "sk-1****abcd" || entry["x_real_ip"] != "192.0.2.5" || entry["path"] != "/v1/responses" {
			t.Fatalf("unexpected log: %v", entry)
		}
	}
}
