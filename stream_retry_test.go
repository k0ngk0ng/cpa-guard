package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCapacityStreamHTTP(t *testing.T) {
	prelude := "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"
	failure := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"type\":\"service_unavailable_error\"}}}\n\n"
	success := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
	for _, tc := range []struct {
		name, first       string
		disabled, exhaust bool
		want              int
	}{
		{"capacity_then_success", prelude + failure, false, false, 2},
		{"error_event", "data: {\"type\":\"error\",\"error\":{\"code\":\"server_is_overloaded\"}}\n\n", false, false, 2},
		{"budget_exhausted", prelude + failure, false, true, 3},
		{"disabled", prelude + failure, true, false, 1},
		{"after_output", prelude + success + failure, false, false, 1},
		{"other_error", strings.ReplaceAll(prelude+failure, "server_is_overloaded", "invalid_api_key"), false, false, 1},
		{"malformed", "data: invalid-json\n\n" + failure, false, false, 1},
		{"large_prelude", ":" + strings.Repeat("x", 65536) + "\n\n" + failure, false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"model":"test","stream":true}` {
					t.Errorf("request body changed")
				}
				n := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				if n == 1 || tc.exhaust {
					io.WriteString(w, tc.first)
				} else {
					io.WriteString(w, success)
				}
			}))
			defer upstream.Close()
			cfg := testConfig(t, upstream.URL)
			cfg.MaxAttempts = 3
			if tc.disabled {
				cfg.RetryKeys = map[string]bool{}
			}
			guard := httptest.NewServer(newProxy(cfg))
			defer guard.Close()
			res, err := http.Post(guard.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"test","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			wantBody := tc.first
			if tc.want == 2 {
				wantBody = success
			}
			if res.StatusCode != 200 || string(body) != wantBody {
				t.Fatalf("status=%d body differs: %.200s", res.StatusCode, body)
			}
			if int(calls.Load()) != tc.want {
				t.Fatalf("attempts=%d want=%d", calls.Load(), tc.want)
			}
		})
	}
}
