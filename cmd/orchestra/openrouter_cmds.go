package main

// OpenRouter wiring (orchestra/cli.py on main: models, openrouter-key; registry entry;
// the app server's catalog and key-presence hooks).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/Adeel0-0/agent-orchestra/internal/app"
	"github.com/Adeel0-0/agent-orchestra/internal/openrouter"
	"github.com/Adeel0-0/agent-orchestra/internal/players"
	"github.com/Adeel0-0/agent-orchestra/internal/registry"
)

const (
	openRouterPath    = "https://openrouter.ai/api/v1"
	openRouterVersion = "Public HTTPS API (key presence only; credit/model access not checked)"
)

func init() {
	// OpenRouter goes last, as in Python's PLAYERS.
	registry.Register(registry.Spec{
		ID:          openrouter.PlayerID,
		DisplayName: openrouter.DisplayName,
		LoginHint:   openrouter.LoginHint,
		New:         openrouter.NewPlayer,
		Health:      openRouterHealth,
	})
}

// openRouterHealth reports key presence only, like registry.py's OpenRouter branch.
func openRouterHealth() registry.Health {
	h := registry.Health{ID: openrouter.PlayerID, Name: openrouter.DisplayName}
	present, err := openrouter.KeyPresent()
	if err != nil {
		h.LoggedIn, h.Fix = registry.Bool(false), err.Error()
		return h
	}
	h.Path, h.Version, h.LoggedIn = openRouterPath, openRouterVersion, registry.Bool(present)
	if !present {
		h.Fix = openrouter.LoginHint
	}
	return h
}

func openRouterCommands() []command {
	return []command{
		{name: "openrouter-key", help: "Save the OpenRouter key in the OS keyring (hidden terminal input)", usage: "openrouter-key", stopAfter: -1, run: cmdOpenRouterKey},
		{name: "models", help: "List OpenRouter models that support tools (public catalog; no key needed)", usage: "models [--json]", stopAfter: -1,
			flags: []flagSpec{{Name: "json", Kind: "bool"}}, run: cmdModels},
	}
}

func cmdOpenRouterKey(_ parsed, out, errOut io.Writer) int {
	err := openrouter.SaveKeyInteractive(os.Stdin, out)
	var credErr *openrouter.CredentialError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, openrouter.ErrCancelled):
		fmt.Fprintln(errOut, err)
		return 130
	case errors.Is(err, openrouter.ErrNotInteractive), errors.As(err, &credErr):
		fmt.Fprintln(errOut, err)
		return 2
	default:
		fmt.Fprintln(errOut, err)
		return 2
	}
}

func cmdModels(p parsed, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	models, err := openrouter.Models(ctx)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if p.bools["json"] {
		fmt.Fprintln(out, players.DumpsJSON(models)) // json.dumps(models, ensure_ascii=False)
		return 0
	}
	for _, m := range models {
		context := any("?")
		if v, ok := m["context_length"]; ok && v != nil {
			context = v
		}
		fmt.Fprintf(out, "%-55s context: %v\n", players.PyStr(m["id"]), context)
	}
	return 0
}

// addOpenRouterDependencies gives the app server the catalog and key presence.
// The key itself is never returned to the server.
func addOpenRouterDependencies(cfg *app.Config) {
	cfg.KeyPresent = func() bool {
		present, err := openrouter.KeyPresent()
		return err == nil && present
	}
	cfg.Models = func(ctx context.Context) ([]any, error) {
		models, err := openrouter.Models(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]any, len(models))
		for i, m := range models {
			out[i] = m
		}
		return out, nil
	}
}
