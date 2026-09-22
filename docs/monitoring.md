# Monitoring a fleet

`skrog status --prometheus` prints the numbers `--stats` collects as Prometheus
text, so a machine's health lands in the dashboard you already run.

```
skrog status --prometheus
```

```
# HELP skrog_engine_state Engine state; 1 on the live one. idle means idle-stopped on purpose, not broken.
# TYPE skrog_engine_state gauge
skrog_engine_state{state="running"} 1
skrog_engine_state{state="idle"} 0
skrog_engine_state{state="stopped"} 0
# HELP skrog_bridge_transport Live bridge transport; 1 on the active one. fallback is the slow path.
# TYPE skrog_bridge_transport gauge
skrog_bridge_transport{transport="vsock"} 1
skrog_bridge_transport{transport="fallback"} 0
skrog_bridge_transport{transport="direct"} 0
```

## This is not telemetry

Skrog sends nothing anywhere, and this does not change that. There is no
listener, no port and no daemon: the command writes to stdout and stops. The
no-telemetry promise is about what **we** collect. What you measure on your own
machines is yours, and this exists so you can.

## Wiring it up

The output is the [node_exporter textfile
format](https://github.com/prometheus/node_exporter#textfile-collector). Point
node_exporter at a directory and have a scheduled task write into it:

```powershell
# every 5 minutes, into node_exporter's --collector.textfile.directory
skrog status --prometheus | Set-Content -Encoding utf8 C:\metrics\skrog.prom
```

Write to a temporary file and rename it if your collector is fussy about
reading a half-written file; a rename is atomic and the collector never sees a
partial scrape.

There is deliberately **no `--listen`**. A resident HTTP endpoint would be a new
network surface on a product whose [security model](security.md) is careful
about exactly that, and it would need its own ACL story. The textfile route
needs none of it.

## Exit code

`--prometheus` exits **0 whenever it could write metrics**, including when the
engine is down or nothing is installed.

That is deliberate, and it differs from plain `skrog status`, which exits 1 on a
stopped engine. The engine's state is *in* the metrics — `skrog_engine_state`
and `skrog_installed` say it precisely. A non-zero exit would make a wrapper
script discard the reading exactly when it is most worth having.

## What you get

`skrog_build_info` carries the identity; everything else is a number.

| Metric | Type | What it tells you |
| --- | --- | --- |
| `skrog_build_info{version,backend,distro,engine_version}` | gauge | Always 1 — read the labels. **This is the fleet-drift query**: `skrog upgrade` answers "am I current?" for one machine, this answers it for fifty |
| `skrog_installed` | gauge | 1 when an engine is installed. Absent metrics below when 0 |
| `skrog_engine_state{state}` | gauge | `running` / `idle` / `stopped`; `idle` is healthy |
| `skrog_desired_state{state}` | gauge | What the operator last asked for. Disagreeing with `engine_state` for long is the alert worth having |
| `skrog_supervisor_running` | gauge | 1 when a supervisor holds the lock |
| `skrog_supervisor_reading_fresh` | gauge | 0 means the supervisor's numbers are stale — it may have died (#166) |
| `skrog_supervisor_reading_age_seconds` | gauge | Age of that reading |
| `skrog_supervisor_uptime_seconds` | gauge | Supervisor uptime |
| `skrog_engine_uptime_seconds` | gauge | Engine uptime; 0 when down |
| `skrog_engine_starts_total` | counter | Engine starts since the supervisor started |
| `skrog_idle_stops_total` | counter | Times the idle timeout reclaimed the RAM |
| `skrog_bridge_transport{transport}` | gauge | `vsock` / `fallback` / `direct`. **The one number that explains a slow `docker` with a healthy engine** — fallback costs ~165 ms per connection against vsock's ~0.6 ms |
| `skrog_bridge_connections_total` | counter | Connections accepted |
| `skrog_bridge_bytes_total{direction}` | counter | Bytes relayed, `to_engine` / `to_client` |
| `skrog_bridge_active_connections` | gauge | Currently open |
| `skrog_containers{state}` | gauge | `running` / `paused` / `stopped` |
| `skrog_images`, `skrog_volumes` | gauge | Counts in the engine's store |
| `skrog_images_bytes`, `skrog_volumes_bytes`, `skrog_build_cache_bytes` | gauge | What they hold |
| `skrog_engine_reclaimable_bytes` | gauge | What `docker system prune` could free |
| `skrog_vhdx_size_bytes` | gauge | What the `.vhdx` occupies on the host volume |
| `skrog_vhdx_guest_used_bytes` | gauge | What the filesystem inside it uses |
| `skrog_vhdx_reclaimable_bytes` | gauge | Roughly what [`skrog compact`](housekeeping.md) could return — an estimate, since compaction works in blocks |
| `skrog_host_free_bytes` | gauge | Free space on the volume holding the engine's data |
| `skrog_vm_cpus`, `skrog_vm_memory_total_bytes`, `skrog_vm_memory_available_bytes`, `skrog_vm_swap_total_bytes` | gauge | What the WSL2 VM has — see [VM sizing](vm-sizing.md) |

Engine, disk and VM metrics need a running engine, and collecting them costs WSL
calls. `skrog_stats_probed` is 0 when the engine was down, and those metrics are
**absent** rather than zero — "no data" and "zero containers" must not look
alike.

Nothing here starts the engine to answer, and nothing wakes an idle-stopped one.
Safe to poll.

## Three alerts worth having

```promql
# The engine is down but somebody asked for it to be up.
skrog_desired_state{state="running"} == 1 and skrog_engine_state{state="stopped"} == 1

# The bridge fell back to the slow transport; docker still works, 250x slower per connection.
skrog_bridge_transport{transport="fallback"} == 1

# The supervisor stopped flushing: its numbers, and probably the supervisor, are gone.
skrog_supervisor_reading_fresh == 0
```

## See also

[JSON output contract](cli-json.md) for the `--json` shapes ·
[Housekeeping](housekeeping.md) for acting on the disk numbers ·
[`skrog top`](memory.md) for where one machine's VM memory goes, live
