package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerCommand(t *testing.T) {
	token := strings.Repeat("a", 64)
	uid := fmt.Sprintf("AGENT_RELAY_UID=%d", os.Getuid())
	tests := []struct {
		name     string
		goos     string
		wantArgs []string
		wantURL  string
	}{
		{"colima on macos reaches the host loopback", "darwin", []string{
			"docker", "run", "-it", "--rm", "--name", "relay-alice", "--hostname", "relay-alice",
			"-v", "/ws:/workspace", "-v", "relay-alice-home:/home/node", "-e", uid, "-e", "AGENT_RELAY_MCP_URL", containerImage(),
		}, "http://host.docker.internal:8770/mcp/" + token},
		{"linux shares the host network", "linux", []string{
			"docker", "run", "-it", "--rm", "--name", "relay-alice", "--hostname", "relay-alice", "--network", "host",
			"-v", "/ws:/workspace", "-v", "relay-alice-home:/home/node", "-e", uid, "-e", "AGENT_RELAY_MCP_URL", containerImage(),
		}, "http://127.0.0.1:8770/mcp/" + token},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Container: "relay-alice", Workspace: "/ws", MCPListen: "127.0.0.1:8770", MCPToken: token}
			args, env := containerCommand(cfg, tt.goos)
			if strings.Join(args, " ") != strings.Join(tt.wantArgs, " ") {
				t.Errorf("args:\n%s\nwant:\n%s", strings.Join(args, " "), strings.Join(tt.wantArgs, " "))
			}
			if want := []string{"AGENT_RELAY_MCP_URL=" + tt.wantURL}; strings.Join(env, " ") != strings.Join(want, " ") {
				t.Errorf("env = %v, want %v", env, want)
			}
		})
	}
}

func TestContainerCommandKeepsTokenOutOfArgs(t *testing.T) {
	token := strings.Repeat("b", 64)
	cfg := &Config{Container: "relay-alice", ContainerCLI: "podman", Workspace: "/ws", MCPListen: "127.0.0.1:8770", MCPToken: token}
	args, _ := containerCommand(cfg, "darwin")
	if args[0] != "podman" || strings.Contains(strings.Join(args, " "), token) {
		t.Fatalf("args = %v", args)
	}
}

func TestEnsureImage(t *testing.T) {
	tests := []struct {
		name    string
		present bool
		want    []string
	}{
		{"builds the image when missing", false, []string{"image inspect " + containerImage(), "build -t " + containerImage()}},
		{"reuses the image when present", true, []string{"image inspect " + containerImage()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			logf := filepath.Join(t.TempDir(), "calls")
			status := map[bool]string{true: "0", false: "1"}[tt.present]
			stub := "#!/bin/sh\necho \"$1 $2 $3\" >> " + logf + "\n[ \"$1\" = image ] && exit " + status + "\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			if err := ensureImage(&Config{Container: "relay-alice"}, &Progress{}); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(logf)
			if got := strings.TrimSpace(string(data)); got != strings.Join(tt.want, "\n") {
				t.Errorf("calls:\n%s\nwant:\n%s", got, strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestRequestBaseContainerTarget(t *testing.T) {
	req := requestBase(&initAnswers{Name: "alice", Workspace: "/ws", Targets: []string{"container"}})
	want := SetupRequest{Name: "alice", Workspace: "/ws", Container: "relay-alice", Peers: map[string]Peer{}}
	if req.Container != want.Container || req.Sandbox != "" || len(req.Clients) != 0 {
		t.Fatalf("request = %+v, want %+v", req, want)
	}
}
