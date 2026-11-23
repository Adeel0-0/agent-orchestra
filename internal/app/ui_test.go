package app

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func testWeb() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<html><body>inert shell</body></html>")},
		"assets/app.js": {Data: []byte(`console.log("static")`)},
	}
}

func newUIFixture(t *testing.T) *apiFixture {
	return newAPIFixture(t, func(c *Config) {
		c.Web = testWeb()
		c.FolderPicker = func(context.Context) (string, error) { return os.TempDir(), nil }
	})
}

var noAuth = map[string]string{"Authorization": ""}

func withOrigin(origin string) map[string]string {
	return map[string]string{"Authorization": "", "Origin": origin}
}

func TestUIStaticShellIsPublicButNeverContainsLaunchToken(t *testing.T) {
	f := newUIFixture(t)
	shell := f.do("GET", "/", nil, noAuth)
	if shell.code != 200 || strings.Contains(shell.body, testToken) || !strings.Contains(shell.body, "inert shell") {
		t.Fatalf("shell %d %s", shell.code, shell.body)
	}
	if !strings.Contains(shell.header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("shell lacks CSP")
	}
	if ct := shell.header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("shell content type %s", ct)
	}
	js := f.do("GET", "/assets/app.js", nil, noAuth)
	if js.code != 200 || !strings.HasPrefix(js.header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("asset %d %s", js.code, js.header.Get("Content-Type"))
	}
	if r := f.do("HEAD", "/assets/app.js", nil, noAuth); r.code != 200 {
		t.Fatalf("HEAD asset %d", r.code)
	}
	if r := f.do("GET", "/api/projects", nil, noAuth); r.code != 401 {
		t.Fatalf("API without token %d", r.code)
	}
	if r := f.do("GET", "/", nil, map[string]string{"Authorization": "", "Host": "evil.test"}); r.code != 403 {
		t.Fatalf("shell with foreign host %d", r.code)
	}
	for _, p := range []string{"/assets/../orchestra.db", "/assets/", "/assets/missing.js", "/index.html", "/assets/%2e%2e/index.html"} {
		if r := f.do("GET", p, nil, noAuth); r.code == 200 {
			t.Fatalf("%s served: %s", p, r.body)
		}
	}
	if r := f.do("POST", "/assets/app.js", nil, noAuth); r.code != 401 {
		t.Fatalf("POST asset without token %d", r.code)
	}
}

func TestUINonceIsSingleUseAndRequiresExactOrigin(t *testing.T) {
	f := newUIFixture(t)
	nonce := f.app.Bootstrap.Issue(bootstrapTTL)
	if r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce}, noAuth); r.code != 403 {
		t.Fatalf("no origin %d %s", r.code, r.body)
	}
	r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce}, withOrigin(testBase))
	if r.code != 200 || r.json(t)["token"] != testToken {
		t.Fatalf("exchange %d %s", r.code, r.body)
	}
	if r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce}, withOrigin(testBase)); r.code != 401 {
		t.Fatalf("replay %d %s", r.code, r.body)
	}
}

func TestUIExpiredNonceAndQueryTokenCannotAuthenticate(t *testing.T) {
	f := newUIFixture(t)
	nonce := f.app.Bootstrap.Issue(-time.Second)
	if r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce}, withOrigin(testBase)); r.code != 401 {
		t.Fatalf("expired %d", r.code)
	}
	if r := f.do("GET", "/api/projects?token="+testToken, nil, noAuth); r.code != 401 {
		t.Fatalf("query token %d", r.code)
	}
	if r := f.do("GET", "/api/bootstrap?nonce=x", nil, noAuth); r.code != 401 {
		t.Fatalf("GET bootstrap %d", r.code)
	}
	for _, bad := range []any{"short", strings.Repeat("x", 129), 5, nil} {
		if r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": bad}, withOrigin(testBase)); r.code != 401 {
			t.Fatalf("nonce %v -> %d", bad, r.code)
		}
	}
}

func TestUINativePickerRequiresAPIAuth(t *testing.T) {
	f := newUIFixture(t)
	if r := f.do("POST", "/api/folders/pick", map[string]any{}, noAuth); r.code != 401 {
		t.Fatalf("unauthenticated pick %d", r.code)
	}
	r := f.do("POST", "/api/folders/pick", map[string]any{}, nil)
	if r.code != 200 {
		t.Fatalf("pick %d %s", r.code, r.body)
	}
	if info, err := os.Stat(r.json(t)["path"].(string)); err != nil || !info.IsDir() {
		t.Fatalf("picked %s", r.body)
	}
	if r := f.do("GET", "/api/capabilities", nil, nil); r.body != `{"native_folder_picker":true}` {
		t.Fatalf("capabilities %s", r.body)
	}
}

func TestUIConcurrentNonceExchangeHasExactlyOneWinner(t *testing.T) {
	f := newUIFixture(t)
	nonce := f.app.Bootstrap.Issue(bootstrapTTL)
	codes := make([]int, 8)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce}, withOrigin(testBase)).code
		}(i)
	}
	wg.Wait()
	sort.Ints(codes)
	if codes[0] != 200 || codes[1] != 401 || codes[7] != 401 {
		t.Fatalf("codes = %v", codes)
	}
}

func TestUIForeignOriginAndOversizedExchangeCannotConsumeNonce(t *testing.T) {
	f := newUIFixture(t)
	nonce := f.app.Bootstrap.Issue(bootstrapTTL)
	if r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce}, withOrigin("https://foreign.example")); r.code != 403 {
		t.Fatalf("foreign origin %d", r.code)
	}
	r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce, "padding": strings.Repeat("x", 1200)}, withOrigin(testBase))
	if r.code != 413 {
		t.Fatalf("oversized %d %s", r.code, r.body)
	}
	if r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": nonce}, withOrigin(testBase)); r.code != 200 {
		t.Fatalf("exchange after rejected attempts %d", r.code)
	}
}

func TestUIBootstrapNoncesAreBoundedAndOldestIsEvicted(t *testing.T) {
	b := NewBootstrapTokens()
	first := b.Issue(bootstrapTTL)
	for i := 0; i < maxBootstrapNonces; i++ {
		b.Issue(bootstrapTTL)
	}
	if b.Consume(first) {
		t.Fatal("oldest nonce survived eviction")
	}
	if len(b.nonces) != maxBootstrapNonces {
		t.Fatalf("%d live nonces", len(b.nonces))
	}
	long := b.Issue(time.Hour)
	if exp := b.nonces[long]; time.Until(exp) > bootstrapTTL {
		t.Fatal("ttl not capped at 60 seconds")
	}
}

func TestUIMissingAssetsAreRefused(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	_, err := New(Config{Store: store, Port: testPort, Token: testToken,
		Factory: (&sessionFixture{}).factory, Web: fstest.MapFS{"index.html": {Data: []byte("x")}}})
	if err != ErrUIAssetsMissing {
		t.Fatalf("err = %v", err)
	}
}
