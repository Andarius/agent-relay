package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWireClients(t *testing.T) {
	bin, ws := t.TempDir(), t.TempDir()
	logf := filepath.Join(t.TempDir(), "calls")
	stub := "#!/bin/sh\necho \"$(pwd) ${0##*/} $*\" >> " + logf + "\necho \"done $*\"\n"
	for _, c := range knownClients {
		if err := os.WriteFile(filepath.Join(bin, c), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	cfg := &Config{Workspace: ws, Clients: []string{"claude", "codex"}, MCPListen: "127.0.0.1:8770", MCPToken: strings.Repeat("a", 64)}
	prog := &Progress{}
	if err := wireClients(cfg, prog); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(logf)
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	url := "http://127.0.0.1:8770/mcp/" + cfg.MCPToken
	want := []string{
		ws + " claude mcp remove -s local agent-relay",
		ws + " claude mcp add -s local -t http agent-relay " + url,
		ws + " codex mcp remove agent-relay",
		ws + " codex mcp add agent-relay --url " + url,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if out, _ := clientRun(ws, cfg.MCPToken, "claude", url); strings.Contains(out, cfg.MCPToken) {
		t.Errorf("token leaked in output: %s", out)
	}
}

func TestWireSandboxUsesPerInstallEntry(t *testing.T) {
	url := "http://127.0.0.1:8770/mcp/" + strings.Repeat("a", 64)
	tests := []struct {
		name   string
		listed string
		want   []string
	}{
		{"existing sandbox", "relay-alice", []string{
			"mcp rm --force agent-relay-alice", "mcp add agent-relay-alice --url " + url, "ls",
			"mcp unload agent-relay --sandbox relay-alice", "mcp unload agent-relay-alice --sandbox relay-alice",
			"mcp load agent-relay-alice --sandbox relay-alice",
		}},
		{"new sandbox", "other", []string{
			"mcp rm --force agent-relay-alice", "mcp add agent-relay-alice --url " + url, "ls",
			"create --name relay-alice --static-mcp agent-relay-alice claude /ws",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			logf := filepath.Join(t.TempDir(), "calls")
			stub := "#!/bin/sh\necho \"$*\" >> " + logf + "\n[ \"$1\" = ls ] && echo " + tt.listed + "\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			cfg := &Config{Name: "alice", Sandbox: "relay-alice", Workspace: "/ws", MCPListen: "127.0.0.1:8770",
				MCPToken: strings.Repeat("a", 64)}
			if err := wireSandbox(cfg, &Progress{}); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(logf)
			if got := strings.TrimSpace(string(data)); got != strings.Join(tt.want, "\n") {
				t.Errorf("calls:\n%s\nwant:\n%s", got, strings.Join(tt.want, "\n"))
			}
		})
	}
}
