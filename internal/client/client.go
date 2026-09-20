package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultStreamIdleTimeout bounds how long a streaming response may stay
// completely silent before it is considered stalled and aborted. The abort
// surfaces as a regular stream error, which the executor's retry tier
// handles. 0 disables the watchdog.
var defaultStreamIdleTimeout = 120 * time.Second

// SetStreamIdleTimeout overrides defaultStreamIdleTimeout (test use).
func SetStreamIdleTimeout(d time.Duration) { defaultStreamIdleTimeout = d }

// defaultRequestTimeout bounds a single non-streaming chat completion request
// (connect plus full response). A server that accepts the connection and never
// responds would otherwise block the caller for the OS TCP lifetime. 0
// disables the bound.
var defaultRequestTimeout = 5 * time.Minute

// SetRequestTimeout overrides defaultRequestTimeout (test use).
func SetRequestTimeout(d time.Duration) { defaultRequestTimeout = d }

type Config struct {
	BaseURL      string
	APIKey       string
	Model        string
	Timeout      time.Duration
	EnableImages bool
	LogitBias    map[string]int
	AppVersion   string
	UserAgent    string
}

type BackendType string

const (
	BackendUnknown       BackendType = "unknown"
	BackendLlamaCPP      BackendType = "llama.cpp"
	BackendGenericOpenAI BackendType = "openai"
)

type Client struct {
	mu             sync.RWMutex
	cfg            Config
	httpClient     *http.Client
	backend        BackendType
	ctxSize        int
	supportsVision bool
	userAgent      string
}

func NewClient(cfg Config) *Client {
	ua := cfg.UserAgent
	if ua == "" {
		ver := cfg.AppVersion
		if ver == "" {
			ver = "dev"
		}
		ua = fmt.Sprintf("late-cli/%s (+https://github.com/mlhher/late-cli)", ver)
	}

	return &Client{
		cfg:       cfg,
		backend:   BackendUnknown,
		ctxSize:   -1, // -1 means unknown or not applicable
		userAgent: ua,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
			// No client-level timeout: streaming responses are legitimately
			// long-lived, so stalls are bounded by the stream idle watchdog
			// (defaultStreamIdleTimeout) and non-streaming requests by
			// defaultRequestTimeout instead.
			Timeout: 0,
		},
	}
}

func (c *Client) applyHeaders(req *http.Request) {
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if strings.Contains(strings.ToLower(c.cfg.BaseURL), "openrouter.ai") {
		req.Header.Set("HTTP-Referer", "https://github.com/mlhher/late-cli")
		req.Header.Set("X-OpenRouter-Title", "Late-CLI")
		req.Header.Set("X-OpenRouter-Categories", "cli-agent")
	}
}

// chatCompletionURL builds the chat completions endpoint from BaseURL.
// If the base URL has no path (e.g. "http://localhost:8080"), /v1 is
// appended automatically for backwards compatibility. If a path is already
// present (e.g. "https://api.z.ai/api/coding/paas/v4"), it is used as-is
// and only /chat/completions is appended — consistent with the OpenAI SDK
// convention that base_url is a true base the caller controls.
func (c *Client) chatCompletionURL() string {
	base := strings.TrimSuffix(c.cfg.BaseURL, "/")
	if u, err := url.Parse(base); err == nil && (u.Path == "" || u.Path == "/") {
		base += "/v1"
	}
	return base + "/chat/completions"
}

// ChatCompletion sends a chat prompt to the OpenAI-compatible endpoint.
func (c *Client) ChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if c.getBackend() == BackendUnknown || (c.getBackend() == BackendLlamaCPP && c.ContextSize() == -1) {
		_ = c.DiscoverBackend(ctx)
	}

	if req.Model == "" && c.cfg.Model != "" {
		req.Model = c.cfg.Model
	}

	req.LogitBias = c.mergeLogitBias(req.LogitBias)

	body, err := c.marshalFlattened(req)
	if err != nil {
		return nil, err
	}

	url := c.chatCompletionURL()

	// Bound the non-streaming request so a server that accepts the connection
	// and never responds cannot block the caller for the OS TCP lifetime. 5
	// minutes is generous for a non-stream completion; if the deadline elapses
	// the error surfaces normally through the request error path below. A
	// non-positive defaultRequestTimeout disables the bound.
	reqCtx := ctx
	if defaultRequestTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	c.applyHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.formatError(resp)
	}

	var chatResp ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, err
	}
	return &chatResp, nil
}

// ChatCompletionStream streams responses from the OpenAI-compatible endpoint.
func (c *Client) ChatCompletionStream(ctx context.Context, req ChatCompletionRequest) (<-chan ChatCompletionChunk, <-chan error) {
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}
	out := make(chan ChatCompletionChunk)
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errCh)

		// streamCtx owns this request so the idle watchdog (spawned below,
		// before the scan loop) can abort an in-flight body read when the
		// provider goes silent. The deferred scancel lives in this goroutine —
		// the same owner as close(out)/close(errCh) — so the watchdog can
		// never outlive the request.
		streamCtx, scancel := context.WithCancel(ctx)
		defer scancel()

		if c.getBackend() == BackendUnknown || (c.getBackend() == BackendLlamaCPP && c.ContextSize() == -1) {
			_ = c.DiscoverBackend(ctx)
		}

		if req.Model == "" && c.cfg.Model != "" {
			req.Model = c.cfg.Model
		}

		req.LogitBias = c.mergeLogitBias(req.LogitBias)

		body, err := c.marshalFlattened(req)
		if err != nil {
			errCh <- err
			return
		}

		url := c.chatCompletionURL()
		httpReq, err := http.NewRequestWithContext(streamCtx, "POST", url, bytes.NewBuffer(body))
		if err != nil {
			errCh <- err
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		if c.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
		c.applyHeaders(httpReq)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errCh <- c.formatError(resp)
			return
		}

		// lastRead is the last time the response body made real progress (a
		// full line was consumed). It is seeded here at response time and
		// updated after every scanned line below — never while blocked inside
		// scanner.Scan(), which is precisely the stalled state the watchdog
		// must detect.
		var lastRead int64
		atomic.StoreInt64(&lastRead, time.Now().UnixNano())

		// Idle watchdog: without it, a provider that accepts the request and
		// then streams nothing (half-open connection, stalled backend) blocks
		// scanner.Scan() for the OS TCP lifetime, and to the caller that is
		// indistinguishable from the model "thinking". If no line arrives
		// within defaultStreamIdleTimeout, cancel streamCtx so the in-flight
		// body read fails; the resulting error flows through the normal
		// scanner.Err() path below as a regular stream error, which the
		// executor's retry tier handles. A non-positive
		// defaultStreamIdleTimeout disables the watchdog.
		if defaultStreamIdleTimeout > 0 {
			watchdogDone := make(chan struct{})
			go func() {
				defer close(watchdogDone)
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-streamCtx.Done():
						return
					case <-ticker.C:
						last := time.Unix(0, atomic.LoadInt64(&lastRead))
						if time.Since(last) > defaultStreamIdleTimeout {
							// Deliberate cancellation, not a client bug: the
							// idle watchdog fired because the provider stopped
							// streaming. The in-flight body read fails below
							// and is surfaced as a regular stream error.
							scancel()
							return
						}
					}
				}
			}()
			// Join the watchdog before this goroutine — the request owner —
			// returns, so the watchdog never outlives the request and its
			// defaultStreamIdleTimeout reads cannot race with a Set* override
			// restore in tests. scancel is called first (idempotent; the head
			// defer below still covers early returns before this point) so the
			// watchdog is released from its select instead of deadlocking the
			// join.
			defer func() {
				scancel()
				<-watchdogDone
			}()
		}

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			// Real progress: a full line arrived. Never updated while blocked
			// inside Scan() — that silence is exactly what the watchdog
			// measures.
			atomic.StoreInt64(&lastRead, time.Now().UnixNano())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			// Handle [DONE] sentinel (OpenAI standard)
			if data == "[DONE]" {
				break
			}

			// Handle empty data line — some servers signal end this way
			if data == "" {
				break
			}

			var chunk ChatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}

		// If the loop exited because scanner.Scan() returned false (connection closed)
		// or an empty data line, check for read errors and propagate them.
		if err := scanner.Err(); err != nil {
			select {
			case errCh <- fmt.Errorf("stream interrupted: %w", err):
			default:
			}
		}
	}()

	return out, errCh
}

// Completion sends a raw prompt to llama.cpp (used for Impersonation fallback).
func (c *Client) Completion(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/completion"

	// Bound the non-streaming request (same pattern as ChatCompletion) so a
	// server that accepts the connection and never responds cannot block the
	// impersonation fallback for the OS TCP lifetime. A non-positive
	// defaultRequestTimeout disables the bound.
	reqCtx := ctx
	if defaultRequestTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	c.applyHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.formatError(resp)
	}

	var completionResp CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&completionResp); err != nil {
		return nil, err
	}
	return &completionResp, nil
}

// HealthCheck asserts that the server is reachable and identifies its type.
func (c *Client) HealthCheck(ctx context.Context) error {
	if c.getBackend() == BackendUnknown {
		_ = c.DiscoverBackend(ctx)
	}

	url := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/health"

	// Bound the probe (same pattern as ChatCompletion): a health check against
	// a server that accepts the connection and never responds must fail with a
	// deadline error instead of blocking the caller for the OS TCP lifetime. A
	// non-positive defaultRequestTimeout disables the bound.
	reqCtx := ctx
	if defaultRequestTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status: %d", resp.StatusCode)
	}
	return nil
}

// RefreshContextSize re-probes the backend properties to update the context size if it's llama.cpp.
func (c *Client) RefreshContextSize(ctx context.Context) {
	c.mu.RLock()
	isLlama := c.backend == BackendLlamaCPP
	baseURL := strings.TrimSuffix(c.cfg.BaseURL, "/")
	apiKey := c.cfg.APIKey
	c.mu.RUnlock()
	if !isLlama {
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	propsURLs := []string{baseURL + "/props"}
	if u, err := url.Parse(baseURL); err == nil && u.Path != "" && u.Path != "/" {
		parent := strings.TrimSuffix(baseURL, u.Path)
		if parent != baseURL {
			propsURLs = append(propsURLs, parent+"/props")
		}
	}

	var newCtxSize int
	for _, propsURL := range propsURLs {
		req, err := http.NewRequestWithContext(probeCtx, "GET", propsURL, nil)
		if err != nil {
			continue
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		c.applyHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode == http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil {
				nCtx, _ := parsePropsBodyData(body)
				newCtxSize = nCtx
				if newCtxSize <= 0 {
					newCtxSize = c.probeModelsEndpoint(probeCtx, baseURL)
				}
			}
			break
		}
		resp.Body.Close()
	}

	if newCtxSize <= 0 {
		newCtxSize = c.probeModelsEndpoint(probeCtx, baseURL)
	}

	if newCtxSize > 0 {
		c.mu.Lock()
		c.ctxSize = newCtxSize
		c.mu.Unlock()
	}
}

// DiscoverBackend probes certain endpoints to identify the inference engine.
// It tries `/props` at the raw BaseURL and at the parent path (in case
// BaseURL includes a path prefix like "/v1").
//
// Crucially, this function performs network requests without holding c.mu,
// so that concurrent readers (e.g. TUI rendering ContextSize) are never blocked.
func (c *Client) DiscoverBackend(ctx context.Context) BackendType {
	c.mu.RLock()
	if c.backend == BackendLlamaCPP && c.ctxSize != -1 {
		b := c.backend
		c.mu.RUnlock()
		return b
	}
	baseURL := strings.TrimSuffix(c.cfg.BaseURL, "/")
	apiKey := c.cfg.APIKey
	c.mu.RUnlock()

	// Bound the discovery probe so an unreachable server never hangs indefinitely
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	propsURLs := []string{baseURL + "/props"}
	if u, err := url.Parse(baseURL); err == nil && u.Path != "" && u.Path != "/" {
		parent := strings.TrimSuffix(baseURL, u.Path)
		if parent != baseURL {
			propsURLs = append(propsURLs, parent+"/props")
		}
	}

	var (
		discoveredBackend        = BackendUnknown
		discoveredCtxSize        = -1
		discoveredSupportsVision = false
	)

	for _, propsURL := range propsURLs {
		req, err := http.NewRequestWithContext(probeCtx, "GET", propsURL, nil)
		if err != nil {
			continue
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		c.applyHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode == http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil {
				discoveredBackend = BackendLlamaCPP
				nCtx, vis := parsePropsBodyData(body)
				discoveredCtxSize = nCtx
				discoveredSupportsVision = vis
			}
			break
		}
		resp.Body.Close()
	}

	// If /props returned n_ctx <= 0 or failed entirely, try /v1/models
	if discoveredCtxSize <= 0 {
		ctxSize := c.probeModelsEndpoint(probeCtx, baseURL)
		if ctxSize > 0 {
			discoveredCtxSize = ctxSize
			discoveredBackend = BackendLlamaCPP
		}
	}

	if discoveredBackend == BackendUnknown {
		discoveredBackend = BackendGenericOpenAI
	}

	c.mu.Lock()
	c.backend = discoveredBackend
	if discoveredCtxSize > 0 {
		c.ctxSize = discoveredCtxSize
	}
	if discoveredSupportsVision {
		c.supportsVision = discoveredSupportsVision
	}
	res := c.backend
	c.mu.Unlock()

	return res
}

// probeModelsEndpoint fetches /v1/models and extracts n_ctx from the loaded model.
func (c *Client) probeModelsEndpoint(ctx context.Context, baseURL string) int {
	modelsURLs := []string{baseURL + "/v1/models"}

	// Also try parent path
	if u, err := url.Parse(baseURL); err == nil && u.Path != "" && u.Path != "/" {
		parent := strings.TrimSuffix(baseURL, u.Path)
		if parent != baseURL {
			modelsURLs = append(modelsURLs, parent+"/v1/models")
		}
	}

	for _, url := range modelsURLs {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		if c.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
		c.applyHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		// Parse the OpenAI /v1/models response to find the loaded model's n_ctx
		var modelsResp struct {
			Data []struct {
				ID     string `json:"id"`
				Status struct {
					Value string `json:"value"`
				} `json:"status"`
				Meta *struct {
					NCtx int `json:"n_ctx"`
				} `json:"meta,omitempty"`
			} `json:"data"`
		}

		if err := json.Unmarshal(body, &modelsResp); err != nil {
			continue
		}

		for _, m := range modelsResp.Data {
			if m.Status.Value == "loaded" && m.Meta != nil && m.Meta.NCtx > 0 {
				return m.Meta.NCtx
			}
		}

		// Also accept any model with meta.n_ctx, even if not marked loaded
		for _, m := range modelsResp.Data {
			if m.Meta != nil && m.Meta.NCtx > 0 {
				return m.Meta.NCtx
			}
		}
	}

	return 0
}

// parsePropsBodyData extracts ctxSize and vision support from a /props JSON body.
func parsePropsBodyData(body []byte) (int, bool) {
	var props PropsResponse
	if err := json.Unmarshal(body, &props); err == nil {
		return props.DefaultGenerationSettings.NCtx, props.Modalities.Vision
	}

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawMap); err != nil {
		return 0, false
	}

	var (
		nCtx int
		vis  bool
	)
	if dgs, ok := rawMap["default_generation_settings"]; ok {
		var dgsMap map[string]json.RawMessage
		if json.Unmarshal(dgs, &dgsMap) == nil {
			if nCtxRaw, ok := dgsMap["n_ctx"]; ok {
				_ = json.Unmarshal(nCtxRaw, &nCtx)
			}
		}
	}

	if modRaw, ok := rawMap["modalities"]; ok {
		var mods Modalities
		if json.Unmarshal(modRaw, &mods) == nil {
			vis = mods.Vision
		}
	}
	return nCtx, vis
}

func (c *Client) getBackend() BackendType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.backend
}

func (c *Client) Backend() BackendType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.backend
}

func (c *Client) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.BaseURL
}

func (c *Client) ContextSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ctxSize
}

func (c *Client) IsLlamaCPP() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.backend == BackendLlamaCPP
}

func (c *Client) SupportsVision() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.EnableImages || c.supportsVision
}

func (c *Client) marshalFlattened(req ChatCompletionRequest) ([]byte, error) {
	// Marshal the request normally first
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	// Unmarshal into a map
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	// Move everything from extra_body to the root
	if extra, ok := m["extra_body"].(map[string]any); ok {
		for k, v := range extra {
			m[k] = v
		}
		// Remove the extra_body field
		delete(m, "extra_body")
	}

	return json.Marshal(m)
}

func (c *Client) formatError(resp *http.Response) error {
	var apiErr APIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err == nil && apiErr.Error.Message != "" {
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr.Error.Message)
	}
	return fmt.Errorf("status: %d", resp.StatusCode)
}

func (c *Client) APIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.APIKey
}

func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

func (c *Client) LogitBias() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.LogitBias
}

func (c *Client) SetLogitBias(bias map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.LogitBias = bias
}

// MergeLogitBiases merges dynamic biases with user overrides.
// User overrides take precedence over dynamic defaults on key collisions.
func MergeLogitBiases(defaults, overrides map[string]int) map[string]int {
	if len(defaults) == 0 && len(overrides) == 0 {
		return nil
	}
	merged := make(map[string]int, len(defaults)+len(overrides))
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	return merged
}

func (c *Client) mergeLogitBias(reqBias map[string]int) map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return MergeLogitBiases(c.cfg.LogitBias, reqBias)
}
