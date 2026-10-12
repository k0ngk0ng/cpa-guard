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

// Bound speculative buffering; unknown events or actual output commit the stream.
const maxStreamPreludeBytes = 64 << 10

type replayStreamBody struct {
	io.Reader
	io.Closer
}

func retryableCapacityStream(response *http.Response) bool {
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
					Type  string `json:"type"`
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
					return event.Error.Code == "server_is_overloaded"
				case "response.failed":
					return event.Response.Error.Code == "server_is_overloaded"
				case "response.created", "response.in_progress":
					if len(event.Response.Output) != 0 {
						return false
					}
				default:
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
