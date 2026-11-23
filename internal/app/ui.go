package app

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	bootstrapTTL       = 60 * time.Second
	maxBootstrapNonces = 16
)

// BootstrapTokens are one-use, short-lived nonces the launcher puts in the page URL
// fragment; the page exchanges one for the launch token (orchestra/app/ui.py).
type BootstrapTokens struct {
	mu     sync.Mutex
	nonces map[string]time.Time
	order  []string // issue order, to evict the oldest
}

// NewBootstrapTokens returns an empty nonce set.
func NewBootstrapTokens() *BootstrapTokens {
	return &BootstrapTokens{nonces: map[string]time.Time{}}
}

// tokenURLSafe is Python's secrets.token_urlsafe(n).
func tokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Issue returns a new nonce valid for min(ttl, 60s). At most 16 live nonces exist;
// issuing more evicts the oldest.
func (b *BootstrapTokens) Issue(ttl time.Duration) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	live := b.order[:0:0]
	for _, n := range b.order {
		if exp, ok := b.nonces[n]; ok && exp.After(now) {
			live = append(live, n)
		} else {
			delete(b.nonces, n)
		}
	}
	b.order = live
	if len(b.order) >= maxBootstrapNonces {
		delete(b.nonces, b.order[0])
		b.order = b.order[1:]
	}
	if ttl > bootstrapTTL {
		ttl = bootstrapTTL
	}
	nonce := tokenURLSafe(32)
	b.nonces[nonce] = now.Add(ttl)
	b.order = append(b.order, nonce)
	return nonce
}

// Consume atomically removes nonce and reports whether it was live.
func (b *BootstrapTokens) Consume(nonce any) bool {
	s, ok := nonce.(string)
	if !ok || pyLen(s) < 24 || pyLen(s) > 128 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	exp, ok := b.nonces[s]
	if !ok {
		return false
	}
	delete(b.nonces, s)
	for i, n := range b.order {
		if n == s {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
	return time.Now().Before(exp)
}

// publicUIRequest: GET/HEAD of / and /assets/*, and POST /api/bootstrap.
func publicUIRequest(r *http.Request) bool {
	p := r.URL.Path
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && (p == "/" || strings.HasPrefix(p, "/assets/")) {
		return true
	}
	return p == "/api/bootstrap" && r.Method == http.MethodPost
}

// ErrUIAssetsMissing means the web bundle has no index.html or assets folder.
var ErrUIAssetsMissing = errors.New("UI assets are missing. Build web/ with npm ci and npm run build, or install a packaged wheel.")

func checkWebAssets(web fs.FS) error {
	info, err := fs.Stat(web, "index.html")
	if err != nil || info.IsDir() {
		return ErrUIAssetsMissing
	}
	info, err = fs.Stat(web, "assets")
	if err != nil || !info.IsDir() {
		return ErrUIAssetsMissing
	}
	return nil
}

var webTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json", ".map": "application/json",
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".ico": "image/x-icon", ".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf",
	".txt": "text/plain; charset=utf-8", ".wasm": "application/wasm",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := webTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// serveFile writes one regular file from web; anything else is a 404.
func serveFile(w http.ResponseWriter, r *http.Request, web fs.FS, name string) {
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	f, err := web.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", info.ModTime(), rs)
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, "", info.ModTime(), bytes.NewReader(data))
}
