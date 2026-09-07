# CuliPulse agent — uptime monitoring that also watches what's behind your firewall

A small Go probe that monitors your infrastructure **from inside your own network** — private
services UptimeRobot and Pingdom can't reach. It pulls check assignments from a CuliPulse
deployment, runs them (`http` / `cert` / `tcp` / `icmp` / `udp`) plus optional network
discovery, and reports results back. Outbound-only: no inbound ports, no VPN. Built into a
distroless image by `Dockerfile`.

> **This repository is a read-only, open-source (Apache-2.0) mirror of the agent that CuliPulse
> ships.** It is published so you can **audit exactly what runs inside your network** before
> you run it. Development happens upstream; changes land here on each tagged release. See
> [Source & contributing](#source--contributing).

## Why you can trust it in your network (security model)

The agent is designed to be safe to run behind your firewall:

- **Outbound-only.** It opens connections *out* to the CuliPulse API to fetch work and post
  results. It exposes **no inbound port** and needs **no inbound firewall rule** — nothing on
  the internet can reach it. By default the agent's only outbound destination is
  `CULIPULSE_API_URL`; error telemetry to Sentry is **opt-in** — it happens only if you set the
  optional `SENTRY_DSN` env var, and is off unless you do.
- **No inbound control channel.** It is a poller: on an interval (`CULIPULSE_POLL_SECONDS`,
  default 30) it asks the API "what should I check?" There is no remote-exec / command channel,
  no shell in the image, and it never writes files from server input.
- **It probes only what your account configures.** The agent runs the checks *you* create in
  your CuliPulse account — including services on **private/internal addresses that never touch
  the internet** (that is the whole point of running it inside your network). It reaches nothing
  you didn't configure a monitor for.
- **Credentials are env-injected, never baked in.** The only secret is the agent token
  (`CULIPULSE_AGENT_TOKEN`), passed as an environment variable at run time — not compiled into
  the binary, not stored in this source, and never written to logs.
- **TLS is fully verified** against the system roots (the distroless image ships CA certs); the
  agent never disables certificate verification.
- **Least privilege.** Runs as a distroless container with no shell. `--cap-add=NET_RAW` is
  requested **only if you use ICMP checks** — drop it otherwise.

### Trust boundary — read this

The agent executes the probe instructions its control plane (`CULIPULSE_API_URL`) hands it, and
reports results back over the outbound channel. It does **not** independently allowlist targets,
so it extends the same trust you would give any hosted monitoring agent you enrol: whoever
controls your CuliPulse account (and CuliPulse itself, as the operator) can direct it at targets
reachable from where it runs. Run it with that in mind — on a host with only the network reach
it needs. Platform-operated (shared) CuliPulse probes are separately restricted from
private/internal addresses; your own agent is intentionally allowed into your own network.

All of the above is verifiable in this source — that is the point of publishing it.

## Check types

`http` and `tcp` can also run from the CuliPulse edge (no agent); `icmp` and `udp` are
agent-only. A `udp` check sends an optional payload and is "up" when a reply arrives within the
timeout (matching the optional `expect` substring, in utf8 or hex); silence is always "down", so
supply a protocol-correct payload for a meaningful check. The agent advertises its capabilities
(`http,cert,tcp,icmp,udp`) at enrollment.

## Image

`ghcr.io/culiops/culipulse-agent:latest` (also tagged per commit `sha-<short>` and, on `v*`
releases, by semver).

The image is **multi-arch** (`linux/amd64` + `linux/arm64`) — `docker run …:latest` auto-selects
your host's architecture, so it runs natively on Apple Silicon and ARM cloud VMs
(AWS Graviton / Ampere) as well as x86-64. Each agent reports its architecture on check-in; the
dashboard shows it per agent.

## Run

Get the exact command (with your token) from the dashboard: **Agents → enroll**, or any agent's
detail page → **Connect this agent**. It looks like:

    docker run -d --restart=unless-stopped --cap-add=NET_RAW \
      -e CULIPULSE_API_URL=https://culipulse.dev \
      -e CULIPULSE_AGENT_TOKEN=cpa_... \
      ghcr.io/culiops/culipulse-agent:latest

`--cap-add=NET_RAW` is needed only for ICMP checks; drop it otherwise.

### Environment variables

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `CULIPULSE_API_URL` | yes | — | Your CuliPulse deployment URL |
| `CULIPULSE_AGENT_TOKEN` | yes | — | The enrollment token (`cpa_…`) |
| `CULIPULSE_POLL_SECONDS` | no | `30` | How often to fetch work |
| `CULIPULSE_MAX_CONCURRENCY` | no | `100` | Max concurrent checks |
| `CULIPULSE_SUMMARY_SECONDS` | no | `60` | Summary/heartbeat interval |
| `CULIPULSE_DISCOVER_CIDR` | no | — | CIDR range to scan for network discovery |
| `SENTRY_DSN` | no | — | **Optional, off by default.** If set, recovered panics are reported to this Sentry DSN (errors only). Unset = no telemetry and no calls to anything but `CULIPULSE_API_URL`. |

## Updating

**Recommended — auto-update sidecar (set once):** run Watchtower scoped to just the agent
container; it pulls and recreates it in place when a new image ships, preserving your env/token:

    docker run -d --name culipulse-watchtower --restart=unless-stopped \
      -v /var/run/docker.sock:/var/run/docker.sock \
      containrrr/watchtower --cleanup culipulse-<name>

If you run more than one agent on the same host, give each Watchtower a unique `--name`
(e.g. `culipulse-watchtower-<name>`) so they don't conflict.

**Manual:** from the dashboard (Agents → your agent → **Update this agent**), click
**Generate update command** — it mints a fresh token and gives you a
`docker pull … && docker rm -f … && docker run …` block pinned to the new version.

The dashboard shows each agent's version and whether an update is available.

## Build from source

    go build -o culipulse-agent .        # requires Go 1.25+
    go test ./...                          # run the test suite

The agent depends only on the Go standard library and the modules pinned in `go.mod` (the AWS SDK,
used only for optional AWS discovery, and `sentry-go`, used only when `SENTRY_DSN` is set).

## Source & contributing

This repo is a **one-way mirror** — the source of truth is the CuliPulse monorepo, and each
tagged release is published here automatically. You are welcome to **read, audit, and open
issues** against this repository. For security reports, please disclose privately rather than in
a public issue.

## License

Licensed under the [Apache License 2.0](./LICENSE).
