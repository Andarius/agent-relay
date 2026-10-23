package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// knownClients are the agent CLIs init can attach the relay to directly, outside a sandbox.
var knownClients = []string{"claude", "codex"}

// clientRun runs an agent CLI in dir and returns its output with the MCP token masked.
func clientRun(dir, token string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, &out, &out
	err := cmd.Run()
	return strings.ReplaceAll(out.String(), token, "<token>"), err
}

// clientCommands returns the remove and add commands that attach the relay to one client.
// Claude gets it in local scope for the workspace only; Codex has no per-folder scope, so it is user-wide.
func clientCommands(client, url string) (rm, add []string) {
	switch client {
	case "claude":
		return []string{"claude", "mcp", "remove", "-s", "local", "agent-relay"},
			[]string{"claude", "mcp", "add", "-s", "local", "-t", "http", "agent-relay", url}
	case "codex":
		return []string{"codex", "mcp", "remove", "agent-relay"},
			[]string{"codex", "mcp", "add", "agent-relay", "--url", url}
	}
	return nil, nil
}

func wireClients(cfg *Config, prog *Progress) error {
	url := fmt.Sprintf("http://%s/mcp/%s", cfg.MCPListen, cfg.MCPToken)
	for _, c := range cfg.Clients {
		if _, err := exec.LookPath(c); err != nil {
			prog.Add("%s not found: skipping it", c)
			continue
		}
		rm, add := clientCommands(c, url)
		_, _ = clientRun(cfg.Workspace, cfg.MCPToken, rm...)
		if out, err := clientRun(cfg.Workspace, cfg.MCPToken, add...); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(add[:3], " "), err, lastLine(out))
		}
		prog.Add("attached the relay to %s", c)
	}
	return nil
}

func unwireClients(cfg *Config, prog *Progress) {
	for _, c := range cfg.Clients {
		if _, err := exec.LookPath(c); err != nil {
			continue
		}
		rm, _ := clientCommands(c, "")
		if _, err := clientRun(cfg.Workspace, cfg.MCPToken, rm...); err == nil {
			prog.Add("detached the relay from %s", c)
		}
	}
	if cfg.Sandbox != "" {
		if _, err := exec.LookPath("sbx"); err == nil {
			if _, err := sbxRun(cfg.MCPToken, "mcp", "rm", "--force", cfg.SbxMCPName()); err == nil {
				prog.Add("removed the relay from sbx; delete the sandbox with: sbx rm %s", cfg.Sandbox)
			}
		}
	}
}
