# Deploying Oikade on a Raspberry Pi

Oikade runs as one process on one machine, so one Pi is all a home needs ([OIK-143](https://linear.app/oikade/issue/OIK-143)).

## One Pi, with the host

**What you need:** a Raspberry Pi 4 or 5 with a 64-bit OS, booting from an SSD if you can, on the same subnet as your iPhone and home hub, with IPv6 on. You run [Zigbee2MQTT](https://www.zigbee2mqtt.io) and an MQTT broker yourself, on this Pi or another. Your Mac needs Go, and `oikadectl`:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikadectl@<version>   # the release the Pi runs
```

**Install.** The Pi needs only `oikade-host`, from the release's `oikade-host-linux-arm64.tar.gz`:

```sh
curl -fsSL https://github.com/harrymitchinson/oikade/releases/download/<version>/oikade-host-linux-arm64.tar.gz | tar xz oikade-host
sudo install -m 755 oikade-host /usr/local/bin/oikade-host
sudo useradd --system --home-dir /var/lib/oikade --shell /usr/sbin/nologin oikade
```

Then write `/etc/systemd/system/oikade.service`. `--api-addr` is how your Mac reaches it:

```ini
[Unit]
Description=Oikade host
After=network-online.target
Wants=network-online.target

[Service]
User=oikade
ExecStart=/usr/local/bin/oikade-host --api-addr :8443 --broker mqtt://oikade@broker:1883 --broker-password-secret broker
StateDirectory=oikade
StateDirectoryMode=0700
Restart=always
RestartSec=2
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now oikade
```

If your broker needs no password, leave out the user and `--broker-password-secret`. If it does, the host can't connect until you've logged in from your Mac, below, and set the secret from there:

```sh
oikadectl secret set broker     # on your Mac
ssh pi sudo systemctl restart oikade
```

`journalctl -u oikade -f` on the Pi is its log, and `http://pi:8080/metrics` its metrics.

If that Pi runs a firewall, open Matter's port, mDNS and the API: `sudo ufw allow 5540/udp`, `sudo ufw allow 5353/udp` and `sudo ufw allow 8443/tcp`.

**Log in from your Mac.** On the Pi, run `sudo -u oikade oikade-host client add mac`. It prints a line to run on your Mac within 10 minutes:

```sh
oikadectl login pi.home:8443 --pin sha256:9f2c… --code k7Qx…
```

From then on, `oikadectl status` on your Mac answers from the Pi.

**Your home.** Start a repo and deploy it:

```sh
oikadectl init home && cd home
oikadectl deploy
```

The [README](../README.md#getting-started-in-go) has the rest, including the token its workflow deploys with. `oikadectl matter` shows the code to pair with the Home app.

**Back up.** Your backup has two parts:

```sh
oikadectl backup
rsync -a --rsync-path="sudo rsync" pi:/var/lib/oikade/ pi-backup/oikade/          # everything Oikade has
```

The second part is Zigbee2MQTT's own data directory, wherever you keep it, which holds the network key and the database. It isn't Oikade's, and Oikade's backup doesn't have it. Losing it means pairing every Zigbee device again.

The copy holds your secrets and the bridge's keys in the clear, so keep it private.

**Restore to a new Pi.** Install the binary, the user and the unit as above, but run `sudo systemctl enable oikade` without `--now`, so the host doesn't start on an empty directory. The same address and subnet keep the Home app happy, and nothing needs pairing again:

```sh
ssh new-pi sudo install -d -m 700 /var/lib/oikade
rsync -a --rsync-path="sudo rsync" pi-backup/oikade/ new-pi:/var/lib/oikade/
ssh new-pi "sudo sh -c 'cd /var/lib/oikade && cp \"\$(ls -d backups/* | tail -1)\" oikade.db && chown -R oikade: . && systemctl start oikade'"
```

The `cp` puts the snapshot where the store goes, since the `oikade.db` in the copy was changing as it was copied. A controller paired since that backup isn't in it, and needs pairing again. The pin and your logins come back with it, so your Mac carries on as before.

**Upgrade.**

```sh
oikadectl backup                                                          # on your Mac
sudo cp -p /usr/local/bin/oikade-host /usr/local/bin/oikade-host.previous
sudo install -m 755 oikade-host /usr/local/bin/oikade-host                # from the new release's oikade-host-linux-arm64.tar.gz
sudo systemctl restart oikade
```

Matter stops with the host, so the Home app shows no response until it's back. If the new version doesn't come up, `sudo install -m 755 /usr/local/bin/oikade-host.previous /usr/local/bin/oikade-host` and restart. Afterwards, `go install` the same version of `oikadectl` on your Mac, update `oikade` in your home's `go.mod` and run `oikadectl deploy`, as your code is built against the old version.

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
| Bridge doesn't appear in the Home app | `dns-sd -B _matterc._udp` from a Mac on the same network. If it's missing: has the 15-minute window passed (`oikadectl matter` says, and restarting the host opens it again)? Is the Pi on the same subnet as the home hub? Does a firewall drop mDNS? |
| "Accessory not found" or pairing fails | `journalctl -u oikade` on the Pi. Is IPv6 on for the Pi and the network? Is there a home hub? |
| A capability is missing | Oikade doesn't map that expose yet, e.g. colour, colour temperature or multi-gang switches (`state_l2`). |
| The Home app shows "No Response" | Does `oikadectl status` show the device offline? Is Zigbee2MQTT or the broker down? Is the host itself up? |
| A rule never fires | Is it in `oikadectl status`, and not paused? `oikadectl tail` shows changes and runs as they happen, and `oikade_rule_runs_total` shows whether it's hitting `condition_false`. Rules use the machine's time zone, so check it if the condition uses the time. |
| A plugin keeps restarting | `journalctl -u oikade` on the Pi has its output and why it stopped. |
| `oikadectl` on your Mac can't reach the host | Is `--api-addr` in the unit, and the port open? `journalctl -u oikade` says `Serving API over TLS` with the pin, which must match the one you logged in with. |
