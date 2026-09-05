package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type modelRule struct {
	AllowKeys []string `toml:"allow_keys"`
}

type modelRules map[string]modelRule

const defaultModelACLFile = "model-acl.toml"

type modelPolicy struct {
	// Legacy JSON uses a per-key allow list; TOML computes denied models per key.
	allow modelSet
	deny  modelSet
}

func (p *modelPolicy) unrestricted() bool {
	if p.allow != nil {
		return p.allow["*"]
	}
	return len(p.deny) == 0
}

func (p *modelPolicy) permits(model string) bool {
	if p.allow != nil {
		return p.allow["*"] || p.allow[model]
	}
	return !p.deny[model]
}

func loadACLConfig(path string) (modelACL, modelRules, error) {
	if path == "" {
		path = defaultModelACLFile
		// Only an absent default file disables ACL. A broken symlink,
		// unreadable file, or malformed policy must still prevent startup.
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return nil, nil, nil
		} else if err != nil {
			return nil, nil, fmt.Errorf("unable to inspect default model-acl.toml")
		}
	}
	// Preserve JSON loading for existing files, including extensionless paths.
	if !strings.EqualFold(filepath.Ext(path), ".toml") {
		acl, err := loadModelACL(path)
		return acl, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to open MODEL_ACL_FILE")
	}
	defer file.Close()
	var config struct {
		Models modelRules `toml:"models"`
	}
	metadata, err := toml.NewDecoder(file).Decode(&config)
	// Parser diagnostics can include secret key values, so never surface them.
	if err != nil || len(metadata.Undecoded()) != 0 {
		return nil, nil, fmt.Errorf("MODEL_ACL_FILE must be valid TOML with only models tables and allow_keys arrays")
	}
	if config.Models == nil {
		config.Models = make(modelRules)
	}
	for model, rule := range config.Models {
		if model == "" || model == "*" || strings.TrimSpace(model) != model {
			return nil, nil, fmt.Errorf("MODEL_ACL_FILE contains an invalid model name")
		}
		if !metadata.IsDefined("models", model, "allow_keys") {
			return nil, nil, fmt.Errorf("MODEL_ACL_FILE model rules require allow_keys")
		}
		for _, key := range rule.AllowKeys {
			if key == "" || strings.ContainsAny(key, " \t\r\n") {
				return nil, nil, fmt.Errorf("MODEL_ACL_FILE contains an invalid API key")
			}
		}
	}
	return nil, config.Models, nil
}

func (cfg Config) authenticateACL(r *http.Request) (*modelPolicy, *httpError) {
	if cfg.ModelRules == nil {
		allowed, err := cfg.ModelACL.authenticate(r)
		if err != nil || allowed == nil {
			return nil, err
		}
		return &modelPolicy{allow: allowed}, nil
	}
	key, err := requestAPIKey(r)
	if err != nil {
		return nil, err
	}
	denied := make(modelSet)
	for model, rule := range cfg.ModelRules {
		permitted := false
		for _, allowedKey := range rule.AllowKeys {
			if allowedKey == "*" || allowedKey == key {
				permitted = true
				break
			}
		}
		if !permitted {
			denied[model] = true
		}
	}
	return &modelPolicy{deny: denied}, nil
}

// Only forward a completely validated list. A malformed or oversized upstream
// response must never cause the unfiltered list to be sent instead.
func filterModelList(response *http.Response, policy *modelPolicy, limit int64) error {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("expected complete model list")
	}
	if response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		return fmt.Errorf("encoded model list")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return fmt.Errorf("unable to read model list")
	}
	envelope, err := uniqueObject(bytes.NewReader(data))
	if err != nil {
		return err
	}
	var models []json.RawMessage
	if err := json.Unmarshal(envelope["data"], &models); err != nil || models == nil {
		return fmt.Errorf("invalid model list")
	}
	filtered := make([]json.RawMessage, 0, len(models))
	for _, entry := range models {
		fields, err := uniqueObject(bytes.NewReader(entry))
		if err != nil {
			return err
		}
		var id string
		if err := json.Unmarshal(fields["id"], &id); err != nil || id == "" {
			return fmt.Errorf("invalid model id")
		}
		if policy.permits(id) {
			filtered = append(filtered, entry)
		}
	}
	envelope["data"], err = json.Marshal(filtered)
	if err != nil {
		return err
	}
	output, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(output))
	response.ContentLength = int64(len(output))
	for _, name := range []string{"Content-Length", "ETag", "Last-Modified", "Content-Encoding", "Content-MD5", "Digest"} {
		response.Header.Del(name)
	}
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Cache-Control", "private, no-store")
	return nil
}
