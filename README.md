# <picture><source media="(prefers-color-scheme: dark)" srcset="logo.svg"><img src="logo-light.svg" alt="" width="34" align="top"></picture> pylon-beacon

**One binary. No open ports. Full node vitals.**

pylon-beacon is a tiny monitoring agent for [PylonMon](https://pylonmon.com).
It **pushes** your machine's vitals out — CPU, memory, per-mount disk,
temperature, load, uptime, plus anything you add — so it works behind NAT,
CGNAT, and firewalls, on any box that can make an outbound HTTPS request.
Nothing scrapes you. Nothing listens. If the box goes silent, you get paged:
**the silence is the signal.**

- Single static binary (Go, standard library only — zero dependencies)
- Linux + Windows
- Auto-creates its own monitor in PylonMon on first push — no clicking
- Extensible: add your own metrics with one config line each

## Install

**Linux** — installs `/usr/local/bin/pylon-beacon`, writes
`/etc/pylon-beacon.conf`, and registers a systemd unit:

```sh
curl -fsSL https://pylonmon.com/beacon.sh | sh
```

**Windows** — PowerShell as Administrator; installs to
`C:\Program Files\pylon-beacon` and registers a SYSTEM scheduled task that
starts at boot and auto-restarts:

```powershell
irm https://pylonmon.com/beacon.ps1 | iex
```

Both installers prompt for your API key (or read `PYLON_KEY` if set).

**From source** (any platform with Go):

```sh
go build -o pylon-beacon .
```

## The API key

Create an **🗼 ingest-scoped** key in PylonMon:
**Settings → Admin → Status page & API → + New key → scope "ingest"**.

Ingest keys can push check-ins and metrics and do **nothing else** — they
can't read your monitors, touch incidents, or change any settings. That makes
one key safe to distribute to every box you own.

## Configuration

Config lives at `/etc/pylon-beacon.conf` (Linux) or
`C:\Program Files\pylon-beacon\beacon.conf` (Windows). Full reference:

```ini
# ---- required ----
key      = pm_xxxxxxxxxxxx        # your ingest-scoped PylonMon API key

# ---- optional ----
url      = https://pylonmon.com   # your PylonMon instance
node     = db01                   # monitor name; defaults to this hostname
interval = 20                     # seconds between pushes (default 20, min 15)

# ---- extend what it collects ----
# Each entry under [custom] runs on every push. The FIRST NUMBER found in the
# command's output becomes the metric value, reported under the entry's name.
[custom]
gpu_temp_c    = nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader
queue_depth   = redis-cli llen jobs
zpool_errors  = sh -c "zpool status -x | grep -c 'DEGRADED\|FAULTED'"
battery_pct   = powershell -c "(Get-CimInstance Win32_Battery).EstimatedChargeRemaining"
# is the LOCAL site up? 1/0 — pair with a vital rule `site_up < 1`
site_up       = sh -c "curl -sf http://localhost/ >/dev/null && echo 1 || echo 0"
```

**Monitoring a web server?** For *public* up/down, a normal off-site PylonMon
HTTP monitor is the better tool — it sees what your visitors see. Use the
`site_up` recipe above for a *local* check the outside world can't do (is the
app answering on localhost even when the public route is broken?), then set a
vital rule `site_up < 1` on the node monitor.

Notes on `[custom]`:

- Commands run as the service user, with the agent's environment.
- Anything printable-as-a-number works: shell one-liners, scripts, CLIs.
- A failing command just skips that metric for the push — it never blocks
  the heartbeat.
- Name metrics with units in the suffix (`_pct`, `_c`, `_s`) and PylonMon
  formats them accordingly.

## Built-in collectors

| Metric        | Linux source              | Windows source            |
|---------------|---------------------------|---------------------------|
| `cpu_pct`     | `/proc/stat` delta        | `GetSystemTimes` delta    |
| `mem_pct`     | `/proc/meminfo`           | `GlobalMemoryStatusEx`    |
| `disk_pct` (per mount) | `statfs`         | `GetDiskFreeSpaceEx`      |
| `load1`       | `/proc/loadavg`           | —                         |
| `temp_c`      | `/sys/class/thermal`      | — (add via `[custom]`)    |
| `uptime_s`    | `/proc/uptime`            | `GetTickCount64`          |

## What happens in PylonMon

- The first push **auto-creates a node monitor** named after the machine — a
  first-class **BEACON** monitor type. It counts as one monitor against your plan.
- Deadline = **1.5× your push interval** (minimum 30s), so the default 20s
  agent is flagged DOWN after ~30s of silence — alert channels, escalation
  ladders, and incidents all fire like any PylonMon monitor. Tune it per node
  in the monitor's edit form.
- The monitor's detail card shows the live **🗼 Node vitals** grid — with
  per-vital sparklines from recent pushes and daily min/avg/max trends kept
  for your plan's retention window.
- **Threshold rules**: on the node monitor you can set per-vital alerts
  (`disk_pct / > 90`, `temp_c > 75`, …) that page your channels the moment a
  push crosses the line — and send an all-clear when it recovers.
- Attach runbooks, labels, and SLOs like any other monitor.

## The wire format

The agent is a convenience, not a requirement. Anything that can POST JSON
over HTTPS can be a beacon:

```sh
curl -X POST https://pylonmon.com/api/ingest/exporter \
  -H "Authorization: Bearer $PYLON_KEY" \
  -d '{"node":"db01","interval":60,"metrics":{"cpu_pct":12.3,"mem_pct":44.1,"disk_pct":{"/":81},"temp_c":52}}'
```

`metrics` accepts numbers and one level of nesting (for per-mount disk and
the like). Up to 40 metrics per push.

## Service management

Linux:

```sh
systemctl status pylon-beacon
journalctl -u pylon-beacon -f
```

Windows (PowerShell as admin):

```powershell
Get-Service pylon-beacon
Restart-Service pylon-beacon
```

## License

MIT.
