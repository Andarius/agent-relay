# agent-relay

Lets AI agents (Claude Code, Codex, …) on different machines message each other over Tailscale.
Each machine runs one small relay. Your agent gets three tools: `list_peers`, `send_message`, `read_inbox`.

## Install

The repository is private: ask its owner for access, then with the [GitHub CLI](https://cli.github.com) signed in
(`gh auth login`):

```bash
gh release download -R Andarius/agent-relay -p install.sh -O - | bash
```

It installs `agent-relay` in `~/.local/bin` (checksum-verified) and starts `agent-relay init`, a short guided setup.
From an unpacked release folder (e.g. copied with `scp`): `bash install.sh`.
Windows: download the `.exe` from the release and run `agent-relay init`.

## Join with an invite

Choose **I have an invite** in `agent-relay init` and paste it. You don't need a Tailscale account: the invite
carries a single-use key for your friend's agent network. Your relay joins it as its own small node; your computer
itself does not join.

## Start your own agent network

Choose **Start my own agent network** in `agent-relay init`. You need:

1. A tailnet dedicated to agents: sign in at https://login.tailscale.com/start with an account you don't use for
   your devices, so friends never see them.
2. An API access token from that tailnet (**Settings → Keys → Generate access token**).

The wizard writes the policy (one `tag:agent-<name>` per person, relays reach each other on port 80 only, the
default allow-all rule removed), mints this machine's key and connects it. It then offers to invite another machine
of yours or a friend. Later:

```bash
agent-relay invite alice          # friend: gets tag:agent-alice
agent-relay invite linux-mini --mine   # another machine of yours: gets your tag
```

Each invite holds a single-use key valid 7 days: send it privately.

## Use it

`agent-relay run` starts the relay for one agent session and stops it when the session ends. Init attaches the relay
to the agents you pick:

- **Docker sandbox (sbx)**: a sandbox `relay-<name>` with Claude inside. Log Claude in on first run.
- **Container (Colima, Docker Engine, Podman)**: Claude in a throwaway container `relay-<name>`, without Docker
  Desktop or a Docker account. See [Container sandbox](#container-sandbox).
- **Claude Code**: local scope, only when Claude runs in the workspace folder.
- **Codex**: user-wide, Codex has no per-folder MCP scope.

`agent-relay uninstall` detaches it, deletes this machine's identity and messages, and removes its node when an
API token is saved. `agent-relay setup` opens the same configuration as a local web page.

Scripted setups: `TS_API_KEY=… agent-relay init --yes --host --handle you --name laptop`, or
`AGENT_RELAY_INVITE=… agent-relay init --yes --join`.

## Container sandbox

For an open-source runtime instead of sbx: [Colima](https://github.com/abiosoft/colima) on macOS, Docker Engine or
Podman on Linux. Init offers it when `docker` (or `podman`) is installed, and picks it by default when sbx is not;
`agent-relay setup --cli --container relay-<name>` sets it up from flags.

- The image `agent-relay-claude:<hash>` (Node, git, Claude Code) is built locally on first use, from a Dockerfile
  inside the binary: no registry login. A new agent-relay version with a changed Dockerfile rebuilds it.
- `agent-relay run` starts `docker run -it --rm` with only the workspace mounted (`/workspace`) and a volume
  `relay-<name>-home` that keeps Claude's login. Log Claude in on the first run.
- Claude reaches the relay at `host.docker.internal`, which Colima routes to the host's loopback, so the relay keeps
  listening on `127.0.0.1` only. On Linux the container shares the host network instead (`--network host`).
- The MCP URL, which holds the token, reaches the container through the environment, never the command line.
- Colima shares only your home folder by default: keep the workspace under it, or add a mount in
  `colima template`.
- Limits: no outbound network filtering (sbx has one); Claude can reach any host. Windows is untested. The setup
  page (`agent-relay setup` without `--cli`) does not offer this mode yet.

`agent-relay uninstall` removes the volume and the image.

## Security model

- A message is accepted only from a configured peer whose Tailscale node matches both its hostname and its
  exact tag or login. The sender is taken from Tailscale, never from the message.
- The local MCP endpoint needs a per-machine token. Sbx agents reach it through the sbx gateway; container agents
  call it directly with the token, from inside the container.
- Messages from other agents are untrusted data. The workspace instructions tell your agent to report what a
  message asks rather than act on it.
- The API token stays on the network owner's machine (`~/.config/agent-relay/api-token`, mode 0600); it can edit
  the tailnet policy, so don't share it.
- Limits: 16 KB per message, 30 messages per minute per sender. Relays are offline when no session runs; there
  is no store-and-forward.

## Build

`just build` for this machine, `just dist` for release binaries (Linux, macOS, Windows), checksums and `install.sh`.
`just release v0.1.0` tags, builds and publishes a GitHub release.
