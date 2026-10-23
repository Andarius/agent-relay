package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"sync"
	"time"
)

//go:embed assets/setup.html
var setupHTML []byte

// setupUI serves the local setup page; one relay may run while the page is open.
type setupUI struct {
	token  string
	addr   string
	bypass bool

	mu   sync.Mutex
	run  *Running
	cfg  *Config
	prog *Progress
	err  string
	busy bool
	done chan struct{}
}

func runSetupGUI(open, bypass bool) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	ui := &setupUI{token: hex.EncodeToString(buf), addr: ln.Addr().String(), prog: &Progress{}, done: make(chan struct{}),
		bypass: bypass}
	if cfg, err := LoadConfig(); err == nil {
		ui.cfg = cfg
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", ui.page)
	mux.HandleFunc("GET /api/state", ui.guard(ui.state))
	mux.HandleFunc("POST /api/apply", ui.guard(ui.apply))
	mux.HandleFunc("POST /api/invite", ui.guard(ui.invite))
	mux.HandleFunc("POST /api/decode-invite", ui.guard(ui.decodeInvite))
	mux.HandleFunc("POST /api/done", ui.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); close(ui.done) }()
	}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	url := fmt.Sprintf("http://%s/?t=%s", ui.addr, ui.token)
	fmt.Fprintf(os.Stderr, "agent-relay setup: %s\n(press Ctrl-C or click Done when finished)\n", url)
	if open {
		openBrowser(url)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-sig:
	case <-ui.done:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.run != nil {
		ui.run.Close()
	}
	return nil
}

// openBrowser opens url with $BROWSER or the OS default; it returns false when nothing could be started.
func openBrowser(url string) bool {
	var cmd *exec.Cmd
	switch {
	case os.Getenv("BROWSER") != "":
		cmd = exec.Command(os.Getenv("BROWSER"), url)
	case runtime.GOOS == "darwin":
		cmd = exec.Command("open", url)
	case runtime.GOOS == "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Keep the opener's chatter off the terminal the wizard draws on.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// guard rejects requests without the per-launch token or with a foreign Host (DNS rebinding).
func (ui *setupUI) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Host != ui.addr || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Setup-Token")), []byte(ui.token)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		h(w, r)
	}
}

func (ui *setupUI) page(w http.ResponseWriter, r *http.Request) {
	if r.Host != ui.addr || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com")
	_, _ = w.Write(setupHTML)
}

func (ui *setupUI) state(w http.ResponseWriter, _ *http.Request) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	_, sbxErr := exec.LookPath("sbx")
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	out := map[string]any{
		"config": ui.cfg, "busy": ui.busy, "error": ui.err, "log": ui.prog.Lines(),
		"sbx": sbxErr == nil, "home": home, "hostname": host,
	}
	if ui.run != nil {
		state, url := ui.run.Status()
		out["tailnet"], out["login_url"] = state, url
	}
	writeJSON(w, http.StatusOK, out)
}

func (ui *setupUI) apply(w http.ResponseWriter, r *http.Request) {
	var req SetupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.Bypass = ui.bypass
	ui.mu.Lock()
	if ui.busy {
		ui.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "setup already running"})
		return
	}
	if ui.run != nil {
		ui.run.Close()
		ui.run = nil
	}
	ui.busy, ui.err, ui.prog = true, "", &Progress{}
	prog := ui.prog
	ui.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		cfg, run, err := ApplySetup(ctx, req, prog)
		ui.mu.Lock()
		ui.cfg, ui.run = cfg, run
		ui.mu.Unlock()
		if err == nil {
			err = FinishSetup(ctx, cfg, run, prog)
		}
		ui.mu.Lock()
		defer ui.mu.Unlock()
		ui.busy = false
		if err != nil {
			ui.err = err.Error()
			return
		}
		prog.Add("done: start agent sessions with `agent-relay run`")
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (ui *setupUI) invite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Friend  string `json:"friend"`
		Tag     string `json:"tag"`
		Login   string `json:"login"`
		AuthKey string `json:"authkey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.cfg == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "set up this machine first"})
		return
	}
	inv, err := MakeInvite(ui.cfg, Peer{Tag: req.Tag, Login: req.Login}, req.Friend, req.AuthKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invite": inv.Encode(), "config": ui.cfg, "policy": policySnippet(ui.cfg)})
}

func (ui *setupUI) decodeInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Invite string `json:"invite"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	inv, err := DecodeInvite(req.Invite)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": inv.Name, "tags": inv.Tags, "from": inv.From,
		"from_peer": inv.FromPeer, "has_authkey": inv.AuthKey != "", "authkey": inv.AuthKey})
}

// policySnippet lists the tailnet policy entries that fence tagged relays to each other's port 80.
func policySnippet(cfg *Config) string {
	tags := map[string]bool{}
	for _, t := range cfg.Tags {
		tags[t] = true
	}
	for _, p := range cfg.Peers {
		if p.Tag != "" {
			tags[p.Tag] = true
		}
	}
	if len(tags) == 0 {
		return ""
	}
	list := make([]string, 0, len(tags))
	owners := map[string][]string{}
	for t := range tags {
		list = append(list, t)
		owners[t] = []string{"autogroup:admin"}
	}
	sort.Strings(list)
	snippet := map[string]any{
		"tagOwners": owners,
		"grants":    []map[string]any{{"src": list, "dst": list, "ip": []string{"tcp:80"}}},
	}
	data, _ := json.MarshalIndent(snippet, "", "  ")
	if len(cfg.Tags) == 0 {
		return "// This relay has no tag of its own: give it one (e.g. tag:agent-" + cfg.Name + ") so these rules cover it too.\n" + string(data)
	}
	return string(data)
}
