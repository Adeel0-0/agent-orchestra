// Package config loads orchestra.json with the same schema and defaults as
// orchestra/config.py.
//
// Phase 5 (strict validation) is HALTED in the Go port: unknown keys are ignored,
// there is no ConfigError type and no exit code 2. A file that exists but cannot
// be parsed still fails loudly; it never silently means the defaults.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Adeel0-0/agent-orchestra/internal/pyjson"
)

// GitSync mirrors GitSyncConfig.
type GitSync struct {
	Enabled      bool     `json:"enabled"`
	MainBranch   string   `json:"main_branch"`
	SyncBranches []string `json:"sync_branches"`
	Remote       string   `json:"remote"`
	AutoCommit   bool     `json:"auto_commit"`
	AutoPush     bool     `json:"auto_push"`
	CommitPrefix string   `json:"commit_prefix"`
}

// CI mirrors CiConfig.
type CI struct {
	Enabled             bool   `json:"enabled"`
	Provider            string `json:"provider"`
	WorkflowName        string `json:"workflow_name"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
}

// Pipeline mirrors PipelineConfig; nil means the command is not configured.
type Pipeline struct {
	VerifyCommand   *string `json:"verify_command"`
	PreClaimCommand *string `json:"pre_claim_command"`
	PostSyncCommand *string `json:"post_sync_command"`
	OnErrorCommand  *string `json:"on_error_command"`
}

// Config mirrors OrchestraConfig, in the same field order (it is what `init` writes).
type Config struct {
	WorkspaceRoot    string   `json:"workspace_root"`
	OrchestraDir     string   `json:"orchestra_dir"`
	CoordinationFile *string  `json:"coordination_file"`
	WatchPaths       []string `json:"watch_paths"`
	IgnorePatterns   []string `json:"ignore_patterns"`
	DebounceMS       int      `json:"debounce_ms"`
	LockLeaseSeconds int      `json:"lock_lease_seconds"`
	Git              GitSync  `json:"git"`
	CI               CI       `json:"ci"`
	Pipeline         Pipeline `json:"pipeline"`
}

// Default returns the same defaults as OrchestraConfig().
func Default() Config {
	return Config{
		WorkspaceRoot: ".",
		OrchestraDir:  ".orchestra",
		WatchPaths:    []string{"."},
		IgnorePatterns: []string{
			".git/*", ".orchestra/*", "node_modules/*", "__pycache__/*",
			"*.pyc", "*.log", ".next/*", "dist/*", "build/*",
		},
		DebounceMS:       400,
		LockLeaseSeconds: 1800,
		Git: GitSync{
			Enabled: true, MainBranch: "main", SyncBranches: []string{}, Remote: "origin",
			AutoCommit: true, AutoPush: true, CommitPrefix: "chore(orchestra):",
		},
		CI: CI{
			Provider: "github_actions", WorkflowName: "backend-verification.yml",
			PollIntervalSeconds: 15, TimeoutSeconds: 600,
		},
	}
}

// Candidates are searched in this order, as in Python.
var Candidates = []string{"orchestra.json", "orchestra.yaml", "orchestra.yml", filepath.Join(".orchestra", "config.json")}

// Load finds the first config file in rootDir (the current directory if empty)
// and returns it, or the defaults when no file exists. workspace_root is made
// absolute relative to rootDir, as Python's load_config does.
func Load(rootDir string) (Config, error) {
	if rootDir == "" {
		var err error
		if rootDir, err = os.Getwd(); err != nil {
			return Config{}, err
		}
	}
	root, err := filepath.Abs(rootDir)
	if err != nil {
		return Config{}, err
	}
	for _, name := range Candidates {
		path := filepath.Join(root, name)
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && info.IsDir()) {
			continue
		}
		cfg, err := loadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", path, err)
		}
		if !filepath.IsAbs(cfg.WorkspaceRoot) {
			cfg.WorkspaceRoot = filepath.Join(root, cfg.WorkspaceRoot)
		}
		cfg.WorkspaceRoot = filepath.Clean(cfg.WorkspaceRoot)
		return cfg, nil
	}
	cfg := Default()
	cfg.WorkspaceRoot = root
	return cfg, nil
}

func loadFile(path string) (Config, error) {
	if ext := filepath.Ext(path); ext == ".yaml" || ext == ".yml" {
		// Python reads these with PyYAML; no YAML parser is among the Go port's allowed dependencies.
		return Config{}, errors.New("YAML configuration is not supported by the Go binary; use orchestra.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var top any
	if err := json.Unmarshal(data, &top); err != nil {
		return Config{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, ok := top.(map[string]any); !ok {
		return Config{}, errors.New("top level must be an object")
	}
	// TODO(phase 5, halted): strict validation. Unknown keys are ignored here on
	// purpose; Python rejects them. Keys that are left out keep their defaults.
	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.Git.SyncBranches == nil {
		cfg.Git.SyncBranches = []string{}
	}
	return cfg, nil
}

// Save writes cfg like Python's json.dump(cfg.to_dict(), f, indent=2).
func Save(cfg Config, path string) error {
	data, err := pyjson.MarshalIndent(cfg)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// Str returns the value of an optional command, or "" when unset.
func Str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
