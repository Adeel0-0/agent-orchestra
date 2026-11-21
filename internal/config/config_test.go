package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultsMatchPython(t *testing.T) {
	cfg := Default()
	if cfg.DebounceMS != 400 || cfg.LockLeaseSeconds != 1800 || !cfg.Git.AutoPush || cfg.Git.MainBranch != "main" ||
		cfg.Git.CommitPrefix != "chore(orchestra):" || cfg.CI.WorkflowName != "backend-verification.yml" || cfg.Pipeline.VerifyCommand != nil {
		t.Fatalf("defaults differ from orchestra/config.py: %+v", cfg)
	}
}

func TestNoConfigFileUsesDefaultsAtRoot(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil || cfg.WorkspaceRoot != dir || cfg.OrchestraDir != ".orchestra" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestOmittedKeysKeepDefaultsAndUnknownKeysAreIgnored(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "orchestra.json", `{"git": {"auto_push": false, "auto_psuh": true}, "pipeline": {"verify_command": "npm test"}, "surprise": 1}`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err) // phase 5 is halted: unknown keys must not fail
	}
	if cfg.Git.AutoPush || !cfg.Git.AutoCommit || cfg.Git.Remote != "origin" || Str(cfg.Pipeline.VerifyCommand) != "npm test" || cfg.DebounceMS != 400 {
		t.Fatalf("%+v", cfg)
	}
}

func TestMalformedJSONFailsInsteadOfUsingDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "orchestra.json", `{"git": {"auto_push": false,}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "orchestra.json") {
		t.Fatalf("malformed config must fail with the file name, got %v", err)
	}
}

func TestNonObjectAndWrongTypesFail(t *testing.T) {
	for _, body := range []string{`[1, 2]`, `{"git": 5}`, `{"debounce_ms": "fast"}`} {
		dir := t.TempDir()
		write(t, dir, "orchestra.json", body)
		if _, err := Load(dir); err == nil {
			t.Fatalf("%s loaded without error", body)
		}
	}
}

func TestWorkspaceRootIsResolvedAgainstTheSearchedFolder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "orchestra.json", `{"workspace_root": ".."}`)
	cfg, err := Load(dir)
	if err != nil || cfg.WorkspaceRoot != filepath.Dir(dir) {
		t.Fatalf("got %q, want %q (%v)", cfg.WorkspaceRoot, filepath.Dir(dir), err)
	}
}

func TestDotOrchestraConfigIsFoundLast(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, filepath.Join(".orchestra", "config.json"), `{"lock_lease_seconds": 60}`)
	cfg, err := Load(dir)
	if err != nil || cfg.LockLeaseSeconds != 60 || cfg.WorkspaceRoot != dir {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestYAMLConfigFailsClearly(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "orchestra.yaml", "git:\n  auto_push: false\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "YAML") {
		t.Fatalf("got %v", err)
	}
}

func TestSaveWritesPythonStyleJSONThatLoadsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orchestra.json")
	if err := Save(Default(), path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "{\n  \"workspace_root\": \".\",\n  \"orchestra_dir\": \".orchestra\",\n  \"coordination_file\": null,") {
		t.Fatalf("unexpected layout:\n%s", data)
	}
	cfg, err := Load(dir)
	if err != nil || cfg.WorkspaceRoot != dir || len(cfg.IgnorePatterns) != 9 {
		t.Fatalf("%+v %v", cfg, err)
	}
}
