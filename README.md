# SentNodes Agent

A small container that runs next to a Sentinel dVPN node container and lets you
manage it from [SentNodes](https://sentnodes.com) - price, RPC, restart,
withdrawal - including live host and container monitoring.

It is a **pull-based** agent: it polls SentNodes for commands and executes them
locally. Nothing connects in to your node, so there is no inbound port to open.
Your wallet keys never leave the box: the server only stores the commands you
queue, never reads your keys, and never connects to your node itself.

## How it works

- Reads the node's `config.toml` and operator key from the shared volume.
- Enrolls by proving control of the operator key (signs a server challenge).
- Polls for commands, executes them locally, and reports results.
- Reports host metrics every 10 minutes and pushes container status changes in
  real time.

## Requirements

- The node container is persistent (no `--rm`) so a restart replays its spec.
- The node keyring uses `backend = "test"` (an unattended agent cannot enter a
  passphrase; the agent refuses to start otherwise).
- The agent shares the node's home directory (it reads `config.toml` and the
  keyring there) and can reach the Docker daemon (via direct socket or a socket-proxy).

dVPN nodes use `/root/.sentinel-dvpnx` inside the node container, which is
commonly bind-mounted from `${HOME}/.sentinel-dvpnx` on the host (per the
[node config docs](https://docs.sentinel.co/dvpn-nodes/setup/manual/node-config)).
The agent mounts that same host directory at the same path.

## Quick start (plain docker, no compose)

The only values you set are your SentNodes Agent Key (you can get it from the
[Manage Account](https://sentnodes.com/user/manage) page) and to enable withdrawals
features, you need to set your withdrawal address. Update `${HOME}/.sentinel-dvpnx`
to wherever your dVPN node's volume actually lives - the agent auto-discovers the
node container by that shared volume.

```bash
docker run -d \
  --name sentnodes-agent \
  --restart unless-stopped \
  -e SENTNODES_AGENT_KEY="paste-your-agent-key" \
  -e WITHDRAWAL_ADDRESS="sent1xxxxxxxxxxxxxxxx" \
  -v ${HOME}/.sentinel-dvpnx:/root/.sentinel-dvpnx \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/busurmedia/sentnodes-agent:latest
```

## Quick start (docker compose)

Same setup with compose - again, point `${HOME}/.sentinel-dvpnx` at your node's volume path:

```yaml
sentnodes-agent:
  image: ghcr.io/busurmedia/sentnodes-agent:latest
  environment:
    SENTNODES_AGENT_KEY: "paste-your-agent-key"
    WITHDRAWAL_ADDRESS: "sent1xxxxxxxxxxxxxxxx"   # optional
  volumes:
    - ${HOME}/.sentinel-dvpnx:/root/.sentinel-dvpnx
    - /var/run/docker.sock:/var/run/docker.sock
  restart: unless-stopped
```

Withdrawals use the node's own CometBFT RPC endpoints (from its `config.toml`,
with a public fallback), so no separate endpoint is needed. Two limits apply, set
in P2P with a 50 P2P minimum amount: a minimum single withdrawal (default 250 P2P)
and a minimum balance always kept (default 50 P2P).

## Environment variables

| Variable | Required | Purpose |
|---|---|---|
| `SENTNODES_AGENT_KEY` | yes | Your SentNodes Agent Key, from the [Manage Account](https://sentnodes.com/user/manage) page. |
| `WITHDRAWAL_ADDRESS` | for withdrawals | Pinned destination (`sent1...`). The server can never change it. |
| `WITHDRAWAL_MIN` | no | Minimum single withdrawal, in P2P (default 250; min 50). |
| `WITHDRAWAL_RESERVE` | no | Balance always kept, in P2P (default 50; min 50). |
| `DVPN_NODE_CONTAINER` | no | Only to disambiguate if volume auto-discovery is ambiguous. |
| `DOCKER_HOST` | no | Docker endpoint. Defaults to `unix:///var/run/docker.sock`. If you're using a Docker socket-proxy, you can put the proxy address here, eg: `tcp://socket-proxy:2375`. |

### Using a socket-proxy (optional hardening)

Instead of bind-mounting the Docker socket into the agent, you can front the
daemon with a socket-proxy that exposes only the few endpoints the agent uses
(list/inspect containers, watch events, restart), then point the agent at it with
`DOCKER_HOST`. The agent itself then needs no socket mount:

```yaml
services:
  socket-proxy:
    image: tecnativa/docker-socket-proxy
    environment:
      CONTAINERS: 1   # list + inspect + stats (GET /containers/...)
      EVENTS: 1       # stream container status changes
      POST: 1         # allow the restart call (POST /containers/.../restart)
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    restart: unless-stopped

  sentnodes-agent:
    image: ghcr.io/busurmedia/sentnodes-agent:latest
    environment:
      SENTNODES_AGENT_KEY: "paste-your-agent-key"
      WITHDRAWAL_ADDRESS: "sent1xxxxxxxxxxxxxxxx"   # optional
      DOCKER_HOST: "tcp://socket-proxy:2375"
    volumes:
      - ${HOME}/.sentinel-dvpnx:/root/.sentinel-dvpnx
    depends_on:
      - socket-proxy
    restart: unless-stopped
```

## Troubleshooting

Check the agent logs first: `docker logs sentnodes-agent`.

- **`SENTNODES_AGENT_KEY is required`** - you can get it from the SentNodes
  [Manage Account](https://sentnodes.com/user/manage) page.
- **`only keyring backend 'test' is supported`** - the node keyring is `file`/`os`.
  An unattended agent cannot enter a passphrase. Re-init the node with
  `keyring.backend = "test"`.
- **`reading /root/.sentinel-dvpnx/config.toml: ... no such file`** - the volume mount does not
  point at the node home. Mount the host dir that contains `config.toml` (usually
  `${HOME}/.sentinel-dvpnx`) at `/root/.sentinel-dvpnx`.
- **`node container not found`** - the agent could not match a container sharing
  its `/root/.sentinel-dvpnx` volume. Make sure the node and agent mount the same host directory,
  or set `DVPN_NODE_CONTAINER` to the node's container name.
- **Agent shows offline in SentNodes** - the agent cannot reach
  `api.sentnodes.com`. Check outbound HTTPS and that the box has working IPv4.
- **Restart / price / RPC commands do nothing** - the node container must be
  persistent (no `--rm`) and the agent must reach the Docker daemon. With a
  socket-proxy, ensure `CONTAINERS=1`, `EVENTS=1`, and `POST=1` are enabled.
- **`withdrawals disabled: WITHDRAWAL_ADDRESS is not set`** - set the pinned
  destination to enable withdrawals.
- **`below the minimum withdrawal`** - raise the amount; `WITHDRAWAL_MIN` defaults
  to 250 P2P (min 50 P2P).
- **`would leave less than the ... reserve`** - the amount plus the gas fee would
  drop the operator balance below the kept reserve (`WITHDRAWAL_RESERVE`, default
  50 P2P, min 50 P2P).
- **Verify keyring access** - the agent only reads the key; it never moves or
  modifies it. If enrollment fails with a signature error, confirm `tx.from_name`
  in `config.toml` matches the key in the keyring.

## License

Copyright 2026 Busur Media Indonesia, PT (Busurnode). Licensed under the Apache
License 2.0; see `LICENSE` and `NOTICE`.
