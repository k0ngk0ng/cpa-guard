package main

import (
	"encoding/json"
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

	frame := func(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }
	toolStart := func(kind string) string {
		return frame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "tool-1", "type": kind, "status": "in_progress"}})
	}
	toolDelta := func(kind, id string) string {
		return frame(map[string]any{"type": kind, "item_id": id, "output_index": 0, "delta": "partial"})
	}
	custom := toolStart("custom_tool_call") + toolDelta("response.custom_tool_call_input.delta", "tool-1")
	function := toolStart("function_call") + toolDelta("response.function_call_arguments.delta", "tool-1")
	metadata := frame(map[string]any{"type": "response.metadata"})
	customDone := frame(map[string]any{"type": "response.custom_tool_call_input.done", "item_id": "tool-1", "output_index": 0, "input": "complete"})
	itemDone := frame(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "tool-1", "type": "custom_tool_call", "status": "completed"}})
	for _, tc := range []struct {
		name, first       string
		disabled, exhaust bool
		want              int
	}{
		{"capacity_then_success", prelude + failure, false, false, 2},
		{"partial_custom_retry", prelude + custom + metadata + failure, false, false, 2},
		{"partial_function_retry", prelude + function + failure, false, false, 2},
		{"partial_server_error_retry", prelude + custom + strings.ReplaceAll(failure, "server_is_overloaded", "server_error"), false, false, 2},
		{"partial_exhausted", prelude + custom + failure, false, true, 3},
		{"partial_disabled", prelude + custom + failure, true, false, 1},
		{"completed_input_no_retry", prelude + custom + customDone + failure, false, false, 1},
		{"completed_item_no_retry", prelude + custom + itemDone + failure, false, false, 1},
		{"partial_success_passthrough", prelude + custom + customDone + itemDone, false, false, 1},
		{"partial_eof_passthrough", prelude + custom, false, false, 1},
		{"partial_over_limit", prelude + custom + ":" + strings.Repeat("x", 65536) + "\n\n" + failure, false, false, 1},
		{"unmatched_tool_delta", prelude + toolDelta("response.custom_tool_call_input.delta", "unknown") + failure, false, false, 1},
		{"wrong_tool_delta_kind", prelude + toolStart("function_call") + toolDelta("response.custom_tool_call_input.delta", "tool-1") + failure, false, false, 1},
		{"message_item_no_retry", prelude + toolStart("message") + failure, false, false, 1},

		{"server_error_then_success", strings.ReplaceAll(prelude+failure, "server_is_overloaded", "server_error"), false, false, 2},
		{"server_error_event", "data: {\"type\":\"error\",\"error\":{\"code\":\"server_error\"}}\n\n", false, false, 2},
		{"server_error_budget_exhausted", strings.ReplaceAll(prelude+failure, "server_is_overloaded", "server_error"), false, true, 3},
		{"server_error_disabled", strings.ReplaceAll(prelude+failure, "server_is_overloaded", "server_error"), true, false, 1},
		{"server_error_after_output", strings.ReplaceAll(prelude+success+failure, "server_is_overloaded", "server_error"), false, false, 1},
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
