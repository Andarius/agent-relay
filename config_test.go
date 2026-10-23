package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
		check   func(*testing.T, *Config)
	}{
		{"defaults", Config{Name: "alice", Peers: map[string]Peer{"bob": {Tag: "tag:agent-bob"}}}, "", func(t *testing.T, c *Config) {
			if c.Hostname != "relay-alice" || c.Peers["bob"].Host != "relay-bob" || c.MCPListen != "127.0.0.1:8770" {
				t.Fatalf("defaults not applied: %+v", c)
			}
		}},
		{"peer without identity", Config{Name: "alice", Peers: map[string]Peer{"bob": {}}}, "needs a tag or a login", nil},
		{"peer tag without prefix", Config{Name: "alice", Peers: map[string]Peer{"bob": {Tag: "agent-bob"}}}, "must start with tag:", nil},
		{"own tag without prefix", Config{Name: "alice", Tags: []string{"agent-alice"}}, "must start with tag:", nil},
		{"bad name", Config{Name: "Alice Laptop"}, "lowercase", nil},
		{"peer named like self", Config{Name: "alice", Peers: map[string]Peer{"alice": {Tag: "tag:x"}}}, "invalid or equals", nil},
		{"container without workspace", Config{Name: "alice", Container: "relay-alice"}, "needs a workspace", nil},
		{"container name", Config{Name: "alice", Container: "Relay Alice", Workspace: "/ws"}, "container", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.StateDir = t.TempDir()
			err := tt.cfg.Normalize()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, &tt.cfg)
		})
	}
}

func TestInviteRoundTrip(t *testing.T) {
	t.Setenv("AGENT_RELAY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	tests := []struct {
		name     string
		self     Config
		friend   Peer
		authKey  string
		wantFrom Peer
		wantTags []string
		wantErr  string
	}{
		{"tagged owner, tagged friend", Config{Name: "julien", Tags: []string{"tag:agent-julien"}}, Peer{Tag: "tag:agent-alice"}, "tskey-auth-x",
			Peer{Host: "relay-julien", Tag: "tag:agent-julien"}, []string{"tag:agent-alice"}, ""},
		{"login owner, login friend", Config{Name: "julien", SelfLogin: "j@example.com"}, Peer{Login: "a@example.com"}, "",
			Peer{Host: "relay-julien", Login: "j@example.com"}, nil, ""},
		{"owner identity unknown", Config{Name: "julien"}, Peer{Tag: "tag:agent-alice"}, "", Peer{}, nil, "identity is unknown"},
		{"friend without identity", Config{Name: "julien", SelfLogin: "j@example.com"}, Peer{}, "", Peer{}, nil, "needs a tag or a login"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.self
			cfg.StateDir = t.TempDir()
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			inv, err := MakeInvite(&cfg, tt.friend, "alice", tt.authKey)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if _, added := cfg.Peers["alice"]; added {
					t.Fatal("failed invite still added the peer")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeInvite(inv.Encode())
			if err != nil {
				t.Fatal(err)
			}
			req := RequestFromInvite(got, "/tmp/ws", "")
			if req.Name != "alice" || req.Peers["julien"] != tt.wantFrom || strings.Join(req.Tags, ",") != strings.Join(tt.wantTags, ",") ||
				req.AuthKey != tt.authKey || req.NewNode != (tt.authKey != "") {
				t.Fatalf("request = %+v", req)
			}
			if cfg.Peers["alice"].Host != "relay-alice" {
				t.Fatalf("friend not added as peer: %+v", cfg.Peers)
			}
		})
	}
}

func TestDecodeInviteRejects(t *testing.T) {
	for _, s := range []string{"", "hello", invitePrefix + "!!!", invitePrefix + "e30", Invite{V: 2, Name: "a", From: "b"}.Encode(), Invite{V: 1, Name: "Bad Name", From: "b"}.Encode()} {
		if _, err := DecodeInvite(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestParsePeer(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		want     Peer
		wantErr  bool
	}{
		{"bob=tag:agent-bob", "bob", Peer{Tag: "tag:agent-bob"}, false},
		{"mini=j@example.com,host=relay-x", "mini", Peer{Login: "j@example.com", Host: "relay-x"}, false},
		{"bob", "", Peer{}, true},
		{"bob=tag:x,port=1", "", Peer{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			n, p, err := parsePeer(tt.in)
			if (err != nil) != tt.wantErr || n != tt.wantName || p != tt.want {
				t.Fatalf("got %q %+v %v", n, p, err)
			}
		})
	}
}

func TestPolicySnippetListsExactTags(t *testing.T) {
	cfg := &Config{Name: "julien", Tags: []string{"tag:agent-julien"}, Peers: map[string]Peer{
		"alice": {Host: "relay-alice", Tag: "tag:agent-alice"}, "mini": {Host: "relay-mini", Login: "j@example.com"}}}
	s := policySnippet(cfg)
	for _, want := range []string{`"tag:agent-alice"`, `"tag:agent-julien"`, `"tcp:80"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("snippet missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "*") || strings.Contains(s, "j@example.com") {
		t.Fatalf("snippet has a wildcard or a login:\n%s", s)
	}
	if !strings.Contains(policySnippet(&Config{Name: "x", Peers: map[string]Peer{"a": {Tag: "tag:a"}}}), "has no tag of its own") {
		t.Fatal("untagged relay should be warned about")
	}
	if policySnippet(&Config{Name: "x"}) != "" {
		t.Fatal("untagged config should give no snippet")
	}
}

func TestPrepareIdentity(t *testing.T) {
	hash := func(k string) string { s := sha256.Sum256([]byte(k)); return hex.EncodeToString(s[:]) }
	used := hash("tskey-auth-used")
	tests := []struct {
		name     string
		req      SetupRequest
		wantKept bool
		wantJoin string
	}{
		{"rerun with the same invite keeps the identity", SetupRequest{NewNode: true, AuthKey: "tskey-auth-used"}, true, used},
		{"new invite moves the identity aside", SetupRequest{NewNode: true, AuthKey: "tskey-auth-new"}, false, hash("tskey-auth-new")},
		{"login without key moves the identity aside", SetupRequest{NewNode: true}, false, ""},
		{"not a new node", SetupRequest{AuthKey: "tskey-auth-new"}, true, used},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{StateDir: t.TempDir(), JoinKey: used}
			dir := filepath.Join(cfg.StateDir, "tsnet")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := prepareIdentity(cfg, tt.req, &Progress{}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); (err == nil) != tt.wantKept {
				t.Errorf("identity kept=%v, want %v", err == nil, tt.wantKept)
			}
			if cfg.JoinKey != tt.wantJoin {
				t.Errorf("JoinKey=%q, want %q", cfg.JoinKey, tt.wantJoin)
			}
		})
	}
}
