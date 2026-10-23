package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed assets/AGENTS.md
var agentsMD []byte

//go:embed assets/doorbell.sh
var doorbellSH []byte

// SetupRequest is everything the setup page or CLI collects.
type SetupRequest struct {
	Name      string          `json:"name"`
	Workspace string          `json:"workspace"`
	Sandbox   string          `json:"sandbox"`
	Clients   []string        `json:"clients,omitempty"`
	Tags      []string        `json:"tags"`
	Peers     map[string]Peer `json:"peers"`
	AuthKey   string          `json:"authkey,omitempty"`
	NewNode   bool            `json:"new_node"`
}

// Progress collects human-readable setup steps for the CLI and the page.
type Progress struct {
	mu    sync.Mutex
	lines []string
	echo  bool
}

func (p *Progress) Add(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	p.mu.Lock()
	p.lines = append(p.lines, line)
	p.mu.Unlock()
	if p.echo {
		fmt.Fprintln(os.Stderr, "•", line)
	}
}

func (p *Progress) Lines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...)
}

// ApplySetup writes config and workspace, starts the relay and wires the sandbox; the caller owns the returned relay.
func ApplySetup(ctx context.Context, req SetupRequest, prog *Progress) (*Config, *Running, error) {
	cfg := &Config{Name: req.Name, Workspace: req.Workspace, Sandbox: req.Sandbox, Clients: req.Clients, Tags: req.Tags, Peers: req.Peers}
	if prev, err := LoadConfig(); err == nil {
		cfg.MCPListen, cfg.SelfLogin, cfg.JoinKey = prev.MCPListen, prev.SelfLogin, prev.JoinKey
	}
	if err := cfg.Normalize(); err != nil {
		return nil, nil, err
	}
	if mcpUp(cfg.MCPListen) {
		return nil, nil, fmt.Errorf("a relay is already running on %s; end that session first", cfg.MCPListen)
	}
	if err := prepareIdentity(cfg, req, prog); err != nil {
		return nil, nil, err
	}
	if err := cfg.Save(); err != nil {
		return nil, nil, err
	}
	path, _ := configPath()
	prog.Add("config saved to %s", path)

	if cfg.Workspace != "" {
		if err := writeWorkspace(cfg.Workspace); err != nil {
			return nil, nil, err
		}
		prog.Add("workspace ready at %s", cfg.Workspace)
	}
	tok, err := loadOrCreateToken(filepath.Join(cfg.StateDir, "mcp-token"))
	if err != nil {
		return nil, nil, err
	}
	cfg.MCPToken = tok

	run, err := StartRelay(ctx, cfg, req.AuthKey)
	if err != nil {
		return nil, nil, err
	}
	prog.Add("relay %s started; joining the tailnet as %s", cfg.Name, cfg.Hostname)
	return cfg, run, nil
}

// FinishSetup waits for the tailnet, records this node's identity and wires sbx.
func FinishSetup(ctx context.Context, cfg *Config, run *Running, prog *Progress) error {
	announced := false
	for {
		state, url := run.Status()
		if state == "running" {
			break
		}
		if msg, ok := strings.CutPrefix(state, "error: "); ok {
			return errors.New(msg)
		}
		if state == "needs-login" && !announced {
			prog.Add("open this link to approve the node: %s", url)
			announced = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	lc, err := run.ts.LocalClient()
	if err != nil {
		return err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return err
	}
	if st.Self != nil {
		if u, ok := st.User[st.Self.UserID]; ok && (st.Self.Tags == nil || st.Self.Tags.Len() == 0) {
			cfg.SelfLogin = u.LoginName
		} else {
			cfg.SelfLogin = ""
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		ident := cfg.SelfLogin
		if ident == "" {
			ident = strings.Join(cfg.Tags, ",")
		}
		prog.Add("on the tailnet as %s (%s)", strings.TrimSuffix(st.Self.DNSName, "."), ident)
	}
	if err := wireClients(cfg, prog); err != nil {
		return err
	}
	if cfg.Sandbox == "" {
		return nil
	}
	if _, err := exec.LookPath("sbx"); err != nil {
		prog.Add("sbx not found: skipping the sandbox; attach the relay to your agent by hand")
		return nil
	}
	return wireSandbox(cfg, prog)
}

// prepareIdentity moves the tailnet identity aside for a new node, unless it joined with this same single-use key.
func prepareIdentity(cfg *Config, req SetupRequest, prog *Progress) error {
	if !req.NewNode {
		return nil
	}
	dir := filepath.Join(cfg.StateDir, "tsnet")
	_, statErr := os.Stat(dir)
	var joinKey string
	if req.AuthKey != "" {
		sum := sha256.Sum256([]byte(req.AuthKey))
		joinKey = hex.EncodeToString(sum[:])
	}
	if statErr == nil && joinKey != "" && joinKey == cfg.JoinKey {
		prog.Add("this invite was already used here: keeping the current tailnet identity")
		return nil
	}
	if statErr == nil {
		bak := dir + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.Rename(dir, bak); err != nil {
			return err
		}
		prog.Add("previous tailnet identity moved to %s", bak)
	}
	cfg.SelfLogin, cfg.JoinKey = "", joinKey
	return nil
}

func writeWorkspace(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".agent-inbox"), 0o755); err != nil {
		return err
	}
	files := map[string]struct {
		data []byte
		mode os.FileMode
	}{"AGENTS.md": {agentsMD, 0o644}, "CLAUDE.md": {agentsMD, 0o644}, "doorbell.sh": {doorbellSH, 0o755}}
	for name, f := range files {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// sbxRun runs an sbx command and returns its output with the MCP token masked.
func sbxRun(token string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("sbx", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.ReplaceAll(out.String(), token, "<token>"), err
}

func wireSandbox(cfg *Config, prog *Progress) error {
	url := fmt.Sprintf("http://%s/mcp/%s", cfg.MCPListen, cfg.MCPToken)
	mcpName := cfg.SbxMCPName()
	_, _ = sbxRun(cfg.MCPToken, "mcp", "rm", "--force", mcpName)
	if out, err := sbxRun(cfg.MCPToken, "mcp", "add", mcpName, "--url", url); err != nil {
		return fmt.Errorf("sbx mcp add: %v: %s", err, lastLine(out))
	}
	prog.Add("registered the relay with sbx")
	list, err := sbxRun(cfg.MCPToken, "ls")
	if err != nil {
		return fmt.Errorf("sbx ls: %v: %s", err, lastLine(list))
	}
	exists := false
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == cfg.Sandbox {
			exists = true
		}
	}
	if exists {
		// "agent-relay" is the shared entry older installs used.
		for _, old := range []string{"agent-relay", mcpName} {
			_, _ = sbxRun(cfg.MCPToken, "mcp", "unload", old, "--sandbox", cfg.Sandbox)
		}
		if out, err := sbxRun(cfg.MCPToken, "mcp", "load", mcpName, "--sandbox", cfg.Sandbox); err != nil {
			return fmt.Errorf("sbx mcp load: %v: %s", err, lastLine(out))
		}
		prog.Add("reloaded the relay into sandbox %s", cfg.Sandbox)
		return nil
	}
	if cfg.Workspace == "" {
		return errors.New("a sandbox needs a workspace folder")
	}
	if out, err := sbxRun(cfg.MCPToken, "create", "--name", cfg.Sandbox, "--static-mcp", mcpName, "claude", cfg.Workspace); err != nil {
		return fmt.Errorf("sbx create: %v: %s", err, lastLine(out))
	}
	prog.Add("created sandbox %s; log Claude in once with: sbx run --name %s", cfg.Sandbox, cfg.Sandbox)
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// Invite carries what a friend needs to reach this relay; it can contain a tailnet auth key, so send it privately.
type Invite struct {
	V        int      `json:"v"`
	Name     string   `json:"name"`
	Tags     []string `json:"tags,omitempty"`
	AuthKey  string   `json:"authkey,omitempty"`
	From     string   `json:"from"`
	FromPeer Peer     `json:"from_peer"`
}

const invitePrefix = "agent-relay-invite:"

func (i Invite) Encode() string {
	data, _ := json.Marshal(i)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(data)
}

func DecodeInvite(s string) (Invite, error) {
	var inv Invite
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), invitePrefix)
	if !ok {
		return inv, errors.New("not an agent-relay invite")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return inv, fmt.Errorf("invite: %w", err)
	}
	if err := json.Unmarshal(data, &inv); err != nil {
		return inv, fmt.Errorf("invite: %w", err)
	}
	if inv.V != 1 || !nameRe.MatchString(inv.Name) || !nameRe.MatchString(inv.From) {
		return inv, errors.New("invite: unsupported or malformed")
	}
	return inv, nil
}

// MakeInvite adds the friend as a peer of this relay and returns the invite to send them.
func MakeInvite(cfg *Config, friend Peer, friendName, authKey string) (Invite, error) {
	self := Peer{Host: cfg.Hostname}
	switch {
	case len(cfg.Tags) > 0:
		self.Tag = cfg.Tags[0]
	case cfg.SelfLogin != "":
		self.Login = cfg.SelfLogin
	default:
		return Invite{}, errors.New("this relay's identity is unknown: finish setup (join the tailnet) first")
	}
	if friend.Host == "" {
		friend.Host = "relay-" + friendName
	}
	cfg.Peers[friendName] = friend
	if err := cfg.Normalize(); err != nil {
		delete(cfg.Peers, friendName)
		return Invite{}, err
	}
	if err := cfg.Save(); err != nil {
		return Invite{}, err
	}
	inv := Invite{V: 1, Name: friendName, AuthKey: authKey, From: cfg.Name, FromPeer: self}
	if friend.Tag != "" {
		inv.Tags = []string{friend.Tag}
	}
	return inv, nil
}

// RequestFromInvite turns an invite into a setup request for the friend's machine.
func RequestFromInvite(inv Invite, workspace, sandbox string) SetupRequest {
	return SetupRequest{
		Name: inv.Name, Workspace: workspace, Sandbox: sandbox, Tags: inv.Tags,
		Peers: map[string]Peer{inv.From: inv.FromPeer}, AuthKey: inv.AuthKey, NewNode: inv.AuthKey != "",
	}
}
