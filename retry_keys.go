package main

import "net/http"

// Compute a request-local budget; never mutate the shared proxy config.
func (cfg Config) requestMaxAttempts(r *http.Request) int {
	if cfg.RetryKeys == nil || cfg.RetryKeys["*"] {
		return cfg.MaxAttempts
	}
	key, err := requestAPIKey(r)
	if err != nil || !cfg.RetryKeys[key] {
		return 1
	}
	return cfg.MaxAttempts
}
