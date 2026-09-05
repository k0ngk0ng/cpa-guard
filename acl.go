package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"sort"
	"strings"
)

type modelSet map[string]bool
type modelACL map[string]modelSet

// Decode objects without silently accepting duplicate keys or trailing JSON.
func uniqueObject(reader io.Reader) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(reader)
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected object")
	}
	result := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected key")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate key")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return result, nil
}

func loadModelACL(path string) (modelACL, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("unable to open MODEL_ACL_FILE")
	}
	defer file.Close()
	entries, err := uniqueObject(file)
	if err != nil {
		return nil, fmt.Errorf("MODEL_ACL_FILE must be a JSON object with unique keys")
	}
	acl := make(modelACL, len(entries))
	for key, raw := range entries {
		if key == "" || strings.ContainsAny(key, " \t\r\n") {
			return nil, fmt.Errorf("MODEL_ACL_FILE contains an invalid API key")
		}
		var models []string
		if err := json.Unmarshal(raw, &models); err != nil || models == nil {
			return nil, fmt.Errorf("MODEL_ACL_FILE values must be arrays of model names")
		}
		allowed := make(modelSet, len(models))
		for _, model := range models {
			if model == "" || strings.TrimSpace(model) != model {
				return nil, fmt.Errorf("MODEL_ACL_FILE contains an invalid model name")
			}
			allowed[model] = true
		}
		acl[key] = allowed
	}
	return acl, nil
}

func (acl modelACL) authenticate(r *http.Request) (modelSet, *httpError) {
	if acl == nil {
		return nil, nil
	}
	key, err := requestAPIKey(r)
	if err != nil {
		return nil, err
	}
	allowed, exists := acl[key]
	if !exists {
		return nil, &httpError{401, "invalid API key"}
	}
	return allowed, nil
}

func requestAPIKey(r *http.Request) (string, *httpError) {
	// Accept one credential source only, so upstream credential precedence cannot
	// select a different key from the one checked here.
	key := ""
	sources := 0
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		values := r.Header.Values(name)
		for _, value := range values {
			sources++
			if name == "Authorization" {
				fields := strings.Fields(value)
				if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
					return "", &httpError{401, "invalid API key"}
				}
				key = fields[1]
			} else {
				key = value
			}
		}
	}
	// Query credentials are deliberately unsupported.
	for name := range r.URL.Query() {
		if strings.EqualFold(name, "key") || strings.EqualFold(name, "api_key") || strings.EqualFold(name, "access_token") {
			return "", &httpError{401, "query API keys are not supported"}
		}
	}
	if sources != 1 || key == "" {
		return "", &httpError{401, "invalid API key"}
	}
	return key, nil
}

func (policy *modelPolicy) authorize(r *http.Request, body *spooledBody) *httpError {
	if policy.unrestricted() {
		return nil
	}
	denied := &httpError{403, "model or endpoint is not allowed for this API key"}
	// Only explicitly understood routes are safe for restricted keys. In
	// particular, stored responses and WebSocket sessions can omit the model.
	if r.URL.RawQuery != "" || r.URL.RawPath != "" {
		return denied
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		return nil
	}
	if r.Method != http.MethodPost {
		return denied
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/embeddings", "/v1/messages", "/v1/messages/count_tokens", "/v1/images/generations", "/v1/audio/speech", "/v1/moderations":
	default:
		return denied
	}
	if r.Header.Get("Content-Encoding") != "" || r.Header.Get("Upgrade") != "" {
		return denied
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return &httpError{415, "ACL requires application/json"}
	}
	if body == nil {
		return &httpError{400, "a JSON body with an explicit model is required"}
	}
	var reader io.Reader = bytes.NewReader(body.data)
	if body.path != "" {
		file, err := os.Open(body.path)
		if err != nil {
			return &httpError{500, "unable to inspect request body"}
		}
		defer file.Close()
		reader = file
	}
	fields, err := uniqueObject(reader)
	if err != nil {
		return &httpError{400, "invalid or ambiguous JSON request"}
	}
	for name := range fields {
		if name != "model" && strings.EqualFold(name, "model") {
			return &httpError{400, "model field must use exact lowercase spelling"}
		}
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model == "" {
		return &httpError{400, "an explicit model string is required"}
	}
	if !policy.permits(model) {
		return denied
	}
	return nil
}

// This list describes configured permissions, not upstream availability.
func (allowed modelSet) writeModels(w http.ResponseWriter) {
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	sort.Strings(names)
	data := make([]map[string]string, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]string{"id": name, "object": "model", "owned_by": "cpa-guard"})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Cpa-Guard-Attempts", "0")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
