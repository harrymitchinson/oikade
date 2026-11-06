# Deploying Oikade on a Raspberry Pi

Oikade runs as one process on one machine, so one Pi is all a home needs ([OIK-143](https://linear.app/oikade/issue/OIK-143)).

## One Pi, with the host

**What you need:** a Raspberry Pi 4 or 5 with a 64-bit OS, booting from an SSD if you can, on the same subnet as your iPhone and home hub, with IPv6 on. You run [Zigbee2MQTT](https://www.zigbee2mqtt.io) and an MQTT broker yourself, on this Pi or another. Your Mac needs Go, and `oikade`:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade@main
```

**Install.** Use the name your SSH config has for the Pi. You need sudo there. The host runs as the user you log in as, which is what lets `oikade --host` reach it.

```sh
oikade install --host pi -- --broker mqtt://oikade@broker:1883 --broker-password-secret broker
```

If your broker needs no password, leave out the user and `--broker-password-secret`. If it does, set the secret and restart the host, as it can't connect until then:

```sh
oikade secret set broker --host pi
ssh pi sudo systemctl restart oikade
```

`oikade status --host pi` should answer. `journalctl -u oikade -f` on the Pi is its log, and `http://pi:8080/metrics` its metrics.

If that Pi runs a firewall, open Matter's port and mDNS: `sudo ufw allow 5540/udp` and `sudo ufw allow 5353/udp`.

**Your home.** Start a repo and deploy it:

```sh
oikade init home && cd home
oikade deploy --host pi
```

The [README](../README.md#getting-started-in-go) has the rest. `oikade matter --host pi` shows the code to pair with the Home app.

**Back up.** Your backup has two parts:

```sh
ssh pi oikade backup
rsync -a --rsync-path="sudo rsync" pi:/var/lib/oikade/ pi-backup/oikade/          # everything Oikade has
```

The second part is Zigbee2MQTT's own data directory, wherever you keep it, which holds the network key and the database. It isn't Oikade's, and Oikade's backup doesn't have it. Losing it means pairing every Zigbee device again.

The copy holds your secrets and the bridge's keys in the clear, so keep it private.

**Restore to a new Pi.** The same address and subnet keep the Home app happy, and nothing needs pairing again:

```sh
oikade install --host new-pi -- --broker mqtt://oikade@broker:1883 --broker-password-secret broker
ssh new-pi sudo systemctl stop oikade
rsync -a --rsync-path="sudo rsync" pi-backup/oikade/ new-pi:/var/lib/oikade/
ssh new-pi 'cd /var/lib/oikade && sudo cp "$(sudo ls -d backups/* | tail -1)" oikade.db && sudo chown -R "$USER": . && sudo systemctl start oikade'
```

The `cp` puts the snapshot where the store goes, since the `oikade.db` in the copy was changing as it was copied. A controller paired since that backup isn't in it, and needs pairing again.

**Upgrade.** Update `oikade` on your Mac, then:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade@main
oikade upgrade --host pi
```

That builds this version for the Pi, backs up the store, swaps the binary and restarts the host. Matter stops with it, so the Home app shows no response until it's back, and the command says how long that took. If the new version doesn't come up, the old one goes back. Afterwards, update `oikade` in your home's `go.mod` and run `oikade deploy --host pi`, as your code is built against the old version.

**Monitor.** Oikade serves Prometheus metrics, so point any Prometheus at it. This job is the whole of what it needs:

```yaml
scrape_configs:
- job_name: oikade
  static_configs:
  - targets: ["pi:8080"]
```

Then import [`config/grafana/oikade-home.json`](../config/grafana/oikade-home.json) into Grafana, for the **Oikade home** dashboard: temperature and humidity per room, light and door timelines, motion events, battery levels, availability, rule runs, active holds, their next changes, and which level is winning each arbitrated capability. Metrics show device names and values, so keep that port on your own network, or move it with `--metrics-addr`.

## Troubleshooting

| Symptom | Check |
|---|---|
| Bridge doesn't appear in the Home app | `dns-sd -B _matterc._udp` from a Mac on the same network. If it's missing: has the 15-minute window passed (`oikade matter --host pi` says, and restarting the host opens it again)? Is the Pi on the same subnet as the home hub? Does a firewall drop mDNS? |
| "Accessory not found" or pairing fails | `journalctl -u oikade` on the Pi. Is IPv6 on for the Pi and the network? Is there a home hub? |
| A capability is missing | Oikade doesn't map that expose yet, e.g. colour, colour temperature or multi-gang switches (`state_l2`). |
| The Home app shows "No Response" | Does `oikade status --host pi` show the device offline? Is Zigbee2MQTT or the broker down? Is the host itself up? |
| A rule never fires | Is it in `oikade status --host pi`, and not paused? `oikade tail --host pi` shows changes and runs as they happen, and `oikade_rule_runs_total` shows whether it's hitting `condition_false`. Rules use the machine's time zone, so check it if the condition uses the time. |
| A plugin keeps restarting | `journalctl -u oikade` on the Pi has its output and why it stopped. |
| `oikade --host pi` can't reach the host | The SSH user must be the one running the host, with `oikade` on its `PATH`. |
