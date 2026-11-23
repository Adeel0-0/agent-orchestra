package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	gracefulShutdown = 5 * time.Second
	readHeaderLimit  = 10 * time.Second
)

// LaunchConfig configures Serve, RunUI and StartRuntime.
type LaunchConfig struct {
	DataDir string       // "" = ~/.orchestra
	Store   StoreOptions // known players
	Out     io.Writer    // user-facing messages (nil = os.Stdout)
	// App supplies the injected dependencies (Factory, PlayerHealth, Models,
	// KeyPresent, FolderPicker, Web). Store, Port and Token are filled in here.
	App Config
	// OpenBrowser opens url in the default browser (RunUI only; nil = system default).
	OpenBrowser func(url string) error
}

func (c LaunchConfig) out() io.Writer {
	if c.Out == nil {
		return os.Stdout
	}
	return c.Out
}

// ReserveListener binds a random port on IPv4 loopback (exclusively on Windows).
func ReserveListener() (net.Listener, error) {
	lc := net.ListenConfig{Control: exclusiveAddrUse}
	return lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
}

// Runtime is a running local server that owns its store and agent turns.
type Runtime struct {
	App    *App
	Port   int
	Origin string

	srv  *http.Server
	done chan struct{}
	err  error
}

// StartRuntime reserves a loopback port, opens the store and starts serving.
func StartRuntime(cfg LaunchConfig) (*Runtime, error) {
	ln, err := ReserveListener()
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	store, err := OpenStore(cfg.DataDir, cfg.Store)
	if err != nil {
		ln.Close()
		return nil, err
	}
	appCfg := cfg.App
	appCfg.Store, appCfg.Port = store, port
	app, err := New(appCfg)
	if err != nil {
		store.Close()
		ln.Close()
		return nil, err
	}
	rt := &Runtime{
		App: app, Port: port, Origin: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{}),
		srv: &http.Server{Handler: app, ReadHeaderTimeout: readHeaderLimit, IdleTimeout: 2 * time.Minute},
	}
	go func() {
		defer close(rt.done)
		if err := rt.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			rt.err = err
		}
	}()
	return rt, nil
}

// BootstrapURL is the page address with a fresh one-use nonce in the fragment.
// It never contains the launch token.
func (rt *Runtime) BootstrapURL() string {
	return rt.Origin + "/#bootstrap=" + rt.App.Bootstrap.Issue(bootstrapTTL)
}

// Done is closed when the HTTP server stops on its own (or after Stop).
func (rt *Runtime) Done() <-chan struct{} { return rt.done }

// Stop stops accepting requests (5 s grace), then cancels running turns, waits for
// their cleanup and closes the store. Keep the process alive until it returns.
func (rt *Runtime) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdown)
	defer cancel()
	if err := rt.srv.Shutdown(ctx); err != nil {
		rt.srv.Close()
	}
	<-rt.done
	closeErr := rt.App.Close()
	if rt.err != nil {
		return rt.err
	}
	return closeErr
}

// Serve is `orchestra serve`: the API only (no UI routes, no folder picker) on a
// random loopback port, printing the URL and launch token, until ctx is cancelled.
func Serve(ctx context.Context, cfg LaunchConfig) error {
	cfg.App.Web, cfg.App.FolderPicker, cfg.App.Token = nil, nil, ""
	rt, err := StartRuntime(cfg)
	if err != nil {
		return err
	}
	out := cfg.out()
	fmt.Fprintf(out, "Orchestra API: %s\n", rt.Origin)
	fmt.Fprintf(out, "Authorization: Bearer %s\n", rt.App.Token)
	fmt.Fprintln(out, "Keep this launch token private. Ctrl+C stops the server. This command serves the API only.")
	select {
	case <-ctx.Done():
	case <-rt.Done():
	}
	return rt.Stop()
}

// RunUI is `orchestra ui` in browser mode: it serves the UI, opens the default
// browser at a one-use bootstrap URL and runs until ctx is cancelled. There is no
// native window yet, so the folder picker is always unavailable.
func RunUI(ctx context.Context, cfg LaunchConfig) error {
	if cfg.App.Web == nil {
		return ErrUIAssetsMissing
	}
	cfg.App.FolderPicker, cfg.App.Token = nil, ""
	rt, err := StartRuntime(cfg)
	if err != nil {
		return err
	}
	out := cfg.out()
	open := cfg.OpenBrowser
	if open == nil {
		open = openBrowser
	}
	url := rt.BootstrapURL()
	if err := open(url); err != nil {
		// The nonce is one-use and expires in 60 seconds; this terminal is local.
		fmt.Fprintf(out, "Could not open a browser (%v). Open this address within 60 seconds:\n%s\n", err, url)
	} else {
		fmt.Fprintln(out, "Orchestra is open in your browser. Ctrl+C stops the local server.")
	}
	select {
	case <-ctx.Done():
	case <-rt.Done():
	}
	return rt.Stop()
}
