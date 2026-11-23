// Package openrouter is the OpenRouter API player: Orchestra itself runs the agent
// loop against OpenRouter's public Chat Completions API and executes the project
// tools (orchestra/players/openrouter*.py, credentials.py).
package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// DefaultBaseURL is OpenRouter's public API.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

const (
	maxSSEEvent    = 2 * 1024 * 1024
	maxCatalog     = 8 * 1024 * 1024
	maxErrorBody   = 64 * 1024
	maxRetryDelay  = 30 * time.Second
	attempts       = 3
	catalogEntries = 32
)

// Error is an OpenRouter failure: HTTP status (0 if none), and the server's
// Retry-After when it sent one.
type Error struct {
	Message    string
	Status     int
	RetryAfter *time.Duration
}

func (e *Error) Error() string { return e.Message }

func newError(format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

var errRedirect = errors.New("redirect refused")

// Client talks to OpenRouter over HTTPS with SSE streaming. Transient HTTP
// failures (429, 5xx) are retried only before the first streamed chunk.
type Client struct {
	BaseURL      string
	Timeout      time.Duration // connect, response-header and idle-read timeout
	RetryBackoff time.Duration
	CatalogTTL   time.Duration
	HTTPClient   *http.Client

	keyMu  sync.Mutex
	key    string
	hasKey bool // false: read the key from the OS keyring on first use

	mu     sync.Mutex
	active bool
}

// NewClient returns a client that reads the API key from the OS keyring when a
// chat request first needs it. The model catalog needs no key.
func NewClient() *Client {
	return &Client{BaseURL: DefaultBaseURL, Timeout: 30 * time.Second,
		RetryBackoff: 500 * time.Millisecond, CatalogTTL: 5 * time.Minute}
}

// NewClientWithKey returns a client using key instead of the keyring.
func NewClientWithKey(key string) *Client {
	c := NewClient()
	c.key, c.hasKey = key, true
	return c
}

func (c *Client) baseURL() string { return strings.TrimRight(c.BaseURL, "/") }

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: c.Timeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = c.Timeout
	transport.ResponseHeaderTimeout = c.Timeout
	c.HTTPClient = &http.Client{
		Transport: transport,
		// Never forward Authorization to a redirected host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
	return c.HTTPClient
}

// redact removes the actual key and any Bearer credential, replaces
// non-printable characters and keeps at most 500 characters.
var bearerPattern = regexp.MustCompile(`(?i)Bearer[\s\x{0B}\x{85}\p{Z}]+[^\s\x{0B}\x{85}\p{Z}]+`)

func (c *Client) redact(message string) string {
	c.keyMu.Lock()
	key := c.key
	c.keyMu.Unlock()
	if key != "" {
		message = strings.ReplaceAll(message, key, "[redacted]")
	}
	message = bearerPattern.ReplaceAllString(message, "Bearer [redacted]")
	message = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, message)
	if utf8.RuneCountInString(message) > 500 {
		message = string([]rune(message)[:500])
	}
	return message
}

func (c *Client) getKey() (string, error) {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	if !c.hasKey {
		key, err := GetKey()
		if err != nil {
			return "", err
		}
		c.key, c.hasKey = key, true
	}
	if c.key == "" {
		return "", newError("OpenRouter API key is missing. Run: orchestra openrouter-key")
	}
	return c.key, nil
}

func (c *Client) begin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active {
		return newError("A request is already running.")
	}
	c.active = true
	return nil
}

func (c *Client) end() {
	c.mu.Lock()
	c.active = false
	c.mu.Unlock()
}

// idleBody cancels the request when no bytes arrive for the idle timeout, like
// urllib's per-read socket timeout.
type idleBody struct {
	body    io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
	fired   chan struct{}
}

func newIdleBody(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleBody {
	b := &idleBody{body: body, timeout: timeout, fired: make(chan struct{})}
	b.timer = time.AfterFunc(timeout, func() {
		close(b.fired)
		cancel()
	})
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.body.Close()
}

func (b *idleBody) timedOut() bool {
	select {
	case <-b.fired:
		return true
	default:
		return false
	}
}

// open sends one request. body != nil makes it an authenticated chat POST.
func (c *Client) open(ctx context.Context, endpoint string, body map[string]any, key string) (*idleBody, context.CancelFunc, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	method, accept := http.MethodGet, "application/json"
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			cancel()
			return nil, nil, newError("Cannot encode the OpenRouter request.")
		}
		method, accept, payload = http.MethodPost, "text/event-stream", bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.baseURL()+endpoint, payload)
	if err != nil {
		cancel()
		return nil, nil, newError("Cannot connect to OpenRouter (network error or timeout).")
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if errors.Is(err, errRedirect) {
			return nil, nil, newError("OpenRouter redirect refused.")
		}
		return nil, nil, newError("Cannot connect to OpenRouter (network error or timeout).")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer cancel()
		defer resp.Body.Close()
		return nil, nil, c.httpError(resp)
	}
	return newIdleBody(resp.Body, c.Timeout, cancel), cancel, nil
}

func (c *Client) httpError(resp *http.Response) *Error {
	e := &Error{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	message := "Check credentials, credit, model access or rate limits."
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	if err == nil && len(raw) <= maxErrorBody {
		var detail struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &detail) == nil {
			var inner map[string]any
			if json.Unmarshal(detail.Error, &inner) == nil {
				if m, ok := inner["message"].(string); ok {
					message = c.redact(m)
				}
			}
		}
	}
	e.Message = fmt.Sprintf("OpenRouter HTTP %d: %s", resp.StatusCode, message)
	return e
}

// parseRetryAfter accepts delay-seconds (any float, negative means 0) or an
// HTTP date.
func parseRetryAfter(header string, now time.Time) *time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	var d time.Duration
	if f, err := strconv.ParseFloat(header, 64); err == nil {
		switch {
		case math.IsNaN(f) || f <= 0:
			d = 0
		case f > maxRetryDelay.Seconds():
			d = maxRetryDelay
		default:
			d = time.Duration(f * float64(time.Second))
		}
		return &d
	}
	t, err := http.ParseTime(header)
	if err != nil {
		return nil
	}
	d = max(0, t.Sub(now))
	return &d
}

// consumerError carries an error returned by the caller's chunk function through
// the stream unchanged.
type consumerError struct{ err error }

func (e *consumerError) Error() string { return e.err.Error() }

// Stream posts body to /chat/completions with streaming and usage enabled and
// calls fn for every chunk on the caller's goroutine. A transient HTTP error is
// retried (at most three attempts, Retry-After or exponential backoff capped at
// 30 seconds) only if no chunk was delivered. Cancelling ctx stops the request
// and any backoff and returns ctx.Err().
func (c *Client) Stream(ctx context.Context, body map[string]any, fn func(chunk map[string]any) error) error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	key, err := c.getKey()
	if err != nil {
		return err
	}
	payload := make(map[string]any, len(body)+2)
	for k, v := range body {
		payload[k] = v
	}
	payload["stream"] = true
	payload["stream_options"] = map[string]any{"include_usage": true}

	for attempt := 0; ; attempt++ {
		streamed := false
		err := c.streamOnce(ctx, payload, key, func(chunk map[string]any) error {
			streamed = true
			return fn(chunk)
		})
		if err == nil {
			return nil
		}
		var ce *consumerError
		if errors.As(err, &ce) {
			return ce.err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var oe *Error
		if !errors.As(err, &oe) || streamed || attempt == attempts-1 ||
			!(oe.Status == 429 || (oe.Status >= 500 && oe.Status < 600)) {
			return err
		}
		delay := c.RetryBackoff * time.Duration(1<<attempt)
		if oe.RetryAfter != nil {
			delay = *oe.RetryAfter
		}
		delay = min(delay, maxRetryDelay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, payload map[string]any, key string, fn func(map[string]any) error) error {
	body, cancel, err := c.open(ctx, "/chat/completions", payload, key)
	if err != nil {
		return c.redactErr(err)
	}
	defer cancel()
	defer body.Close()
	err = ParseSSE(body, func(chunk map[string]any) error {
		if err := fn(chunk); err != nil {
			return &consumerError{err}
		}
		return nil
	})
	if err == nil {
		return nil
	}
	var ce *consumerError
	if errors.As(err, &ce) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if body.timedOut() {
		return newError("OpenRouter stream failed (network or decoding error).")
	}
	return c.redactErr(err)
}

// redactErr applies redact to OpenRouter errors and hides any other error text.
func (c *Client) redactErr(err error) error {
	var oe *Error
	if errors.As(err, &oe) {
		return &Error{Message: c.redact(oe.Message), Status: oe.Status, RetryAfter: oe.RetryAfter}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return newError("OpenRouter stream failed (network or decoding error).")
}

// ParseSSE reads Server-Sent Events frames (not transport chunks) and calls fn
// with each data payload parsed as a JSON object. The stream must end with the
// explicit [DONE] sentinel; an event over 2 MiB, invalid UTF-8 or JSON, a
// non-object payload or an error object fails the stream.
func ParseSSE(r io.Reader, fn func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data []string
	size := 0
	for {
		raw, err := readLine(br, maxSSEEvent-size)
		if err != nil {
			return err
		}
		if raw == nil {
			return newError("OpenRouter stream ended before [DONE].")
		}
		size += len(raw)
		if !utf8.Valid(raw) {
			return newError("Invalid UTF-8 in OpenRouter stream.")
		}
		line := strings.TrimRight(string(raw), "\r\n")
		if line != "" {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(value, " "))
			}
			continue
		}
		payload := strings.Join(data, "\n")
		data, size = nil, 0
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return nil
		}
		dec := json.NewDecoder(strings.NewReader(payload))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil || dec.More() {
			return newError("Invalid JSON in OpenRouter stream.")
		}
		if _, err := dec.Token(); err != io.EOF {
			return newError("Invalid JSON in OpenRouter stream.")
		}
		obj, ok := value.(map[string]any)
		if !ok {
			return newError("Expected an object in OpenRouter stream.")
		}
		if errValue := obj["error"]; truthy(errValue) {
			message := "OpenRouter stream failed"
			if m, ok := errValue.(map[string]any); ok {
				if v, present := m["message"]; present {
					message = pyStr(v)
				}
			} else {
				message = pyStr(errValue)
			}
			return newError("%s", message)
		}
		if err := fn(obj); err != nil {
			return err
		}
	}
}

// readLine returns one line including its '\n', nil at EOF with nothing read,
// or an error once the event grows past budget bytes.
func readLine(br *bufio.Reader, budget int) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > budget {
			return nil, newError("OpenRouter SSE event exceeds 2 MiB.")
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, nil
			}
			return line, nil
		default:
			return nil, err
		}
	}
}

// ------------------------------------------------------------------ catalog

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]catalogEntry{}
)

type catalogEntry struct {
	expires time.Time
	raw     []byte
}

// ResetCatalogCache forgets cached model catalogs (for tests).
func ResetCatalogCache() {
	catalogMu.Lock()
	catalogCache = map[string]catalogEntry{}
	catalogMu.Unlock()
}

// Models returns the public catalog filtered to models that advertise tool
// support. It needs no key and is cached process-wide per base URL.
func (c *Client) Models(ctx context.Context) ([]map[string]any, error) {
	base := c.baseURL()
	catalogMu.Lock()
	entry, ok := catalogCache[base]
	catalogMu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return decodeModels(entry.raw)
	}
	if err := c.begin(); err != nil {
		return nil, err
	}
	defer c.end()
	body, cancel, err := c.open(ctx, "/models", nil, "")
	if err != nil {
		return nil, c.redactErr(err)
	}
	defer cancel()
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxCatalog+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, newError("OpenRouter stream failed (network or decoding error).")
	}
	if len(raw) > maxCatalog {
		return nil, newError("Model catalog is too large.")
	}
	models, err := filterCatalog(raw)
	if err != nil {
		return nil, err
	}
	filtered, err := json.Marshal(models)
	if err != nil {
		return nil, newError("Invalid OpenRouter model catalog.")
	}
	catalogMu.Lock()
	if len(catalogCache) >= catalogEntries {
		catalogCache = map[string]catalogEntry{}
	}
	ttl := c.CatalogTTL
	catalogCache[base] = catalogEntry{expires: time.Now().Add(ttl), raw: filtered}
	catalogMu.Unlock()
	return decodeModels(filtered)
}

func filterCatalog(raw []byte) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, newError("Invalid OpenRouter model catalog.")
	}
	data, ok := doc["data"].([]any)
	if !ok {
		return nil, newError("Invalid OpenRouter model catalog.")
	}
	models := []any{}
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := m["id"].(string); !ok {
			continue
		}
		if supportsTools(m["supported_parameters"]) {
			models = append(models, m)
		}
	}
	return models, nil
}

func supportsTools(v any) bool {
	switch params := v.(type) {
	case []any:
		for _, p := range params {
			if p == "tools" {
				return true
			}
		}
	case string:
		return strings.Contains(params, "tools")
	case map[string]any:
		_, ok := params["tools"]
		return ok
	}
	return false
}

func decodeModels(raw []byte) ([]map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var models []map[string]any
	if err := dec.Decode(&models); err != nil {
		return nil, newError("Invalid OpenRouter model catalog.")
	}
	return models, nil
}

// Models lists tool-capable models for `orchestra models`.
func Models(ctx context.Context) ([]map[string]any, error) {
	return NewClient().Models(ctx)
}

// ------------------------------------------------------------------ helpers

// truthy is Python's bool() for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}

// pyStr is Python's str() for decoded JSON values (close enough for messages).
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case json.Number:
		return string(x)
	}
	return pyDumps(v, false)
}
