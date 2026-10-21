package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

// Running is a started relay: the MCP endpoint is up immediately, the peer API once the tailnet is.
type Running struct {
	ts      *tsnet.Server
	store   *Store
	mcpSrv  *http.Server
	peerSrv *http.Server

	mu       sync.Mutex
	state    string // "starting", "needs-login", "running", "error: …"
	loginURL string
}

func (r *Running) Status() (state, loginURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.loginURL
}

func (r *Running) set(state, url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = state
	if url != "" {
		r.loginURL = url
	}
}

// StartRelay starts the relay; authKey, when set, is only used if this node has no tailnet identity yet.
func StartRelay(ctx context.Context, cfg *Config, authKey string) (*Running, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	store, err := OpenStore(filepath.Join(cfg.StateDir, "relay.db"))
	if err != nil {
		return nil, err
	}
	run := &Running{store: store, state: "starting"}
	run.ts = &tsnet.Server{
		Hostname:      cfg.Hostname,
		Dir:           filepath.Join(cfg.StateDir, "tsnet"),
		AuthKey:       authKey,
		AdvertiseTags: cfg.Tags,
		Logf:          func(string, ...any) {},
		UserLogf: func(f string, a ...any) {
			msg := fmt.Sprintf(f, a...)
			if i := strings.Index(msg, "https://login.tailscale.com/"); i >= 0 {
				url := strings.Fields(msg[i:])[0]
				run.set("needs-login", url)
				slog.Info("tailnet login needed", "url", url)
			}
		},
	}
	if err := run.ts.Start(); err != nil {
		store.Close()
		return nil, err
	}
	lc, err := run.ts.LocalClient()
	if err != nil {
		run.Close()
		return nil, err
	}
	relay := &Relay{cfg: cfg, store: store, client: run.ts.HTTPClient(), whois: lc, limiter: newLimiter(rateMax, rateWindow)}

	mcpLn, err := net.Listen("tcp", cfg.MCPListen)
	if err != nil {
		run.Close()
		return nil, fmt.Errorf("mcp listen %s: %w", cfg.MCPListen, err)
	}
	run.mcpSrv = &http.Server{Handler: relay.MCPHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = run.mcpSrv.Serve(mcpLn) }()
	slog.Info("mcp endpoint up", "addr", cfg.MCPListen, "name", cfg.Name)

	go func() {
		if _, err := run.ts.Up(ctx); err != nil {
			run.set("error: "+err.Error(), "")
			return
		}
		ln, err := run.ts.Listen("tcp", fmt.Sprintf(":%d", peerPort))
		if err != nil {
			run.set("error: "+err.Error(), "")
			return
		}
		run.peerSrv = &http.Server{Handler: relay.PeerHandler(), ReadHeaderTimeout: 10 * time.Second}
		run.set("running", "")
		slog.Info("peer api up", "url", fmt.Sprintf("http://%s:%d", cfg.Hostname, peerPort), "peers", cfg.PeerNames())
		_ = run.peerSrv.Serve(ln)
	}()
	return run, nil
}

func (r *Running) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range []*http.Server{r.mcpSrv, r.peerSrv} {
		if s != nil {
			_ = s.Shutdown(ctx)
		}
	}
	if r.ts != nil {
		_ = r.ts.Close()
	}
	if r.store != nil {
		_ = r.store.Close()
	}
}

// mcpUp reports whether something already answers on the MCP address.
func mcpUp(addr string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/mcp")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return true
}
