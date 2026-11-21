# A host on a Raspberry Pi

From a bare Pi to a home paired with the Home app, step by step. Oikade runs as one process on one machine, so one Pi is all a home needs. [Running a home](running-a-home.md) says more about each step.

## What you need

- **A Raspberry Pi 4 or 5** with a 64-bit OS, booting from an SSD if you can.
- **The same subnet** as your phone and your controller's home hub, with IPv6 on.
- **For Zigbee,** a [Zigbee2MQTT](https://www.zigbee2mqtt.io) and an MQTT broker you run, on this Pi or another.
- **On your laptop,** Go, and `oikade` at the release the Pi runs:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade@<version>
```

## Install the host

The Pi needs only `oikade-host`, from the release's `oikade-host-linux-arm64.tar.gz`, and it installs itself:

```console
$ curl -fsSL https://github.com/harrymitchinson/oikade/releases/download/<version>/oikade-host-linux-arm64.tar.gz | tar xz oikade-host
$ sudo ./oikade-host install
Installing oikade-host v0.3.0 to /usr/local/bin/oikade-host
Created user oikade
Wrote /etc/systemd/system/oikade.service, with the API on :8443
Starting oikade, and waiting for it to serve
Installed oikade-host v0.3.0, running as oikade
On laptop, by 14:32:05, run:
  oikade login pi.home:8443 --pin sha256:9f2c… --code k7Qx…
The code works once. client/laptop is added once it's used, and oikade remove client/laptop cancels it. If the host's name doesn't reach it from there, use its address.
```

The host keeps its data in `/var/lib/oikade`, logs to `journalctl -u oikade`, and serves metrics on `http://pi.home:8080/metrics`. [Under systemd](running-a-home.md#under-systemd) has the unit it writes, and how to add flags.

If the Pi runs a firewall, open Matter's port, mDNS and the API. `install` prints the line if ufw is on, and leaves the firewall to you:

```sh
sudo ufw allow 5540/udp && sudo ufw allow 5353/udp && sudo ufw allow 8443/tcp
```

## Log in from your laptop

Run the `oikade login` line `install` printed, on your laptop, within 10 minutes. From then on, `oikade status` on your laptop answers from the Pi. For another machine, or once the code has expired, run `sudo oikade-host add client/NAME` on the Pi for a new line.

## Deploy your home

Start a home, if you haven't, and deploy it:

```sh
oikade init home && cd home
oikade deploy
```

What your plugins need, such as where Zigbee2MQTT's broker is and its password, goes in your home's code and its [values](running-a-home.md#your-values), which `oikade deploy` brings. Once your devices are on record, `oikade types` writes `devices.go` from them. [Deploying from CI](running-a-home.md#deploying-from-ci) says how to deploy what reaches `main` from GitHub.

## Pair the Home app

```sh
oikade status matter   # the codes, and the QR code to scan
```

In the Home app, choose **Add Accessory** and scan the code, or **More options** and enter the 11-digit one, then choose Add Anyway at the warning that it's uncertified. The window to pair is open for 15 minutes after the host starts, and `oikade pair matter` opens it again. [Pairing](devices.md#pairing) has the rest.

## Back up

Your backup has two parts:

```sh
oikade backup                                                                 # a snapshot of the store, on the Pi
rsync -a --rsync-path="sudo rsync" pi.home:/var/lib/oikade/ pi-backup/oikade/   # everything Oikade has
```

The second part is Zigbee2MQTT's own data directory, wherever you keep it, which holds the network key and the database. It isn't Oikade's, and Oikade's backup doesn't have it. Losing it means pairing every Zigbee device again.

The copy holds your values, what plugins' setups kept, and the bridge's keys, in the clear, so keep it private.

## Restore to a new Pi

Install `oikade-host` as above, and ignore its login line: it's for the empty host it starts, whose identity the restore replaces. Then stop it, and put the copy in its place:

```sh
ssh new-pi sudo systemctl stop oikade
rsync -a --delete --rsync-path="sudo rsync" pi-backup/oikade/ new-pi:/var/lib/oikade/
ssh new-pi "sudo sh -c 'cd /var/lib/oikade && cp \"\$(ls -d backups/* | tail -1)\" oikade.db && chown -R oikade: . && systemctl start oikade'"
```

The `cp` puts the snapshot where the store goes, since the `oikade.db` in the copy was changing as it was copied. The same address and subnet keep the Home app happy, and nothing needs pairing again, though a controller paired since that backup isn't in it. The pin and your logins come back with it, so your laptop carries on as before.

## Upgrade

Run the new release's `install` on the Pi:

```console
$ curl -fsSL https://github.com/harrymitchinson/oikade/releases/download/<version>/oikade-host-linux-arm64.tar.gz | tar xz oikade-host
$ sudo ./oikade-host install
Upgrading oikade-host v0.3.0 to v0.3.1
Backed up the store to /var/lib/oikade/backups/oikade-20261120T100211Z.db
Starting oikade, and waiting for it to serve
Upgraded to v0.3.1. The previous version is /usr/local/bin/oikade-host.previous
```

If the new version doesn't serve within a minute, `install` puts the old one back. Matter stops with the host, so the Home app shows no response until it's back, which took 2 s on a Pi 4. Then, on your laptop, `go install` the same version of `oikade`, bump `github.com/harrymitchinson/oikade` in your home's `go.mod`, and run `oikade deploy`. [Upgrading the host](running-a-home.md#upgrading-the-host) says what else it does.

## Monitor

`oikade status` and `oikade tail` say what your home is doing: see [Working with a running home](working-with-a-home.md). For history, point any Prometheus at `pi.home:8080`, and import the Grafana dashboard, as [Metrics](metrics.md#prometheus-and-grafana) says. Metrics show device names and values, so keep that port on your own network, or move it with `--metrics-addr`.

## Troubleshooting

| Symptom | Check |
|---|---|
| The bridge doesn't appear in the Home app | `dns-sd -B _matterc._udp` from a Mac on the same network. If it's missing: has the 15-minute window passed (`oikade status matter` says, and `oikade pair matter` opens it again)? Is the Pi on the same subnet as the home hub? Does a firewall drop mDNS? |
| "Accessory not found", or pairing fails | `journalctl -u oikade` on the Pi. Is IPv6 on for the Pi and the network? Is there a home hub? |
| A capability is missing | Oikade has no [capability](capabilities.md) for it yet, such as colour, colour temperature, or a switch's second gang. |
| The Home app shows "No Response" | Does `oikade status` show the device offline, or its plugin with a problem? Is Zigbee2MQTT or its broker down? Is the host itself up? |
| A rule never fires | Is it in `oikade status`, and not paused? `oikade tail` shows changes and runs as they happen, and `oikade_rule_runs_total` shows whether it's hitting `condition_false`. Rules use the Pi's time zone, so check it if the condition uses the time. |
| A plugin keeps restarting | `journalctl -u oikade` on the Pi has its output and why it stopped. |
| `oikade` on your laptop can't reach the host | Is `--api-addr` in the unit, and the port open? `journalctl -u oikade` says `Serving API over TLS` with the pin, which must match the one you logged in with. |
