package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultUpstreamURL       = "http://127.0.0.1:8317"
	defaultAppPort           = "8217"
	defaultMaxAttempts       = 100
	maxAllowedAttempts       = 500
	defaultMaxElapsed        = 55 * time.Minute
	defaultMaxBodyBytes      = 128 << 20
	defaultMemoryBodyBytes   = 2 << 20
	defaultInitialBackoff    = 250 * time.Millisecond
	defaultMaxBackoff        = 5 * time.Second
	defaultBackoffJitter     = 0.20
	defaultRetryAfterCap     = 60 * time.Second
	defaultDrainBytes        = 1 << 20
	defaultMaxInflight       = 256
	defaultConnectTimeout    = 10 * time.Second
	defaultServerReadHeader  = 30 * time.Second
	defaultServerIdleTimeout = 2 * time.Minute
)

// version is replaced by the release workflow. Keeping a value in the source
// makes local builds and diagnostics explicit.
var version = "dev"

// Config contains all runtime controls. Durations are configured in
// milliseconds so the same values can be used from a Kubernetes ConfigMap or
// a systemd EnvironmentFile.
type Config struct {
	ListenAddr            string
	Upstream              *url.URL
	MaxAttempts           int
	MaxElapsed            time.Duration
	MaxBodyBytes          int64
	MemoryBodyBytes       int64
	InitialBackoff        time.Duration
	MaxBackoff            time.Duration
	BackoffJitter         float64
	RetryAfterCap         time.Duration
	MaxResponseDrainBytes int64
	MaxInflight           int
	ConnectTimeout        time.Duration
	HeaderTimeout         time.Duration
	MetricsEnabled        bool
	BodyDir               string
	Debug                 bool
	RetryStatuses         map[int]struct{}
}

func loadConfig() (Config, error) {
	upstreamText := envString("UPSTREAM_URL", defaultUpstreamURL)
	upstream, err := url.Parse(upstreamText)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return Config{}, fmt.Errorf("invalid UPSTREAM_URL")
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return Config{}, fmt.Errorf("UPSTREAM_URL must use http or https")
	}

	maxAttempts, err := envInt("MAX_ATTEMPTS", defaultMaxAttempts, 1, maxAllowedAttempts)
	if err != nil {
		return Config{}, err
	}
	maxElapsed, err := envDurationMS("MAX_ELAPSED_MS", defaultMaxElapsed)
	if err != nil {
		return Config{}, err
	}
	maxBody, err := envInt64("MAX_BODY_BYTES", defaultMaxBodyBytes, 1, 1<<31)
	if err != nil {
		return Config{}, err
	}
	memoryBody, err := envInt64("MEMORY_BODY_BYTES", defaultMemoryBodyBytes, 0, 1<<31)
	if err != nil {
		return Config{}, err
	}
	if memoryBody > maxBody {
		memoryBody = maxBody
	}
	initialBackoff, err := envDurationMS("INITIAL_BACKOFF_MS", defaultInitialBackoff)
	if err != nil {
		return Config{}, err
	}
	maxBackoff, err := envDurationMS("MAX_BACKOFF_MS", defaultMaxBackoff)
	if err != nil {
		return Config{}, err
	}
	if maxBackoff < initialBackoff {
		return Config{}, fmt.Errorf("MAX_BACKOFF_MS must be >= INITIAL_BACKOFF_MS")
	}
	jitter, err := envFloat("BACKOFF_JITTER", defaultBackoffJitter, 0, 1)
	if err != nil {
		return Config{}, err
	}
	retryAfterCap, err := envDurationMS("RETRY_AFTER_CAP_MS", defaultRetryAfterCap)
	if err != nil {
		return Config{}, err
	}
	drainBytes, err := envInt64("MAX_RESPONSE_DRAIN_BYTES", defaultDrainBytes, 0, 1<<31)
	if err != nil {
		return Config{}, err
	}
	maxInflight, err := envInt("MAX_INFLIGHT", defaultMaxInflight, 0, 100000)
	if err != nil {
		return Config{}, err
	}
	connectTimeout, err := envDurationMS("CONNECT_TIMEOUT_MS", defaultConnectTimeout)
	if err != nil {
		return Config{}, err
	}
	headerTimeout, err := envDurationMS("UPSTREAM_HEADER_TIMEOUT_MS", 0)
	if err != nil {
		return Config{}, err
	}

	statuses := map[int]struct{}{502: {}, 503: {}, 504: {}}
	if raw := strings.TrimSpace(os.Getenv("RETRY_STATUSES")); raw != "" {
		statuses = make(map[int]struct{})
		for _, item := range strings.Split(raw, ",") {
			status, parseErr := strconv.Atoi(strings.TrimSpace(item))
			if parseErr != nil || status < 100 || status > 599 {
				return Config{}, fmt.Errorf("invalid RETRY_STATUSES value")
			}
			statuses[status] = struct{}{}
		}
	}

	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		appPort := envString("APP_PORT", defaultAppPort)
		parsedPort, portErr := strconv.Atoi(appPort)
		if portErr != nil || parsedPort < 1 || parsedPort > 65535 {
			return Config{}, fmt.Errorf("APP_PORT must be an integer between 1 and 65535")
		}
		listenAddr = "0.0.0.0:" + appPort
	}

	return Config{
		ListenAddr:            listenAddr,
		Upstream:              upstream,
		MaxAttempts:           maxAttempts,
		MaxElapsed:            maxElapsed,
		MaxBodyBytes:          maxBody,
		MemoryBodyBytes:       memoryBody,
		InitialBackoff:        initialBackoff,
		MaxBackoff:            maxBackoff,
		BackoffJitter:         jitter,
		RetryAfterCap:         retryAfterCap,
		MaxResponseDrainBytes: drainBytes,
		MaxInflight:           maxInflight,
		ConnectTimeout:        connectTimeout,
		HeaderTimeout:         headerTimeout,
		MetricsEnabled:        envBool("METRICS_ENABLED", false),
		BodyDir:               envString("BODY_DIR", os.TempDir()),
		Debug:                 envBool("DEBUG", false),
		RetryStatuses:         statuses,
	}, nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback, min, max int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, min, max)
	}
	return parsed, nil
}

func envInt64(name string, fallback, min, max int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, min, max)
	}
	return parsed, nil
}

func envDurationMS(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer in milliseconds", name)
	}
	return time.Duration(parsed) * time.Millisecond, nil
}

func envFloat(name string, fallback, min, max float64) (float64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be a number between %g and %g", name, min, max)
	}
	return parsed, nil
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

type httpError struct {
	Status  int
	Message string
}

func (e *httpError) Error() string { return e.Message }

type spooledBody struct {
	dir  string
	path string
	size int64
	data []byte
}

func (b *spooledBody) cleanup() {
	if b == nil || b.dir == "" {
		return
	}
	_ = os.RemoveAll(b.dir)
}

func spoolBody(r *http.Request, cfg Config) (*spooledBody, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, nil
	}
	if r.ContentLength > cfg.MaxBodyBytes {
		return nil, &httpError{Status: http.StatusRequestEntityTooLarge, Message: "request body exceeds configured limit"}
	}
	if cfg.MemoryBodyBytes > 0 && r.ContentLength >= 0 && r.ContentLength <= cfg.MemoryBodyBytes {
		data, readErr := io.ReadAll(io.LimitReader(r.Body, cfg.MaxBodyBytes+1))
		if readErr != nil {
			return nil, fmt.Errorf("read request body in memory: %w", readErr)
		}
		if int64(len(data)) > cfg.MaxBodyBytes {
			return nil, &httpError{Status: http.StatusRequestEntityTooLarge, Message: "request body exceeds configured limit"}
		}
		return &spooledBody{size: int64(len(data)), data: data}, nil
	}

	dir, err := os.MkdirTemp(cfg.BodyDir, "cpa-guard-")
	if err != nil {
		return nil, fmt.Errorf("create request spool directory: %w", err)
	}
	path := filepath.Join(dir, "body")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create request spool file: %w", err)
	}

	limited := io.LimitReader(r.Body, cfg.MaxBodyBytes+1)
	size, copyErr := io.Copy(file, limited)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("read request body to spool: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("close request spool file: %w", closeErr)
	}
	if size > cfg.MaxBodyBytes {
		_ = os.RemoveAll(dir)
		return nil, &httpError{Status: http.StatusRequestEntityTooLarge, Message: "request body exceeds configured limit"}
	}
	return &spooledBody{dir: dir, path: path, size: size}, nil
}

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"expect":              {},
}

func cloneRequestHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		if _, hop := hopByHopHeaders[strings.ToLower(key)]; hop || strings.EqualFold(key, "host") || strings.EqualFold(key, "content-length") {
			continue
		}
		dst[key] = append([]string(nil), values...)
	}
	// RFC 7230 allows Connection to nominate additional hop-by-hop headers.
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			dst.Del(strings.TrimSpace(token))
		}
	}
	return dst
}

func targetURL(base *url.URL, r *http.Request) *url.URL {
	target := *base
	prefix := strings.TrimRight(base.Path, "/")
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	target.Path = prefix + path
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	return &target
}

type attemptResult struct {
	response *http.Response
	err      error
	release  func()
}

func (p *Proxy) doAttempt(incoming *http.Request, body *spooledBody, attempt int, deadline time.Time) attemptResult {
	ctx, cancel := context.WithCancel(incoming.Context())
	released := false
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if !released {
			released = true
			cancel()
		}
	}

	var deadlineTimer *time.Timer
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			release()
			return attemptResult{err: context.DeadlineExceeded}
		}
		deadlineTimer = time.AfterFunc(remaining, cancel)
	}

	target := targetURL(p.cfg.Upstream, incoming)
	var reader io.Reader
	var bodyFile *os.File
	if body != nil {
		if body.path != "" {
			var err error
			bodyFile, err = os.Open(body.path)
			if err != nil {
				if deadlineTimer != nil {
					deadlineTimer.Stop()
				}
				release()
				return attemptResult{err: err}
			}
			reader = bodyFile
		} else {
			reader = bytes.NewReader(body.data)
		}
	}

	upstreamRequest, err := http.NewRequestWithContext(ctx, incoming.Method, target.String(), reader)
	if err != nil {
		if bodyFile != nil {
			_ = bodyFile.Close()
		}
		if deadlineTimer != nil {
			deadlineTimer.Stop()
		}
		release()
		return attemptResult{err: err}
	}
	upstreamRequest.Header = cloneRequestHeaders(incoming.Header)
	if body != nil {
		upstreamRequest.ContentLength = body.size
		upstreamRequest.Header.Set("Content-Length", strconv.FormatInt(body.size, 10))
	} else {
		upstreamRequest.ContentLength = 0
	}
	if incoming.Host != "" {
		// Preserve the Host that CPA currently receives from ingress-nginx.
		upstreamRequest.Host = incoming.Host
	}

	response, err := p.client.Do(upstreamRequest)
	if bodyFile != nil {
		_ = bodyFile.Close()
	}
	if deadlineTimer != nil {
		deadlineTimer.Stop()
	}
	if err != nil {
		release()
		return attemptResult{err: err}
	}
	return attemptResult{response: response, release: release}
}

func (p *Proxy) canRetry(attempt int, deadline time.Time, delay time.Duration) bool {
	if attempt >= p.cfg.MaxAttempts {
		return false
	}
	if deadline.IsZero() {
		return true
	}
	return time.Now().Add(delay).Before(deadline)
}

func (p *Proxy) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if p.cfg.RetryAfterCap > 0 && retryAfter > p.cfg.RetryAfterCap {
			return p.cfg.RetryAfterCap
		}
		return retryAfter
	}

	delay := p.cfg.InitialBackoff
	for index := 1; index < attempt && delay < p.cfg.MaxBackoff; index++ {
		if delay > p.cfg.MaxBackoff/2 {
			delay = p.cfg.MaxBackoff
			break
		}
		delay *= 2
	}
	if delay > p.cfg.MaxBackoff {
		delay = p.cfg.MaxBackoff
	}
	if p.cfg.BackoffJitter == 0 || delay == 0 {
		return delay
	}
	factor := 1 + ((mathrand.Float64()*2 - 1) * p.cfg.BackoffJitter)
	if factor < 0 {
		factor = 0
	}
	return time.Duration(float64(delay) * factor)
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func drainResponse(response *http.Response, limit int64) {
	if response == nil || response.Body == nil {
		return
	}
	if limit > 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, limit))
	} else {
		_, _ = io.Copy(io.Discard, response.Body)
	}
	_ = response.Body.Close()
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func responseHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, hop := hopByHopHeaders[strings.ToLower(key)]; hop {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type flushWriter struct {
	w http.ResponseWriter
}

func (f flushWriter) Write(data []byte) (int, error) {
	n, err := f.w.Write(data)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func forwardResponse(w http.ResponseWriter, response *http.Response, attempts int) (int, error) {
	defer response.Body.Close()
	responseHeaders(w.Header(), response.Header)
	w.Header().Set("X-Cpa-Guard-Attempts", strconv.Itoa(attempts))
	status := response.StatusCode
	w.WriteHeader(status)
	_, err := io.Copy(flushWriter{w: w}, response.Body)
	return status, err
}

func sendJSONError(w http.ResponseWriter, status int, message string, attempts int) {
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"type":    "cpa_guard_error",
			"message": message,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	if attempts > 0 {
		w.Header().Set("X-Cpa-Guard-Attempts", strconv.Itoa(attempts))
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

type metrics struct {
	requests        atomic.Uint64
	attempts        atomic.Uint64
	retries         atomic.Uint64
	retry502        atomic.Uint64
	retry503        atomic.Uint64
	retry504        atomic.Uint64
	retryNetwork    atomic.Uint64
	clientCancelled atomic.Uint64
	inflight        atomic.Int64
	statusClasses   [5]atomic.Uint64
}

func (m *metrics) recordStatus(status int) {
	class := status / 100
	if class >= 1 && class <= 5 {
		m.statusClasses[class-1].Add(1)
	}
}

func (m *metrics) render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP cpa_guard_requests_total API requests handled by cpa-guard.\n# TYPE cpa_guard_requests_total counter\ncpa_guard_requests_total %d\n", m.requests.Load())
	fmt.Fprintf(w, "# HELP cpa_guard_attempts_total Upstream attempts, including the initial attempt.\n# TYPE cpa_guard_attempts_total counter\ncpa_guard_attempts_total %d\n", m.attempts.Load())
	fmt.Fprintf(w, "# HELP cpa_guard_retries_total Upstream retries performed.\n# TYPE cpa_guard_retries_total counter\ncpa_guard_retries_total %d\n", m.retries.Load())
	fmt.Fprintf(w, "# TYPE cpa_guard_retry_responses_total counter\ncpa_guard_retry_responses_total{status=\"502\"} %d\ncpa_guard_retry_responses_total{status=\"503\"} %d\ncpa_guard_retry_responses_total{status=\"504\"} %d\n", m.retry502.Load(), m.retry503.Load(), m.retry504.Load())
	fmt.Fprintf(w, "# TYPE cpa_guard_retry_network_errors_total counter\ncpa_guard_retry_network_errors_total %d\n", m.retryNetwork.Load())
	fmt.Fprintf(w, "# TYPE cpa_guard_client_cancellations_total counter\ncpa_guard_client_cancellations_total %d\n", m.clientCancelled.Load())
	fmt.Fprintf(w, "# TYPE cpa_guard_inflight_requests gauge\ncpa_guard_inflight_requests %d\n", m.inflight.Load())
	fmt.Fprintln(w, "# TYPE cpa_guard_responses_total counter")
	for index := 1; index <= 5; index++ {
		fmt.Fprintf(w, "cpa_guard_responses_total{status_class=\"%dxx\"} %d\n", index, m.statusClasses[index-1].Load())
	}
}

type Proxy struct {
	cfg      Config
	client   *http.Client
	metrics  metrics
	inflight chan struct{}
}

func newProxy(cfg Config) *Proxy {
	maxSockets := cfg.MaxInflight
	if maxSockets <= 0 {
		maxSockets = 256
	}
	transport := &http.Transport{
		// The upstream is fixed by configuration. Do not accidentally route
		// CPA traffic through a process-wide HTTP proxy inherited from the pod.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxSockets,
		MaxIdleConnsPerHost:   maxSockets,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ResponseHeaderTimeout: cfg.HeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy := &Proxy{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if cfg.MaxInflight > 0 {
		proxy.inflight = make(chan struct{}, cfg.MaxInflight)
	}
	return proxy
}

func (p *Proxy) acquire() bool {
	if p.inflight == nil {
		p.metrics.inflight.Add(1)
		return true
	}
	select {
	case p.inflight <- struct{}{}:
		p.metrics.inflight.Add(1)
		return true
	default:
		return false
	}
}

func (p *Proxy) release() {
	if p.inflight != nil {
		<-p.inflight
	}
	p.metrics.inflight.Add(-1)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz", "/readyz":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "ok\n")
		}
		return
	case "/metrics":
		if !p.cfg.MetricsEnabled {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		p.metrics.render(w)
		return
	}

	p.metrics.requests.Add(1)
	if r.Body != nil {
		defer r.Body.Close()
	}
	requestID := newRequestID()
	path := requestLogPath(r)
	w.Header().Set("X-Cpa-Guard-Request-ID", requestID)
	start := time.Now()
	if !p.acquire() {
		sendJSONError(w, http.StatusServiceUnavailable, "retry capacity is currently full", 0)
		p.logRequest(requestID, r.Method, path, http.StatusServiceUnavailable, 0, "capacity_full", start)
		return
	}
	defer p.release()

	body, err := spoolBody(r, p.cfg)
	if body != nil {
		defer body.cleanup()
	}
	if err != nil {
		p.logRequestBodyDebug(requestID, r, err)
		var requestErr *httpError
		if errors.As(err, &requestErr) {
			sendJSONError(w, requestErr.Status, requestErr.Message, 0)
			p.metrics.recordStatus(requestErr.Status)
			p.logRequest(requestID, r.Method, path, requestErr.Status, 0, "request_rejected", start)
			return
		}
		if r.Context().Err() != nil {
			p.metrics.clientCancelled.Add(1)
			p.logRequest(requestID, r.Method, path, 0, 0, "client_cancelled", start)
			return
		}
		sendJSONError(w, http.StatusBadRequest, "unable to read request body", 0)
		p.metrics.recordStatus(http.StatusBadRequest)
		p.logRequest(requestID, r.Method, path, http.StatusBadRequest, 0, "request_body_error", start)
		return
	}

	status, attempts, outcome := p.forwardWithRetries(w, r, body)
	if status > 0 {
		p.metrics.recordStatus(status)
	}
	if outcome == "client_cancelled" {
		p.metrics.clientCancelled.Add(1)
	}
	p.logRequest(requestID, r.Method, path, status, attempts, outcome, start)
}

func (p *Proxy) forwardWithRetries(w http.ResponseWriter, incoming *http.Request, body *spooledBody) (int, int, string) {
	deadline := time.Time{}
	if p.cfg.MaxElapsed > 0 {
		deadline = time.Now().Add(p.cfg.MaxElapsed)
	}
	attemptsMade := 0

	for attempt := 1; attempt <= p.cfg.MaxAttempts; attempt++ {
		if incoming.Context().Err() != nil {
			return 0, attempt - 1, "client_cancelled"
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		attemptsMade = attempt
		p.metrics.attempts.Add(1)
		result := p.doAttempt(incoming, body, attempt, deadline)
		if result.err != nil {
			if incoming.Context().Err() != nil {
				return 0, attempt, "client_cancelled"
			}
			if !p.canRetry(attempt, deadline, p.retryDelay(attempt, 0)) {
				break
			}
			delay := p.retryDelay(attempt, 0)
			p.metrics.retries.Add(1)
			p.metrics.retryNetwork.Add(1)
			if err := sleepContext(incoming.Context(), delay); err != nil {
				if incoming.Context().Err() != nil {
					return 0, attempt, "client_cancelled"
				}
				break
			}
			continue
		}

		response := result.response
		if _, retryable := p.cfg.RetryStatuses[response.StatusCode]; !retryable {
			status, copyErr := forwardResponse(w, response, attempt)
			result.release()
			if copyErr != nil && incoming.Context().Err() != nil {
				return status, attempt, "client_cancelled"
			}
			if copyErr != nil {
				return status, attempt, "response_stream_error"
			}
			return status, attempt, "response"
		}

		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
		delay := p.retryDelay(attempt, retryAfter)
		if !p.canRetry(attempt, deadline, delay) {
			status, copyErr := forwardResponse(w, response, attempt)
			result.release()
			if copyErr != nil && incoming.Context().Err() != nil {
				return status, attempt, "client_cancelled"
			}
			return status, attempt, "retry_budget_exhausted"
		}

		switch response.StatusCode {
		case http.StatusBadGateway:
			p.metrics.retry502.Add(1)
		case http.StatusServiceUnavailable:
			p.metrics.retry503.Add(1)
		case http.StatusGatewayTimeout:
			p.metrics.retry504.Add(1)
		}
		p.metrics.retries.Add(1)
		drainResponse(response, p.cfg.MaxResponseDrainBytes)
		result.release()
		if err := sleepContext(incoming.Context(), delay); err != nil {
			if incoming.Context().Err() != nil {
				return 0, attempt, "client_cancelled"
			}
			break
		}
	}

	if incoming.Context().Err() != nil {
		return 0, attemptsMade, "client_cancelled"
	}
	// Deliberately do not expose upstream error details to clients.
	sendJSONError(w, http.StatusServiceUnavailable, "upstream unavailable after retry budget", attemptsMade)
	return http.StatusServiceUnavailable, attemptsMade, "retry_budget_exhausted"
}

func requestLogPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/"
	}
	path := r.URL.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// logRequestBodyDebug emits diagnostics for failures that happen before an
// upstream attempt. It intentionally records request metadata only: request
// bodies, authorization headers, and query parameters must never be logged.
func (p *Proxy) logRequestBodyDebug(requestID string, r *http.Request, err error) {
	if !p.cfg.Debug {
		return
	}

	entry := map[string]any{
		"event":             "request_body_debug",
		"request_id":        requestID,
		"method":            r.Method,
		"path":              requestLogPath(r),
		"content_length":    r.ContentLength,
		"transfer_encoding": append([]string(nil), r.TransferEncoding...),
		"content_type":      r.Header.Get("Content-Type"),
		"content_encoding":  r.Header.Get("Content-Encoding"),
		"expect":            r.Header.Get("Expect"),
		"max_body_bytes":    p.cfg.MaxBodyBytes,
		"memory_body_bytes": p.cfg.MemoryBodyBytes,
		"body_dir":          p.cfg.BodyDir,
		"error":             err.Error(),
	}
	payload, _ := json.Marshal(entry)
	fmt.Println(string(payload))
}

func (p *Proxy) logRequest(requestID, method, path string, status, attempts int, outcome string, start time.Time) {
	entry := map[string]any{
		"event":       "request",
		"request_id":  requestID,
		"method":      method,
		"path":        path,
		"status":      status,
		"attempts":    attempts,
		"outcome":     outcome,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	payload, _ := json.Marshal(entry)
	fmt.Println(string(payload))
}

func newRequestID() string {
	var data [16]byte
	if _, err := cryptorand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(2)
	}
	proxy := newProxy(cfg)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           proxy,
		ReadHeaderTimeout: defaultServerReadHeader,
		IdleTimeout:       defaultServerIdleTimeout,
		MaxHeaderBytes:    64 << 10,
	}

	go func() {
		fmt.Printf("cpa-guard %s listening on %s, upstream=%s, max_attempts=%d\n", version, cfg.ListenAddr, cfg.Upstream.Redacted(), cfg.MaxAttempts)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}()

	shutdown := make(chan os.Signal, 1)
	// Signal handling is kept in a small goroutine so active streaming
	// responses can finish during the normal termination grace period.
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	<-shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
