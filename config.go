package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Peer is a remote relay and the Tailscale identity it must present.
type Peer struct {
	Host  string `json:"host"`
	Tag   string `json:"tag,omitempty"`
	Login string `json:"login,omitempty"`
}

// Config is the relay's persistent configuration (~/.config/agent-relay/config.json).
type Config struct {
	Name      string          `json:"name"`
	Hostname  string          `json:"hostname"`
	Tags      []string        `json:"tags,omitempty"`
	Peers     map[string]Peer `json:"peers"`
	Workspace string          `json:"workspace,omitempty"`
	Sandbox   string          `json:"sandbox,omitempty"`
	Clients   []string        `json:"clients,omitempty"`
	MCPListen string          `json:"mcp_listen"`
	SelfLogin string          `json:"self_login,omitempty"`

	StateDir string `json:"-"`
	MCPToken string `json:"-"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func configPath() (string, error) {
	if p := os.Getenv("AGENT_RELAY_CONFIG"); p != "" {
		return p, nil
	}
	if h := os.Getenv("AGENT_RELAY_HOME"); h != "" {
		return filepath.Join(h, "config.json"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-relay", "config.json"), nil
}

// defaultStateDir holds the tailnet identity, messages and MCP token; $AGENT_RELAY_HOME isolates a test install.
func defaultStateDir(hostname string) (string, error) {
	if h := os.Getenv("AGENT_RELAY_HOME"); h != "" {
		return filepath.Join(h, "state", hostname), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local/share/agent-relay", hostname), nil
}

// Normalize fills defaults and rejects configs that would make authorization ambiguous.
func (c *Config) Normalize() error {
	if !nameRe.MatchString(c.Name) {
		return fmt.Errorf("name %q: use lowercase letters, digits and dashes", c.Name)
	}
	if c.Hostname == "" {
		c.Hostname = "relay-" + c.Name
	}
	if c.MCPListen == "" {
		c.MCPListen = "127.0.0.1:8770"
	}
	if c.Peers == nil {
		c.Peers = map[string]Peer{}
	}
	for name, p := range c.Peers {
		if !nameRe.MatchString(name) || name == c.Name {
			return fmt.Errorf("peer name %q is invalid or equals this relay's name", name)
		}
		if p.Host == "" {
			p.Host = "relay-" + name
		}
		if p.Tag == "" && p.Login == "" {
			return fmt.Errorf("peer %q needs a tag or a login to authenticate it", name)
		}
		if p.Tag != "" && !strings.HasPrefix(p.Tag, "tag:") {
			return fmt.Errorf("peer %q: tag must start with tag:", name)
		}
		c.Peers[name] = p
	}
	for _, t := range c.Tags {
		if !strings.HasPrefix(t, "tag:") {
			return fmt.Errorf("own tag %q must start with tag:", t)
		}
	}
	for _, cl := range c.Clients {
		if !slices.Contains(knownClients, cl) {
			return fmt.Errorf("client %q: want one of %s", cl, strings.Join(knownClients, ", "))
		}
		if c.Workspace == "" {
			return fmt.Errorf("client %s needs a workspace folder", cl)
		}
	}
	if c.Workspace != "" {
		abs, err := filepath.Abs(c.Workspace)
		if err != nil {
			return err
		}
		c.Workspace = abs
	}
	if c.StateDir == "" {
		dir, err := defaultStateDir(c.Hostname)
		if err != nil {
			return err
		}
		c.StateDir = dir
	}
	return nil
}

func (c *Config) InboxDir() string {
	if c.Workspace == "" {
		return ""
	}
	return filepath.Join(c.Workspace, ".agent-inbox")
}

func (c *Config) PeerNames() []string {
	names := make([]string, 0, len(c.Peers))
	for n := range c.Peers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadConfig reads and normalizes the config file and loads (or creates) the MCP token.
func LoadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s: run `agent-relay setup` first", path)
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	tok, err := loadOrCreateToken(filepath.Join(c.StateDir, "mcp-token"))
	if err != nil {
		return nil, fmt.Errorf("mcp token: %w", err)
	}
	c.MCPToken = tok
	return &c, nil
}

// Save writes the config atomically, owner-only.
func (c *Config) Save() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
