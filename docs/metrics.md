# Metrics and logs

What the host exposes for Prometheus and Grafana, and what it logs.

## Metrics

The host serves these on `:8080/metrics`, and health on `:8080/healthz`, over HTTP. That listener is read-only and apart from the local API. It's open to whoever can reach the port, and it shows device names and values. `--metrics-addr` moves it, such as to `127.0.0.1:8080`, and an empty value turns it off. If the port is taken the host logs that and carries on without it. Your process's rule metrics reach the host every 5 seconds, so a scrape can be that far behind, and they go when the process does. `oikade_undeclared_devices` and the plugin metrics have no series on the host yet. With Matter off (`--matter-port=0`) there are no `oikade_matter_*` series.

Point Prometheus at it, and import [`config/grafana/oikade-home.json`](../config/grafana/oikade-home.json) into Grafana for the "Oikade home" dashboard.

| Metric | |
|---|---|
| `oikade_capability_value{device,room,capability}` | Current value. Bools are 0 or 1. |
| `oikade_capability_changes_total{…}` | Counts changes, including ones shorter than the scrape interval (a door opened and closed within 15s) |
| `oikade_device_available{device,room}` | 1 online, 0 offline |
| `oikade_device_last_seen_timestamp_seconds{…}` | |
| `oikade_undeclared_devices{instance}` | Paired but not on record |
| `oikade_plugin_up{plugin}` | 1 while the plugin reports itself online with contract 1 |
| `oikade_plugin_rejected_messages_total{plugin,reason}` | Plugin messages dropped as `malformed`, `wrong_contract`, `unknown_capability`, `invalid_value`, `undeclared_device`, `unannounced_capability` or `unknown_device_type` |
| `oikade_rule_runs_total{rule,result}` | `succeeded`, `failed`, `cancelled`, `busy`, `condition_false`, `condition_error`, `missed` |
| `oikade_rule_next_change_timestamp_seconds{rule}` | When a rule's next time of day fires, or its hold next starts or ends |
| `oikade_rule_active{rule}` | 1 while a Rule's hold is active, 0 otherwise |
| `oikade_rule_budget_target_minutes{rule}` | A budget hold's minutes for the current planning day |
| `oikade_rule_budget_scheduled_minutes{rule}` | How many of them today's plan places |
| `oikade_rule_budget_run_minutes_total{rule}` | Minutes a budget hold's device has been on and available |
| `oikade_rule_budget_today_run_minutes{rule}` | Those minutes in the current planning day |
| `oikade_rule_budget_desired{rule}` | 1 while a budget hold's plan wants its device on, 0 otherwise |
| `oikade_device_control_level{device,capability}` | The level that wins an arbitrated capability: 0 default, 1 Normal, 2 Override, 3 Safety |
| `oikade_rule_events_dropped_total` | Capability changes the rule engine dropped because it fell behind |
| `oikade_api_auth_failures_total` | Calls to the network API refused for want of a valid token |
| `oikade_matter_serving` | 1 while the Matter bridge serves controllers, 0 while it can't. `oikade matter` says why |
| `oikade_matter_fabrics` | Fabrics the bridge is paired on, one for each controller's home |
| `oikade_matter_sessions` | CASE sessions that paired controllers hold |
| `oikade_matter_subscriptions` | Subscriptions that controllers hold |
| `oikade_matter_reports_total` | Reports sent on those subscriptions, the first one and keep-alives included |
| `oikade_matter_interactions_total{interaction,cluster,result}` | Attribute `write`s and command `invoke`s from controllers, by cluster (`on_off`, `level_control`, …, or `other`, which includes a whole request refused before any cluster was looked at) and the status they were answered with (`success`, `unsupported_access`, …) |
| `oikade_matter_panics_total{site}` | Panics in handling one Matter message, which cost that message and its session rather than the host, by `site`: `receive`, `handler`, `subscription` or `mdns`. Each is a bug, and the log has its stack. A panic anywhere else in Matter, such as a timer or a commissioning command, still stops the host |
| `oikade_matter_write_duration_seconds` | Histogram of how long a controller's change takes to get through arbitration to its device's driver, for changes that worked. Writes gather for 50 ms first, which this leaves out |

Every capability change is also logged as `"Changed capability"` with structured fields, so Loki has the event history.

## Logging

The host writes structured JSON logs to standard output, and your process and plugins log to standard error. Under systemd, `journalctl -u oikade` has them all. Every capability change is logged as `"Changed capability"` with structured fields.
