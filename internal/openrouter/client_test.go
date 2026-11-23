package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func parseAll(text string) ([]map[string]any, error) {
	var items []map[string]any
	err := ParseSSE(strings.NewReader(text), func(m map[string]any) error {
		items = append(items, m)
		return nil
	})
	return items, err
}

func TestSSECommentsMultilineUsageAndDone(t *testing.T) {
	items, err := parseAll(": keepalive\r\ndata: {\"choices\":\r\ndata: [], \"usage\":{\"cost\":0.01}}\r\n\r\ndata: [DONE]\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || pyDumps(items[0], false) != `{"choices": [], "usage": {"cost": 0.01}}` {
		t.Fatalf("items %v", items)
	}
}

func TestSSETruncatedOrInvalidStreamIsError(t *testing.T) {
	for _, text := range []string{"data: {bad}\n\n", "data: {\"choices\":[]}\n\n", "data: []\n\n",
		"data: [DONE]", "data: \xff\n\n", "data: " + strings.Repeat("x", maxSSEEvent) + "\n\n"} {
		if _, err := parseAll(text); err == nil {
			t.Errorf("%.40q parsed", text)
		}
	}
}

func TestSSEMidstreamErrorIsNotSuccess(t *testing.T) {
	_, err := parseAll("data: {\"error\":{\"message\":\"Provider failed\"}}\n\ndata: [DONE]\n\n")
	if err == nil || !strings.Contains(err.Error(), "Provider failed") {
		t.Fatalf("err = %v", err)
	}
}

type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []fakeRequest
	gets     []string
	release  chan struct{}
}

type fakeRequest struct {
	path, auth string
	body       map[string]any
}

func (f *fakeServer) posts() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeServer) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gets)
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{release: make(chan struct{})}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		if r.Method == http.MethodGet {
			f.mu.Lock()
			f.gets = append(f.gets, r.URL.Path)
			f.mu.Unlock()
			if r.URL.Path == "/redirect/api/v1/models" {
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
				return
			}
			w.WriteHeader(200)
			flusher.Flush()
			if strings.HasPrefix(r.URL.Path, "/hang/") {
				select {
				case <-f.release:
				case <-time.After(5 * time.Second):
				}
			}
			_, _ = w.Write([]byte(`{"data": [{"id": "test/tools", "supported_parameters": ["tools"], "context_length": 8192},
				{"id": "test/plain", "supported_parameters": []}]}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.requests = append(f.requests, fakeRequest{r.URL.Path, r.Header.Get("Authorization"), body})
		retries := 0
		for _, req := range f.requests {
			if req.body["model"] == "test/retry" {
				retries++
			}
		}
		f.mu.Unlock()
		model, _ := body["model"].(string)
		if model == "test/http-error" || model == "test/retry-wait" || (model == "test/retry" && retries == 1) {
			status, after := 429, "0"
			if model == "test/http-error" {
				status = 400
			}
			if model == "test/retry-wait" {
				after = "10"
			}
			w.Header().Set("Retry-After", after)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter for test-key"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		switch model {
		case "test/partial-error":
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\ndata: {\"error\":{\"code\":503,\"message\":\"unavailable\"}}\n\n"))
			return
		case "test/hang":
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"waiting\"},\"finish_reason\":null}]}\n\n"))
			flusher.Flush()
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"pong\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(func() {
		close(f.release)
		f.Server.Close()
	})
	return f
}

func (f *fakeServer) api() string { return f.URL + "/api/v1" }

func collect(ctx context.Context, c *Client, body map[string]any) ([]map[string]any, error) {
	var chunks []map[string]any
	err := c.Stream(ctx, body, func(m map[string]any) error {
		chunks = append(chunks, m)
		return nil
	})
	return chunks, err
}

func contentOf(chunk map[string]any) string {
	return chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"].(string)
}

func TestCatalogFiltersToolsAndStreamPostsContract(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("test-key")
	c.BaseURL = f.api()
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 1 || models[0]["id"] != "test/tools" {
		t.Fatalf("models %v %v", models, err)
	}
	chunks, err := collect(context.Background(), c, map[string]any{"model": "test/tools", "messages": []any{}})
	if err != nil || contentOf(chunks[0]) != "pong" {
		t.Fatalf("chunks %v %v", chunks, err)
	}
	req := f.posts()[0]
	if req.path != "/api/v1/chat/completions" || req.auth != "Bearer test-key" {
		t.Fatalf("request %+v", req)
	}
	if req.body["stream"] != true || req.body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("body %v", req.body)
	}
}

func TestMissingKeyFailsBeforeNetwork(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("")
	c.BaseURL = f.api()
	_, err := collect(context.Background(), c, map[string]any{"model": "x", "messages": []any{}})
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("err = %v", err)
	}
	if len(f.posts()) != 0 {
		t.Fatal("request sent without a key")
	}
}

func TestHTTPErrorExplainsReasonAndRedactsActualKey(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("test-key")
	c.BaseURL = f.api()
	_, err := collect(context.Background(), c, map[string]any{"model": "test/http-error"})
	if err == nil || !strings.Contains(err.Error(), "Unsupported parameter") || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("err = %v", err)
	}
	var oe *Error
	if !errors.As(err, &oe) || oe.Status != 400 {
		t.Fatalf("err %#v", err)
	}
	if len(f.posts()) != 1 {
		t.Fatalf("%d requests", len(f.posts()))
	}
}

func TestRetriesTransientHTTPOnlyBeforeAnyStreamedChunk(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("test-key")
	c.BaseURL, c.RetryBackoff = f.api(), 0
	chunks, err := collect(context.Background(), c, map[string]any{"model": "test/retry"})
	if err != nil || contentOf(chunks[0]) != "pong" {
		t.Fatalf("chunks %v %v", chunks, err)
	}
	if len(f.posts()) != 2 {
		t.Fatalf("%d requests", len(f.posts()))
	}
	if _, err := collect(context.Background(), c, map[string]any{"model": "test/partial-error"}); err == nil {
		t.Fatal("partial error succeeded")
	}
	if len(f.posts()) != 3 {
		t.Fatalf("%d requests after partial error", len(f.posts()))
	}
}

func TestCatalogIsCachedAcrossNewClients(t *testing.T) {
	f := newFakeServer(t)
	first := NewClient()
	first.BaseURL = f.api()
	models, err := first.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	models[0]["id"] = "mutated-by-caller"
	second := NewClient()
	second.BaseURL = f.api()
	again, err := second.Models(context.Background())
	if err != nil || again[0]["id"] != "test/tools" {
		t.Fatalf("models %v %v", again, err)
	}
	if f.getCount() != 1 {
		t.Fatalf("%d catalog requests", f.getCount())
	}
}

func TestCancelDuringRetryBackoffPreventsSecondRequest(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("test-key")
	c.BaseURL = f.api()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := collect(ctx, c, map[string]any{"model": "test/retry-wait"})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for len(f.posts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("backoff not interrupted")
	}
	if len(f.posts()) != 1 {
		t.Fatalf("%d requests", len(f.posts()))
	}
}

func TestExternalCancelStopsConsumptionDuringIdleRead(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("test-key")
	c.BaseURL = f.api()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var once sync.Once
		done <- c.Stream(ctx, map[string]any{"model": "test/hang", "messages": []any{}}, func(map[string]any) error {
			once.Do(func() { close(started) })
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancel did not stop the read")
	}
}

func TestIdleReadTimeoutFailsTheStream(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("test-key")
	c.BaseURL, c.Timeout = f.api(), 200*time.Millisecond
	start := time.Now()
	_, err := collect(context.Background(), c, map[string]any{"model": "test/hang"})
	if err == nil || !strings.Contains(err.Error(), "network or decoding error") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("idle timeout not applied")
	}
}

func TestCancelDuringModelCatalog(t *testing.T) {
	f := newFakeServer(t)
	c := NewClientWithKey("test-key")
	c.BaseURL = f.URL + "/hang"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Models(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("catalog cancel blocked")
	}
}

func TestRedirectIsRefused(t *testing.T) {
	f := newFakeServer(t)
	c := NewClient()
	c.BaseURL = f.URL + "/redirect/api/v1"
	if _, err := c.Models(context.Background()); err == nil || err.Error() != "OpenRouter redirect refused." {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentRequestIsRefused(t *testing.T) {
	c := NewClientWithKey("k")
	if err := c.begin(); err != nil {
		t.Fatal(err)
	}
	defer c.end()
	if _, err := collect(context.Background(), c, map[string]any{}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
}

func TestRedactAndRetryAfter(t *testing.T) {
	c := NewClientWithKey("sk-secret")
	got := c.redact("bad sk-secret and Authorization: bearer abc.def\x00tail")
	if strings.Contains(got, "sk-secret") || strings.Contains(got, "abc.def") || strings.Contains(got, "\x00") {
		t.Fatalf("redact = %q", got)
	}
	if len([]rune(c.redact(strings.Repeat("é", 600)))) != 500 {
		t.Fatal("not truncated to 500 characters")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for header, want := range map[string]time.Duration{"0": 0, "1.5": 1500 * time.Millisecond, "-3": 0, "inf": maxRetryDelay,
		"Thu, 01 Jan 2026 00:00:10 GMT": 10 * time.Second} {
		got := parseRetryAfter(header, now)
		if got == nil || *got != want {
			t.Errorf("Retry-After %q = %v", header, got)
		}
	}
	if parseRetryAfter("soon", now) != nil {
		t.Error("garbage Retry-After parsed")
	}
}

// ------------------------------------------------------------------ credentials

type memoryVault struct {
	stored map[string]string
	fail   bool
}

func (m *memoryVault) Get(service, account string) (string, bool, error) {
	if m.fail {
		return "", false, errors.New("backend failure with details")
	}
	v, ok := m.stored[service+"\x00"+account]
	return v, ok, nil
}

func (m *memoryVault) Set(service, account, value string) error {
	if m.fail {
		return errors.New("backend failure with details")
	}
	m.stored[service+"\x00"+account] = value
	return nil
}

func useVault(t *testing.T, v secretStore) {
	t.Helper()
	old := vault
	vault = v
	t.Cleanup(func() { vault = old })
}

func TestCredentialsUseKeyringNames(t *testing.T) {
	mem := &memoryVault{stored: map[string]string{}}
	useVault(t, mem)
	if present, err := KeyPresent(); err != nil || present {
		t.Fatalf("present %v %v", present, err)
	}
	if err := SetKey("test-secret"); err != nil {
		t.Fatal(err)
	}
	if mem.stored["agent-orchestra\x00openrouter"] != "test-secret" {
		t.Fatalf("stored %v", mem.stored)
	}
	if key, err := GetKey(); err != nil || key != "test-secret" {
		t.Fatalf("key %q %v", key, err)
	}
	if present, _ := KeyPresent(); !present {
		t.Fatal("key not present")
	}
	for _, bad := range []string{"", "  ", "has space", "tab\t", "x\x1c"} {
		if err := SetKey(bad); err == nil {
			t.Errorf("SetKey(%q) accepted", bad)
		}
	}
	mem.fail = true
	if err := SetKey("test-secret"); err == nil || strings.Contains(err.Error(), "details") {
		t.Fatalf("err = %v", err)
	}
	if _, err := GetKey(); err == nil || err.Error() != "Unable to read the OpenRouter key from the OS keyring." {
		t.Fatalf("err = %v", err)
	}
}

func TestClientReadsKeyFromKeyringLazily(t *testing.T) {
	useVault(t, &memoryVault{stored: map[string]string{"agent-orchestra\x00openrouter": "vault-key"}})
	f := newFakeServer(t)
	c := NewClient()
	c.BaseURL = f.api()
	if _, err := collect(context.Background(), c, map[string]any{"model": "test/tools"}); err != nil {
		t.Fatal(err)
	}
	if f.posts()[0].auth != "Bearer vault-key" {
		t.Fatal("keyring key not used")
	}
}

func TestSaveKeyInteractive(t *testing.T) {
	mem := &memoryVault{stored: map[string]string{}}
	useVault(t, mem)
	old := readHidden
	t.Cleanup(func() { readHidden = old })
	readHidden = func(*os.File, io.Writer) (string, error) { return "sk-typed", nil }
	var out bytes.Buffer
	if err := SaveKeyInteractive(nil, &out); err != nil {
		t.Fatal(err)
	}
	if mem.stored["agent-orchestra\x00openrouter"] != "sk-typed" || strings.Contains(out.String(), "sk-typed") {
		t.Fatalf("stored %v out %q", mem.stored, out.String())
	}
	readHidden = func(*os.File, io.Writer) (string, error) { return "", ErrCancelled }
	if err := SaveKeyInteractive(nil, &out); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
}

func TestHiddenPromptRefusesNonTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := readHiddenLine(r, io.Discard); !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("err = %v", err)
	}
}
