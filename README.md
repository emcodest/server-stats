# server-stats

A terminal dashboard for the box hosting LiveKit — CPU, memory, disk, and
network bandwidth/port-speed utilization, refreshed every couple seconds.
Linux-only (reads `/proc` and `/sys/class/net` directly, no dependencies).

## Build

Cross-compile from macOS/whatever for the server's architecture:

```sh
GOOS=linux GOARCH=amd64 go build -o server-stats .   # most VPS/cloud boxes
GOOS=linux GOARCH=arm64 go build -o server-stats .   # arm64 hosts
```

Copy the binary over and run it directly — no runtime dependencies:

```sh
scp server-stats you@your-livekit-host:~/
ssh you@your-livekit-host ./server-stats
```

## Configuration

No flags — everything is an env var, so it can run unmodified across
different boxes/tiers:

| Var                | Default                              | Notes                                                                                              |
| ------------------ | ------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `NETWORK_IF`        | auto-detect                          | Skips auto-detect entirely if set. Auto-detect uses the default-route interface first (the one actually carrying internet traffic), falling back to the first non-virtual interface in `/sys/class/net`. On a Docker host (which a LiveKit box usually is) this matters — without it, `docker0`/`veth*`/`br-*` sort alphabetically ahead of the real NIC and get picked instead, silently reporting container-internal traffic as if it were the box's real bandwidth. |
| `PORT_SPEED_GBPS`   | `1.0`                                 | Only used as a fallback — the NIC's own negotiated link speed (`/sys/class/net/<iface>/speed`) is read first and normally overrides this. Falls back here when that's unreadable, which is common on virtualized/cloud NICs. |
| `INTERVAL_SECONDS`  | `2`                                   | Refresh interval. |

Example:

```sh
NETWORK_IF=eth0 PORT_SPEED_GBPS=2 ./server-stats
```

## Redis publishing (for the Vapp admin dashboard)

Set both `SERVER_ID` and `REDIS_ADDR` to also publish each snapshot to
Redis, on top of the terminal dashboard (which keeps running exactly as
before — publishing is additive, not a replacement). Leave either unset
and nothing is published; the tool behaves exactly as it does today.

| Var               | Default    | Notes                                                                 |
| ------------------ | ---------- | ---------------------------------------------------------------------- |
| `SERVER_ID`         | *(unset)*  | Label this box publishes under, e.g. `lk1`, `lk2` — shows up as-is in the admin dashboard. Required to publish. |
| `REDIS_ADDR`        | *(unset)*  | `host:port` of the same Redis instance vapp-api uses. Required to publish. |
| `REDIS_PASSWORD`    | *(unset)*  | Optional. |

Each tick, a JSON snapshot is written to `vapp:livekit:stats:<SERVER_ID>`
with a TTL of `5 * INTERVAL_SECONDS` (minimum 15s) — so a box that stops
publishing (crashed, network partition, stopped) simply drops out of the
admin dashboard once its key expires, with nothing to manually deregister.
Adding a new LiveKit box is just: run this binary on it with a unique
`SERVER_ID` pointed at the same Redis — the dashboard picks it up on its
own.

A Redis error is logged and never fatal — publishing failing must never
take down the terminal monitor.

Example:

```sh
SERVER_ID=lk1 REDIS_ADDR=10.0.0.5:6379 REDIS_PASSWORD=secret ./server-stats
```

## What "port speed" means here

`IN utilization` / `OUT utilization` are current throughput as a percentage
of the configured (or NIC-reported) port speed — the number to watch if
LiveKit's media traffic is approaching the box's actual uplink capacity,
distinct from CPU/RAM pressure. The scaling-signal section at the bottom
flags when two or more of CPU/RAM/network-out/disk cross a warning
threshold, as a rough "consider adding another LiveKit node" nudge.
