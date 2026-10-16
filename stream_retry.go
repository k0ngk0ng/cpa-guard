package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Bound speculative buffering, including incomplete tool calls. Completed tool
// input, text output, and unknown events commit the stream.
const maxStreamPreludeBytes = 64 << 10

type replayStreamBody struct {
	io.Reader
	io.Closer
}

func retryableErrorStream(response *http.Response) bool {
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || mediaType != "text/event-stream" || response.Header.Get("Content-Encoding") != "" {
		return false
	}
	original := response.Body
	reader := bufio.NewReader(original)
	var prefix bytes.Buffer
	defer func() {
		response.Body = &replayStreamBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader), Closer: original}
	}()
	var lineBuffer bytes.Buffer
	var data string
	// A delta is safe to buffer only after its matching tool item was seen.
	type pendingTool struct{ id, kind string }
	pending := make(map[int]pendingTool)
	for prefix.Len() < maxStreamPreludeBytes {
		b, err := reader.ReadByte()
		if err != nil {
			return false
		}
		prefix.WriteByte(b)
		if b != '\n' {
			lineBuffer.WriteByte(b)
			continue
		}
		line := strings.TrimSuffix(lineBuffer.String(), "\r")
		lineBuffer.Reset()
		if line == "" {
			if data != "" {
				var event struct {
					Type        string `json:"type"`
					OutputIndex *int   `json:"output_index"`
					ItemID      string `json:"item_id"`
					Item        struct {
						ID     string `json:"id"`
						Type   string `json:"type"`
						Status string `json:"status"`
					} `json:"item"`
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
					Response struct {
						Error struct {
							Code string `json:"code"`
						} `json:"error"`
						Output []json.RawMessage `json:"output"`
					} `json:"response"`
				}
				if json.Unmarshal([]byte(data), &event) != nil {
					return false
				}
				switch event.Type {
				case "error":
					return retryableStreamErrorCode(event.Error.Code)
				case "response.failed":
					return retryableStreamErrorCode(event.Response.Error.Code)
				case "response.created", "response.in_progress":
					if len(event.Response.Output) != 0 {
						return false
					}
				case "response.output_item.added":
					if event.OutputIndex == nil || *event.OutputIndex < 0 || event.Item.ID == "" ||
						(event.Item.Type != "custom_tool_call" && event.Item.Type != "function_call") ||
						(event.Item.Status != "" && event.Item.Status != "in_progress") {
						return false
					}
					if _, exists := pending[*event.OutputIndex]; exists {
						return false
					}
					pending[*event.OutputIndex] = pendingTool{event.Item.ID, event.Item.Type}
				case "response.custom_tool_call_input.delta", "response.function_call_arguments.delta":
					if event.OutputIndex == nil {
						return false
					}
					tool, exists := pending[*event.OutputIndex]
					kind := "custom_tool_call"
					if event.Type == "response.function_call_arguments.delta" {
						kind = "function_call"
					}
					if !exists || tool.id != event.ItemID || tool.kind != kind {
						return false
					}
				case "response.metadata":
					// Diagnostic metadata carries no executable tool input.
				default:
					// This includes input/arguments.done and output_item.done: once
					// a complete call can reach the client, never replay it.
					return false
				}
			}
			data = ""
		} else if strings.HasPrefix(line, "data:") {
			data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ") + "\n"
		}
	}
	return false
}

func retryableStreamErrorCode(code string) bool {
	return code == "server_is_overloaded" || code == "server_error"
}
