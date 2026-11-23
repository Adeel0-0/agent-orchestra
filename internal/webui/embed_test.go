package webui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Every file index.html loads must be embedded; otherwise a clean checkout serves a
// page whose script or stylesheet is missing (for example if .gitignore hides it).
func TestBuiltUIIsEmbeddedWithEveryAssetItLoads(t *testing.T) {
	index, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		t.Fatalf("index.html not embedded: %v", err)
	}
	if !strings.Contains(string(index), `id="root"`) {
		t.Fatalf("unexpected index.html: %.200s", index)
	}
	refs := regexp.MustCompile(`(?:src|href)="/?(assets/[^"]+)"`).FindAllStringSubmatch(string(index), -1)
	if len(refs) == 0 {
		t.Fatal("index.html references no assets")
	}
	sawScript := false
	for _, ref := range refs {
		if _, err := fs.Stat(FS(), ref[1]); err != nil {
			t.Errorf("index.html loads %s, which is not embedded: %v", ref[1], err)
		}
		sawScript = sawScript || strings.HasSuffix(ref[1], ".js")
	}
	if !sawScript {
		t.Error("index.html loads no JavaScript bundle")
	}
}
