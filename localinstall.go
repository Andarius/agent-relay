package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	localMCPName = "agent-relay-local"
	vibeBegin    = "# agent-relay begin"
	vibeEnd      = "# agent-relay end"
)

// hookEvents are the Codex and Claude hooks local mode needs: session start registers, turn start marks
// busy, turn end wakes or marks idle.
var hookEvents = []string{"SessionStart", "UserPromptSubmit", "Stop"}

const localUsage = `Usage:
  agent-relay local install [--bin PATH] codex|vibe|claude ...   attach local messaging to these agents
  agent-relay local uninstall codex|vibe|claude ...              detach it
`

func cmdLocal(args []string) error {
	if len(args) == 0 {
		return errors.New(localUsage)
	}
	fs := flag.NewFlagSet("local "+args[0], flag.ContinueOnError)
	bin := fs.String("bin", "", "agent-relay binary the agents run (default: agent-relay on PATH, else this one)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	clients := fs.Args()
	if len(clients) == 0 {
		return errors.New(localUsage)
	}
	for _, c := range clients {
		if !slices.Contains([]string{"codex", "vibe", "claude"}, c) {
			return fmt.Errorf("unknown agent %q: codex, vibe or claude", c)
		}
	}
	switch args[0] {
	case "install":
		exe, err := relayBinary(*bin)
		if err != nil {
			return err
		}
		for _, c := range clients {
			if err := installLocal(c, exe); err != nil {
				return fmt.Errorf("%s: %w", c, err)
			}
			fmt.Printf("%s: local messaging attached (%s)\n", c, exe)
		}
		if slices.Contains(clients, "codex") {
			fmt.Println("codex: approve the new hooks once with /hooks in an interactive session")
		}
	case "uninstall":
		for _, c := range clients {
			if err := uninstallLocal(c); err != nil {
				return fmt.Errorf("%s: %w", c, err)
			}
			fmt.Printf("%s: local messaging detached\n", c)
		}
	default:
		return errors.New(localUsage)
	}
	return nil
}

func relayBinary(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if p, err := exec.LookPath("agent-relay"); err == nil {
		return filepath.Abs(p)
	}
	return os.Executable()
}

func runQuiet(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installLocal(client, exe string) error {
	switch client {
	case "codex":
		bin, _ := json.Marshal(exe) // a JSON string is a valid TOML basic string
		table := []string{"command = " + string(bin), `args = ["mcp"]`, `default_tools_approval_mode = "approve"`}
		if err := editFile(codexConfigPath(), func(b []byte) ([]byte, error) {
			return setTomlTable(b, "mcp_servers."+localMCPName, table), nil
		}); err != nil {
			return err
		}
		return editFile(codexHooksPath(), func(b []byte) ([]byte, error) { return setJSONHooks(b, exe+" hook", nil) })
	case "claude":
		_ = runQuiet("claude", "mcp", "remove", "-s", "user", localMCPName)
		if err := runQuiet("claude", "mcp", "add", "-s", "user", localMCPName, "--", exe, "mcp"); err != nil {
			return err
		}
		return editFile(claudeSettingsPath(), func(b []byte) ([]byte, error) { return setJSONHooks(b, exe+" hook", claudeAllow()) })
	case "vibe":
		_ = runQuiet("vibe", "mcp", "remove", localMCPName)
		if err := runQuiet("vibe", "mcp", "add", localMCPName, "--transport", "stdio", "--command", exe, "--arg", "mcp"); err != nil {
			return err
		}
		if err := editFile(vibeConfigPath(), func(b []byte) ([]byte, error) { return setVibeToolPermissions(b, true), nil }); err != nil {
			return err
		}
		return editFile(vibeHooksPath(), func(b []byte) ([]byte, error) { return setVibeHooks(b, exe+" hook"), nil })
	}
	return fmt.Errorf("unknown agent %q", client)
}

func uninstallLocal(client string) error {
	switch client {
	case "codex":
		if err := editFile(codexConfigPath(), func(b []byte) ([]byte, error) {
			return setTomlTable(b, "mcp_servers."+localMCPName, nil), nil
		}); err != nil {
			return err
		}
		return editFile(codexHooksPath(), func(b []byte) ([]byte, error) { return setJSONHooks(b, "", nil) })
	case "claude":
		_ = runQuiet("claude", "mcp", "remove", "-s", "user", localMCPName)
		return editFile(claudeSettingsPath(), func(b []byte) ([]byte, error) { return setJSONHooks(b, "", claudeAllow()) })
	case "vibe":
		_ = runQuiet("vibe", "mcp", "remove", localMCPName)
		if err := editFile(vibeConfigPath(), func(b []byte) ([]byte, error) { return setVibeToolPermissions(b, false), nil }); err != nil {
			return err
		}
		return editFile(vibeHooksPath(), func(b []byte) ([]byte, error) { return setVibeHooks(b, ""), nil })
	}
	return fmt.Errorf("unknown agent %q", client)
}

// localTools are the local MCP tools; agents run them without asking, since they only touch the local store.
var localTools = []string{"list_agents", "send_message", "read_inbox"}

// setVibeToolPermissions sets (or removes) permission = "always" for each local tool; vibe names MCP
// tools <server>_<tool>.
func setVibeToolPermissions(raw []byte, allow bool) []byte {
	for _, t := range localTools {
		var body []string
		if allow {
			body = []string{`permission = "always"`}
		}
		raw = setTomlTable(raw, "tools."+localMCPName+"_"+t, body)
	}
	return raw
}

// setTomlTable replaces table [header] and its subtables with body, appending it when absent; a nil body
// removes it. Header lines are matched literally, which holds for the files these tools write.
func setTomlTable(raw []byte, header string, body []string) []byte {
	var kept []string
	skipping := false
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "[") {
			name := strings.Trim(t, "[] ")
			skipping = name == header || strings.HasPrefix(name, header+".")
		}
		if !skipping {
			kept = append(kept, line)
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	if body != nil {
		if len(kept) > 0 {
			kept = append(kept, "")
		}
		kept = append(append(kept, "["+header+"]"), body...)
	}
	if len(kept) == 0 {
		return nil
	}
	return []byte(strings.Join(kept, "\n") + "\n")
}

func homeOr(env, dir string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, dir)
}

func codexHooksPath() string  { return filepath.Join(homeOr("CODEX_HOME", ".codex"), "hooks.json") }
func codexConfigPath() string { return filepath.Join(homeOr("CODEX_HOME", ".codex"), "config.toml") }
func vibeConfigPath() string  { return filepath.Join(homeOr("VIBE_HOME", ".vibe"), "config.toml") }
func claudeSettingsPath() string {
	return filepath.Join(homeOr("CLAUDE_CONFIG_DIR", ".claude"), "settings.json")
}
func vibeHooksPath() string { return filepath.Join(homeOr("VIBE_HOME", ".vibe"), "hooks.toml") }

// editFile rewrites path through edit, keeping the previous version as path.bak-agent-relay.
func editFile(path string, edit func([]byte) ([]byte, error)) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated, err := edit(old)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(old, updated) {
		return nil
	}
	if len(old) > 0 {
		if err := os.WriteFile(path+".bak-agent-relay", old, 0o600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, updated, 0o600)
}

// claudeAllow is the permission rule per local tool; Claude names MCP tools mcp__<server>__<tool>.
func claudeAllow() []string {
	rules := make([]string, 0, len(localTools))
	for _, t := range localTools {
		rules = append(rules, "mcp__"+localMCPName+"__"+t)
	}
	return rules
}

// setJSONHooks edits a Codex hooks.json or Claude settings.json: it drops every agent-relay hook group and
// the allow rules, then, when command is set, adds one hook per event and the rules; the rest is kept.
func setJSONHooks(raw []byte, command string, allow []string) ([]byte, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for event, v := range hooks {
		groups, _ := v.([]any)
		kept := slices.DeleteFunc(slices.Clone(groups), isRelayGroup)
		if len(kept) == 0 && len(groups) > 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if command != "" {
		for _, event := range hookEvents {
			groups, _ := hooks[event].([]any)
			hooks[event] = append(groups, map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": command, "timeout": 5,
			}}})
		}
	}
	doc["hooks"] = hooks
	if len(allow) > 0 {
		perms, _ := doc["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		rules, _ := perms["allow"].([]any)
		rules = slices.DeleteFunc(slices.Clone(rules), func(r any) bool { s, _ := r.(string); return slices.Contains(allow, s) })
		if command != "" {
			for _, a := range allow {
				rules = append(rules, a)
			}
		}
		perms["allow"] = rules
		doc["permissions"] = perms
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n'), err
}

func isRelayGroup(g any) bool {
	m, _ := g.(map[string]any)
	hs, _ := m["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		cmd, _ := hm["command"].(string)
		if f := strings.Fields(cmd); len(f) == 2 && f[1] == "hook" && filepath.Base(f[0]) == "agent-relay" {
			return true
		}
	}
	return false
}

// setVibeHooks replaces the marked agent-relay block of a vibe hooks.toml; an empty command only removes it.
func setVibeHooks(raw []byte, command string) []byte {
	s := string(raw)
	if i := strings.Index(s, vibeBegin); i >= 0 {
		if j := strings.Index(s[i:], vibeEnd); j >= 0 {
			s = s[:i] + strings.TrimLeft(s[i+j+len(vibeEnd):], "\n")
		}
	}
	if command == "" {
		return []byte(s)
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	cmd, _ := json.Marshal(command) // a JSON string is a valid TOML basic string
	block := vibeBegin + "\n"
	block += fmt.Sprintf("[[hooks]]\nname = \"agent-relay-start\"\ntype = \"session_start\"\ncommand = %s\ntimeout = 5\n\n", cmd)
	for _, h := range []struct{ name, typ string }{{"agent-relay-busy", "pre_agent"}, {"agent-relay-wake", "post_agent"}} {
		block += fmt.Sprintf("[[hooks]]\nname = %q\ntype = %q\ncommand = %s\nmatch = \"main\"\ntimeout = 5\n\n", h.name, h.typ, cmd)
	}
	return []byte(s + block + vibeEnd + "\n")
}
