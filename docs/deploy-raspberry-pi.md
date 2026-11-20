# Deploying Oikade on a Raspberry Pi

Oikade runs as one process on one machine, so one Pi is all a home needs (OIK-143).

## One Pi, with the host

**What you need:** a Raspberry Pi 4 or 5 with a 64-bit OS, booting from an SSD if you can, on the same subnet as your iPhone and home hub, with IPv6 on. You run [Zigbee2MQTT](https://www.zigbee2mqtt.io) and an MQTT broker yourself, on this Pi or another. Your Mac needs Go, and `oikade`:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade@<version>   # the release the Pi runs
```

**Install.** The Pi needs only `oikade-host`, from the release's `oikade-host-linux-arm64.tar.gz`. It installs itself:

```console
$ curl -fsSL https://github.com/harrymitchinson/oikade/releases/download/<version>/oikade-host-linux-arm64.tar.gz | tar xz oikade-host
$ sudo ./oikade-host install
Installing oikade-host v0.3.0 to /usr/local/bin/oikade-host
Created user oikade
Wrote /etc/systemd/system/oikade.service, with the API on :8443
Starting oikade, and waiting for it to serve
Installed oikade-host v0.3.0, running as oikade
On laptop, by 14:32, run:
  oikade login pi.home:8443 --pin sha256:9f2c… --code k7Qx…
```

It copies itself to `/usr/local/bin/oikade-host`, makes the system user `oikade`, writes the unit `/etc/systemd/system/oikade.service`, starts it, and waits until the host answers. The unit keeps the host's data in `/var/lib/oikade` and gives it `/run/oikade`, in memory and only its user's, where it writes [your values](running-a-home.md#your-values) for the processes it starts. `--api-addr` is how your Mac reaches it (`:8443` unless you say), and `--client` who the login line is for (`laptop`). Change the unit with `sudo systemctl edit oikade`, whose drop-in `install` never touches. It writes the unit itself only on the first install, or when you give it `--api-addr`.

That's all the unit says: the rest, such as where Zigbee2MQTT's broker is, is in your home's code and its `.env`, which `oikade deploy` brings. If the broker needs a password, it's one of those values too, `Password: oikade.Var("BROKER_PASSWORD")` in the plugin's config, with `BROKER_PASSWORD=…` in `.env` on your Mac. A new one is a deploy, which starts the plugin again with it.

`journalctl -u oikade -f` on the Pi is its log, and `http://pi:8080/metrics` its metrics.

If that Pi runs a firewall, open Matter's port, mDNS and the API: `sudo ufw allow 5540/udp`, `sudo ufw allow 5353/udp` and `sudo ufw allow 8443/tcp`. `install` prints that line if ufw is on, and leaves the firewall to you.

**Log in from your Mac.** Run the `oikade login` line `install` printed on your Mac within 10 minutes. From then on, `oikade status` on your Mac answers from the Pi. For another machine, or once the code has expired, run `sudo oikade-host add client/NAME` on the Pi for a new line.

**Your home.** Start a repo and deploy it:

```sh
oikade init home && cd home
oikade deploy
```

The [README](../README.md#getting-started-in-go) has the rest, including the token its workflow deploys with. `oikade status matter` shows the code to pair with the Home app.

**Back up.** Your backup has two parts:

```sh
oikade backup
rsync -a --rsync-path="sudo rsync" pi:/var/lib/oikade/ pi-backup/oikade/          # everything Oikade has
```

The second part is Zigbee2MQTT's own data directory, wherever you keep it, which holds the network key and the database. It isn't Oikade's, and Oikade's backup doesn't have it. Losing it means pairing every Zigbee device again.

The copy holds your values, what plugins' setups kept and the bridge's keys in the clear, so keep it private.

**Restore to a new Pi.** Install `oikade-host` as above, and ignore its login line: it's for the empty host it starts, whose identity the restore replaces. Then stop it and put the copy in its place. The same address and subnet keep the Home app happy, and nothing needs pairing again:

```sh
ssh new-pi sudo systemctl stop oikade
rsync -a --delete --rsync-path="sudo rsync" pi-backup/oikade/ new-pi:/var/lib/oikade/
ssh new-pi "sudo sh -c 'cd /var/lib/oikade && cp \"\$(ls -d backups/* | tail -1)\" oikade.db && chown -R oikade: . && systemctl start oikade'"
```

The `cp` puts the snapshot where the store goes, since the `oikade.db` in the copy was changing as it was copied. A controller paired since that backup isn't in it, and needs pairing again. The pin and your logins come back with it, so your Mac carries on as before.

**Upgrade.** Run the new release's `oikade-host install` on the Pi:

```console
$ curl -fsSL https://github.com/harrymitchinson/oikade/releases/download/<version>/oikade-host-linux-arm64.tar.gz | tar xz oikade-host
$ sudo ./oikade-host install
Upgrading oikade-host v0.3.0 to v0.3.1
Backed up the store to /var/lib/oikade/backups/oikade-20261120T100211Z.db
Starting oikade, and waiting for it to serve
Upgraded to v0.3.1. The previous version is /usr/local/bin/oikade-host.previous
```

It backs up the store through the running host, keeps the old binary as `oikade-host.previous`, and restarts the host on the new one. If the new version doesn't serve within a minute, it puts the old binary and unit back, restarts that, and fails. If the host isn't serving, say because it crashes on start, `install` stops it and copies the store instead. It keeps your unit as it is, unless you give it `--api-addr`, except that a unit from before Oikade kept your values gains `RuntimeDirectory=oikade`, for `/run/oikade`. A host installed by hand upgrades the same way, as long as its unit is `/etc/systemd/system/oikade.service` and runs `/usr/local/bin/oikade-host`.

Matter stops with the host, so the Home app shows no response until it's back, which took 2 s on a Pi 4. Afterwards, `go install` the same version of `cmd/oikade` on your Mac, update `github.com/harrymitchinson/oikade` in your home's `go.mod` and run `oikade deploy`, as your code is built against the old version.

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
| Bridge doesn't appear in the Home app | `dns-sd -B _matterc._udp` from a Mac on the same network. If it's missing: has the 15-minute window passed (`oikade status matter` says, and `oikade pair matter` opens it again)? Is the Pi on the same subnet as the home hub? Does a firewall drop mDNS? |
| "Accessory not found" or pairing fails | `journalctl -u oikade` on the Pi. Is IPv6 on for the Pi and the network? Is there a home hub? |
| A capability is missing | Oikade doesn't map that expose yet, e.g. colour, colour temperature or multi-gang switches (`state_l2`). |
| The Home app shows "No Response" | Does `oikade status` show the device offline? Is Zigbee2MQTT or the broker down? Is the host itself up? |
| A rule never fires | Is it in `oikade status`, and not paused? `oikade tail` shows changes and runs as they happen, and `oikade_rule_runs_total` shows whether it's hitting `condition_false`. Rules use the machine's time zone, so check it if the condition uses the time. |
| A plugin keeps restarting | `journalctl -u oikade` on the Pi has its output and why it stopped. |
| `oikade` on your Mac can't reach the host | Is `--api-addr` in the unit, and the port open? `journalctl -u oikade` says `Serving API over TLS` with the pin, which must match the one you logged in with. |
