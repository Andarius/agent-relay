package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed assets/container/Dockerfile
var containerDockerfile []byte

//go:embed assets/container/entrypoint.sh
var containerEntrypoint []byte

// containerImage is tagged with the hash of its build files, so a new agent-relay rebuilds a changed image.
func containerImage() string {
	sum := sha256.Sum256(append(append([]byte(nil), containerDockerfile...), containerEntrypoint...))
	return "agent-relay-claude:" + hex.EncodeToString(sum[:6])
}

// detectContainerCLI returns the first container CLI on PATH: docker (Colima, Docker Engine) or podman.
func detectContainerCLI() string {
	for _, c := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return ""
}

func (c *Config) containerCLI() string {
	if c.ContainerCLI != "" {
		return c.ContainerCLI
	}
	return "docker"
}

// ContainerVolume keeps Claude's login and settings across the throwaway containers.
func (c *Config) ContainerVolume() string { return c.Container + "-home" }

// containerCommand returns the command that starts Claude in a throwaway container seeing only the workspace,
// and the environment that carries the MCP URL (it holds the token, so it stays out of argv).
// Colima and Docker Desktop route host.docker.internal to the host's loopback; Linux shares the host network instead.
func containerCommand(cfg *Config, goos string) (args, env []string) {
	_, port, _ := net.SplitHostPort(cfg.MCPListen)
	host := "host.docker.internal"
	args = []string{cfg.containerCLI(), "run", "-it", "--rm", "--name", cfg.Container, "--hostname", cfg.Container}
	if goos == "linux" {
		host = "127.0.0.1"
		args = append(args, "--network", "host")
	}
	// The entrypoint gives the container user the host uid, so files written in the workspace stay the owner's.
	args = append(args, "-v", cfg.Workspace+":/workspace", "-v", cfg.ContainerVolume()+":/home/node",
		"-e", fmt.Sprintf("AGENT_RELAY_UID=%d", os.Getuid()), "-e", "AGENT_RELAY_MCP_URL", containerImage())
	url := fmt.Sprintf("http://%s/mcp/%s", net.JoinHostPort(host, port), cfg.MCPToken)
	return args, []string{"AGENT_RELAY_MCP_URL=" + url}
}

// ensureImage builds the sandbox image locally when this version of it is missing.
func ensureImage(cfg *Config, prog *Progress) error {
	cli, image := cfg.containerCLI(), containerImage()
	if err := exec.Command(cli, "image", "inspect", image).Run(); err == nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "agent-relay-image")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for name, data := range map[string][]byte{"Dockerfile": containerDockerfile, "entrypoint.sh": containerEntrypoint} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	prog.Add("building the sandbox image %s with %s (the first time takes a few minutes)", image, cli)
	var out bytes.Buffer
	cmd := exec.Command(cli, "build", "-t", image, dir)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s build: %v: %s", cli, err, lastLine(out.String()))
	}
	prog.Add("built the sandbox image %s", image)
	return nil
}

// wireContainer prepares the container sandbox at setup time: the relay reaches it at run time, nothing to register.
func wireContainer(cfg *Config, prog *Progress) error {
	if _, err := exec.LookPath(cfg.containerCLI()); err != nil {
		prog.Add("%s not found: skipping the container sandbox", cfg.containerCLI())
		return nil
	}
	if err := ensureImage(cfg, prog); err != nil {
		return err
	}
	prog.Add("container sandbox %s ready; log Claude in once inside it on the first agent-relay run", cfg.Container)
	return nil
}

// unwireContainer deletes the sandbox's home volume (Claude's login) and its image.
func unwireContainer(cfg *Config, prog *Progress) {
	cli := cfg.containerCLI()
	if _, err := exec.LookPath(cli); err != nil {
		return
	}
	if err := exec.Command(cli, "volume", "rm", cfg.ContainerVolume()).Run(); err == nil {
		prog.Add("removed the container volume %s", cfg.ContainerVolume())
	}
	if err := exec.Command(cli, "image", "rm", containerImage()).Run(); err == nil {
		prog.Add("removed the sandbox image %s", containerImage())
	}
}
