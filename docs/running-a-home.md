# Running a home

Deploying a home, running and upgrading the host, and working with it once it runs.

## Deploying

Your home is a Go module, and this replaces the running version from it:

```sh
oikade deploy --data-dir /var/lib/oikade --module .
```

That rejects code that would break a flow's replay, builds your module for the host, and hands it over, with a line on stderr as each part starts. stdout has only the result:

```console
$ oikade deploy --data-dir /var/lib/oikade --module .
Checking your code
Building for linux/arm64
Uploading 11 MB: 11 MB sent
Waiting for the host to check and settle
Deployed version 3f9c2a1
```

A build that takes a while says it's still going every 5 seconds, and on a terminal the upload counts its bytes as it goes. Anywhere else, such as a pipe or CI's log, the lines come whole, with no counter. The host runs its check, then swaps it in with automations stopped for about a second and your device state untouched. Holds are worked out again, and waiting flows carry on from where they were. A flow whose next step changed is dropped and logged. If the new version crashes within the settle period, 5 seconds by default, the last one comes back and the command fails.

```sh
oikade rollback --data-dir /var/lib/oikade
```

returns to the version before the running one. The host keeps the last five.

Removing a Safety rule is refused, and so is renaming one, because a rule's claims end with it and a hold's latch lives under its name, which could reopen a valve. `--allow-safety-removal` on either command goes ahead. `--binary FILE` deploys something already built for the host, and skips the checks that need your source.

## Running the host

The host is one long-running process, `oikade-host`, with one data directory, secrets as files, logs on stdout, and metrics and health over HTTP. Put `oikade-host` from a [release](https://github.com/harrymitchinson/oikade/releases), in `oikade-host-linux-<arch>.tar.gz`, at `/usr/local/bin/oikade-host`, make a user for it that nobody logs in as, and run it under systemd:

```sh
sudo useradd --system --home-dir /var/lib/oikade --shell /usr/sbin/nologin oikade
```

```ini
# /etc/systemd/system/oikade.service
[Unit]
Description=Oikade host
After=network-online.target
Wants=network-online.target

[Service]
User=oikade
ExecStart=/usr/local/bin/oikade-host --data-dir /var/lib/oikade --api-addr :8443
StateDirectory=oikade
StateDirectoryMode=0700
Restart=always
RestartSec=2
KillMode=mixed
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now oikade
```

Add any other flags for `oikade-host`, such as `--broker`, to `ExecStart`. `--api-addr` serves the [network API](#the-network-api), which is how you reach the host from another machine. That machine needs only `oikade-host`. If you put `oikade` there too, its commands use the socket in the data directory, which only the host's user can reach, so run them with `sudo`. Your home and its plugins run as that user too, so give it nothing it doesn't need.

When the host stops, it goes in order: the API, ending streams such as `oikade tail`, then your automations, then the writes already under way, then each plugin, which is sent `SIGTERM` and says it's offline before its socket closes, then the Matter bridge and the store. The log has a line for each. `--stop-timeout` (10 seconds) bounds all of it: past that the rest is cut off, the log says where, and the host kills what's left and exits with an error. No state that must survive is lost, because the host only ever crashes safe, though `oikade replay` can miss the last few seconds. `KillMode=mixed` is what leaves the order to the host: by default systemd sends `SIGTERM` to your automations and plugins at the same moment as the host.

### In Docker

The image holds only `oikade-host`, for linux/arm64 and linux/amd64, and runs it. Matter needs the machine's network, for mDNS and IPv6:

```sh
docker run -d --name oikade --restart unless-stopped --stop-timeout 15 --network host \
  -v oikade:/var/lib/oikade ghcr.io/harrymitchinson/oikade:<version> --api-addr :8443 --broker mqtt://broker:1883
docker exec oikade oikade-host client add laptop
```

`--stop-timeout 15`, before the image, is Docker's: it gives the host longer than its own 10 seconds to stop before Docker kills it. The flags after the image are `oikade-host`'s. `client add` prints the `oikade login` line to run on your laptop, and from then on you use the container from there, over the [network API](#the-network-api). To upgrade, run `oikade backup`, then replace the container on the same volume.

### Secrets

```sh
oikade secret set api-key         # asks for the value, or reads it from stdin
oikade secret list                # names only, with --json too
oikade secret remove api-key
```

Values are never in logs or `--json`, and never on a command line, where `ps` and your shell's history would keep them. Each secret is a file in `secrets` in the data directory, readable only by the host's user, so a backup has them. Your code reads one with `framework.Secret`, and a plugin with `plugin.ReadSecret`, when it next asks. `oikade secret set` restarts nothing, so code that reads a secret once at start sees the new value after your next deploy.

`--secrets-dir DIR` on `oikade-host` points at another directory, which can be read-only, such as a systemd credentials directory. `oikade secret list` shows what's in it, and `set` says to change it where it's mounted from.

A broker's password belongs in a secret, not a flag: use `--broker mqtt://oikade@broker:1883 --broker-password-secret broker`. The host never logs one.

### Upgrading the host

```sh
oikade backup                                                             # a snapshot of the store, in backups/
sudo cp -p /usr/local/bin/oikade-host /usr/local/bin/oikade-host.previous # to go back to
sudo install -m 755 oikade-host /usr/local/bin/oikade-host                # the new version, as a new file
sudo systemctl restart oikade
```

Unlike a deploy, this restarts the host, so Matter, your automations and your plugins all stop until it's back, and the Home app shows the bridge as not responding until it reconnects. The new `oikade-host` opens the same data directory, so nothing needs pairing again. If it doesn't come up, `sudo install -m 755 /usr/local/bin/oikade-host.previous /usr/local/bin/oikade-host` and restart. Update `oikade` on the machines you run commands from to the same version, as an older or newer client may not speak this host's API.

A new host may speak a new protocol to your code, in which case your process is turned away until you bump `oikade` in your module and deploy. Do that straight after upgrading.

## Working with a running home

These talk to the host through a Unix socket in its data directory, which only the user running the host can reach. The [host API](host-api.md) is also served over TLS if you ask for it, with a [token](#the-network-api).

```sh
oikade status                     # the running version, every device, and your rules
oikade status valve               # one device
oikade tail                       # changes, writes, refusals and rule runs as they happen
oikade set fan on off      # turn it off
oikade pause fan                  # stop a rule
oikade resume fan
```

`oikade status` says why as well as what: an arbitrated capability shows its winning claim, such as `Rule/leak-shutoff wants false at Safety`. With your automations down it says since when, and with nothing deployed it says to run `oikade deploy`. An empty `status`, `devices`, `secret list` or `client list` says what would fill it, on stderr, and `--json` prints an empty list. A hold says whether it's active and why it isn't sure, such as `lounge-lamp  hold  active since 23:07, off at 23:10`, or `hall-heater  hold  inactive  stopped: no device in room hall has reported occupancy`. A time that isn't today has its date, and an on or off time that has gone by says `(passed)`. A hold whose `When` can't be read keeps its state until it can. A budget that can't be planned keeps its device off.

`oikade tail` says `Following home's events` on stderr once the host is listening, naming the context, so a quiet house doesn't look hung.

`oikade set` acts like tapping the device. An arbitrated capability treats it as an outside change, shown as `Command`, so its `external` and `overrideHold` settings decide how long it lasts. A capability a Safety claim holds refuses it, rather than writing it back a moment later. It waits up to 3 seconds for the device to answer. A report within 0.5 of the value is its answer, as a driver rounds brightness 33 to 33.1, but a report of another value on the way, as from a lamp mid-fade, isn't. Then it says what the capability ended up at and why, such as `towel-rail on is false, not true: nothing claims it, so its Default wins` when `external: Revert` wrote it back, or that the device hasn't answered yet.

A pause is state, like tapping a light: it's in the store, needs no Git and lasts through deploys and restarts until `oikade resume`. A paused rule stops at once: moments and flows don't start, a flow's run in progress ends, and a hold stops working out its target and leaves its device as it is. Its claims end, except Safety ones, which end only with a release, so a pause can't reopen a valve. Times that pass during a pause aren't caught up. Pausing a Safety rule stops that protection, so it needs `--allow-safety`. So does pausing a rule holding a Safety claim, and anything while no process is connected and no deployed version says what it is. A Safety claim made by a flow you paused stays until something releases it, because the run that would have is gone. A rule deleted by a deploy loses its pause. `oikade pause` names the claims it ended, and the Safety ones it kept.

`--json` on `status`, `tail`, `replay`, `set`, `pause`, `resume`, `devices` and `matter` prints JSON whose fields stay stable for scripts.

From another machine, these and `deploy` and `rollback` go over the [network API](#the-network-api), once you've logged in.

### The network API

`oikade-host --api-addr :8443` also serves the API over TLS, for clients with a token. It's off without the flag, and an address with no port uses 8443. If the port is taken, the host logs that and carries on without it, and a restart tries again. Its first start makes a key and a self-signed certificate, kept in the store, and the log line `Serving API over TLS` gives its `pin`: the SHA-256 of its public key, which a client checks instead of a name or an expiry, so it works by IP address.

To use it from your laptop, on the host's machine run:

```sh
sudo -u oikade oikade-host client add laptop
```

It prints a line to run on the laptop within 10 minutes:

```sh
oikade login pi.home:8443 --pin sha256:9f2c… --code k7Qx…
```

The pin is in the line, so the laptop trusts nothing it hasn't been told. It checks the pin before it sends the code, and the code works once. The login saves the host as a context in `~/.config/oikade/config.json`, with mode 0600, and makes it current. From then on `oikade status`, `deploy` and the rest go to it. `--context NAME` picks another, and `--data-dir` means the host on this machine. `oikade-host client add` always means the host on its own machine, through the socket, so it has no `--context`. A config file others can read is refused.

CI has no one to type a code, so give it a token instead:

```sh
sudo -u oikade oikade-host client add ci --token     # on the host's machine: prints a token and the pin
```

Keep the token in a CI secret as `OIKADE_TOKEN`, then run `oikade login ADDRESS --pin PIN` with it set. That saves a context with no token in it, and `OIKADE_TOKEN` supplies one whenever that context is used. A context with a token of its own never sends `OIKADE_TOKEN`, so a token meant for one host doesn't reach another.

```sh
oikade client list               # who has a token, and when each last used it
oikade client revoke laptop      # its token stops working at once
```

`client add` works only on the host's machine, through the socket, so a stolen token can't make more of them. `revoke` works over the network API too, so any client can cut off a lost one, and it ends that client's open streams. It works while the listener is down, so a token can't come back when the port frees. The token is shown once, and the store keeps only its hash. Every token can do everything: a deploy runs code as the host's user, so **a token is as good as a login on the host.** It keeps a stranger on your network out. It isn't for the open internet, so reach the host over your own VPN. The host logs which client made each call.

## Removing a plugin

Taking a `framework.Plugin` out of your home doesn't remove its devices ([OIK-225](https://linear.app/oikade/issue/OIK-225)). They stay on record with their names, rooms, claims and Matter endpoints, offline, and the Home app shows them as not responding, so that a plugin missing from one build, or a rollback, loses nothing. `oikade check` and `oikade deploy` list those devices, and call out any a Safety claim holds. `oikade devices` and `oikade status` mark them `plugin not imported`. Import the plugin again and they come back as they were. To be rid of them, `oikade devices remove --plugin NAME` removes all of that plugin's devices at once, as `remove` does one, and needs `--allow-safety` if a Safety claim holds any of them.

## State that must survive

Losing these means re-pairing, and then the Home app forgets every room, scene and automation, or every Zigbee device has to be paired again.

- **The host's data directory** (`/var/lib/oikade` by default) is everything a home has to keep, in one file: the Matter bridge's setup code, unique ID, attestation key and fabrics, the device registry with its endpoint IDs, latches, claims, flows, your deployed versions, your secrets, and the network API's key and token hashes. **Back up one directory.** A file copied while it changes may not open, so either stop the host first, or run `oikade backup` and then copy the directory. `oikade backup` writes a consistent snapshot of the store to `backups/oikade-<time>.db` in the directory while the host runs, and keeps the last five. Secrets are in there in the clear, so keep the copy somewhere as private as the machine.
  - **Upgrade:** take that snapshot first with `oikade backup`, and a new `oikade-host` opens the same directory, so nothing needs pairing again. A file from a newer format is refused rather than guessed at, so keep the backup until the upgrade has worked.
  - **Delete:** deleting the directory forgets the bridge. Every controller has lost it, so remove it from each one and pair again. The next host gets new codes.
  - **Restore:** with the host stopped, copy the directory back, give it to the host's user, and if it came from a running host, copy the newest `backups/oikade-<time>.db` over `oikade.db`. A host on a restored directory is the same bridge, with no pairing again, because the identity and fabrics come back with it. A controller added since that backup isn't in it, and needs pairing again. If the Matter identity or fabrics are unreadable, the host never makes new ones: it runs the home without a bridge, and the log and `oikade matter` say why, until you restore them. A restore keeps the network API's pin and every token, including any revoked since that backup, so revoke those again.
- **The device registry** is in the data directory, and holds each device's name, room, device type and Matter endpoint ID. Endpoint IDs are never reused or reordered, and removed devices keep theirs. Back up the directory, or commit `oikade types`' file. Restoring from that file brings back everything it had: endpoints handed out after you last ran `oikade types` aren't in it, so run it after pairing.
- **The Zigbee network key and database are yours,** because they belong to the Zigbee2MQTT you run. They aren't in Oikade's data directory, so back up Zigbee2MQTT's own data directory as well. Oikade's backup doesn't cover it. Losing it means pairing every device again.

## Security notes

- The API is a Unix socket only the host's user can reach. With `--api-addr` it's on the network too, over TLS 1.3, and every call needs a token, which is as good as a login on the host. Keep it off the open internet.
- The metrics listener is read-only, but open to whoever can reach the port, and it shows device names and values. Move it with `--metrics-addr`.
- Importing a plugin means trusting its code, because it runs as the host's user.
- An outside plugin and Zigbee2MQTT are as safe as your broker: anything that can publish to it can control those devices. Give it a password, and keep it in a [secret](#secrets).
- Each plugin's README says what else it needs, such as the [eWeLink LAN plugin](../plugins/ewelinklan/README.md#security).
