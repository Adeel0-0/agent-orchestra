package webassets

import (
	"io/fs"
	"strings"
	"testing"
)

func TestBuiltUIIsEmbedded(t *testing.T) {
	index, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		t.Fatalf("index.html not embedded: %v", err)
	}
	if !strings.Contains(string(index), "<div id=\"root\"") && !strings.Contains(string(index), "id=\"root\"") {
		t.Fatalf("unexpected index.html: %.200s", index)
	}
	assets, err := fs.ReadDir(FS(), "assets")
	if err != nil || len(assets) == 0 {
		t.Fatalf("assets/ not embedded: %v", err)
	}
}
