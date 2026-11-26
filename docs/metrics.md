# Metrics and logs

What the host serves for Prometheus, and what it logs. For what your home did and why, [`oikade history`](working-with-a-home.md#history) is often quicker: metrics are for longer than its event log keeps.

## Metrics

The host serves metrics on `:8080/metrics`, and health on `:8080/healthz`, over HTTP.

- **The listener is read-only,** and apart from the API. It's open to whoever can reach the port, and it shows devices' hardware and values.
- **`--metrics-addr`** moves it, such as to `127.0.0.1:8080`, and an empty value turns it off. If the port is taken, the host logs that and carries on without it.
- **Your process's rule metrics** reach the host every 5 seconds, so a scrape can be that far behind, and they go when the process does. The host serves them only if they're all `oikade_rule_*` and well formed, and logs `Refused rule metrics from your process` when they aren't.
- **Labels:** a `device` label is the device's [ref](devices.md#refs), its hardware's, such as `device="hardware/hue/0x2"`, and a rule metric's `rule` label the case's or the flow's, such as `rule="case/hue/0x2/on/evening"` or `rule="flow/morning"`, so each pastes into `oikade status` or `oikade pause`. `room` and `capability` labels are bare names, for PromQL.
- **Each family** has its `# TYPE`, and an empty `# HELP`: the table below says what each one is.

### Prometheus and Grafana

Point any Prometheus at the host. This job is the whole of what it needs:

```yaml
scrape_configs:
- job_name: oikade
  static_configs:
  - targets: ["pi.home:8080"]
```

Then import [`config/grafana/oikade-home.json`](../config/grafana/oikade-home.json) into Grafana, for the **Oikade home** dashboard: temperature and humidity per room, light and door timelines, motion, battery levels, availability, rule runs, active holds, their next changes, and which level is winning each arbitrated capability.

### The metrics

| Metric | |
|---|---|
| `oikade_capability_value{device,room,capability}` | The current value. A bool is 0 or 1, and a name such as a `mode` its [number](capabilities.md) |
| `oikade_capability_changes_total{device,room,capability}` | Changes, including ones shorter than the scrape interval, such as a door opened and closed within 15 s |
| `oikade_device_available{device,room}` | 1 online, 0 offline, once the plugin has said |
| `oikade_device_last_seen_timestamp_seconds{device,room}` | When the plugin last heard from the device |
| `oikade_undeclared_devices{instance}` | Devices a plugin lists that aren't on record |
| `oikade_device_control_level{device,capability}` | The level that wins an arbitrated capability: 0 default, 1 Normal, 2 Override, 3 Safety |
| `oikade_rule_runs_total{rule,result}` | Runs, by result: `succeeded`, `failed`, `cancelled`, `busy` (a trigger while one ran), `condition_false`, `condition_error`, `missed` (a time of day too late to fire), and for a flow `gave_up` (it woke too late) and `dropped` (its code changed) |
| `oikade_rule_next_change_timestamp_seconds{rule}` | When a rule's next time of day fires, or its case next starts or ends |
| `oikade_rule_active{rule}` | 1 while a case applies, 0 otherwise |
| `oikade_rule_budget_target_minutes{rule}` | A budget's minutes for the current planning day |
| `oikade_rule_budget_scheduled_minutes{rule}` | How many of them today's plan places |
| `oikade_rule_budget_run_minutes_total{rule}` | Minutes a budget's device has been on and available, and offline when it was on as it went and its first report back said on. A stretch offline is added once that report comes, all at once |
| `oikade_rule_budget_missed_minutes_total{rule}` | Minutes of a budget's planned runs that went by with its device off while its conditions were true |
| `oikade_rule_budget_today_run_minutes{rule}` | Those minutes in the current planning day |
| `oikade_rule_budget_desired{rule}` | 1 while a budget's plan wants its device on, 0 otherwise |
| `oikade_rule_events_dropped_total` | Capability changes your automations dropped because they fell behind |
| `oikade_api_auth_failures_total` | Calls to the network API refused for want of a valid token |
| `oikade_matter_serving` | 1 while the Matter bridge serves controllers, 0 while it can't. `oikade status matter` says why |
| `oikade_matter_fabrics` | Fabrics the bridge is paired on, one for each controller's home |
| `oikade_matter_sessions` | CASE sessions that paired controllers hold |
| `oikade_matter_subscriptions` | Subscriptions that controllers hold |
| `oikade_matter_reports_total` | Reports sent on those subscriptions, the first one and keep-alives included |
| `oikade_matter_interactions_total{interaction,cluster,result}` | Attribute `write`s and command `invoke`s from controllers, by cluster (`on_off`, `level_control`, …, or `other`, which includes a whole request refused before any cluster was looked at) and the status they were answered with (`success`, `unsupported_access`, …) |
| `oikade_matter_panics_total{site}` | Panics in handling one Matter message, which cost that message and its session rather than the host, by `site`: `receive`, `handler`, `subscription` or `mdns`. Each is a bug, and the log has its stack. A panic anywhere else in Matter, such as in a timer or a commissioning command, still stops the host |
| `oikade_matter_write_duration_seconds` | A histogram of how long a controller's change takes to get through arbitration to its device's plugin, for changes that worked. It leaves out the time [writes gather](devices.md#writes-from-a-controller) first |

With Matter off (`--matter-port 0`) there are no `oikade_matter_*` series. `oikade_plugin_up` and `oikade_plugin_rejected_messages_total` are defined, but have no series on the host yet.

## Logs

The host writes structured JSON logs to stdout, and your automations and plugins log to stderr. Under systemd, `journalctl -u oikade` has them all, and `journalctl -u oikade -f` follows them.

Some lines worth searching for:

| Message | |
|---|---|
| `Changed capability` | Every capability change but `link-quality`'s, with the device, the capability, the value and the one before |
| `Device went offline`, `Device came back online` | A device's plugin said so |
| `Serving API over TLS` | The [network API](running-a-home.md#the-network-api) is up, with its `pin` |
| `Could not evaluate hold; keeping its state until it can` | A case reads a value nobody has reported. See [A value nobody has reported](automations.md#a-value-nobody-has-reported) |
| `Rule read a Room with no members` | A case reads a room with nothing in it. See [Reading a room or a group](automations.md#reading-a-room-or-a-group) |
| `Refused rule metrics from your process` | See [Metrics](#metrics) |

Errors are under the key `err`, a rule under `rule` as its ref, and a component's name under `logger`.
