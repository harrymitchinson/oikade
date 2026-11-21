# Running a home

How to install the host, deploy your home to it, give it your values, deploy from CI, set up plugins, upgrade, and back up. [A host on a Raspberry Pi](deploy-raspberry-pi.md) walks through the same steps on one machine, and [Working with a running home](working-with-a-home.md) has the commands you use once it's running.

## Installing the host

The host is one long-running process, `oikade-host`, with one data directory. It logs to stdout, and serves metrics and health over HTTP. Take it from a [release](https://github.com/harrymitchinson/oikade/releases), in `oikade-host-linux-<arch>.tar.gz`.

### Under systemd

On a machine with systemd, it installs itself:

```sh
sudo ./oikade-host install     # --api-addr :8443 and --client laptop, unless you say
```

1. It copies itself to `/usr/local/bin/oikade-host`.
2. It makes the system user `oikade`, which nobody logs in as.
3. It writes `/etc/systemd/system/oikade.service`, starts it, and waits until the host answers on its socket.
4. It prints the `oikade login` line for your laptop, and, if ufw is on, the firewall rule, which it leaves to you.

`--json` prints what it did, for scripts. The unit it writes is this:

```ini
# Written by oikade-host install, which writes it again when it's given flags.
# Put changes of your own in a drop-in, with systemctl edit oikade, which it never touches.
[Unit]
Description=Oikade host
After=network-online.target
Wants=network-online.target

[Service]
User=oikade
ExecStart=/usr/local/bin/oikade-host --api-addr :8443
StateDirectory=oikade
StateDirectoryMode=0700
RuntimeDirectory=oikade
RuntimeDirectoryMode=0700
Restart=always
RestartSec=2
KillMode=mixed
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

The unit says only where the data lives (`/var/lib/oikade`), where the host writes [your values](#your-values) for its processes (`/run/oikade`, which is in memory), and what to listen on. What's about the home, such as the Matter bridge's name or where a plugin's broker is, is in its code. So rebuilding the machine is this file and `oikade deploy`.

- **Other flags,** such as `--metrics-addr`, go in `ExecStart` in a drop-in, with `sudo systemctl edit oikade`. `install` writes the unit itself only on a first install or when it's given `--api-addr`, and never touches a drop-in.
- **`--api-addr`** serves the [network API](#the-network-api), which is how you reach the host from another machine. The host's machine needs only `oikade-host`.
- **`oikade` on the host's machine** uses the socket in the data directory, which only the host's user can reach, so run its commands with `sudo`.
- **Your home and its plugins run as the host's user,** so give that user nothing it doesn't need.

### In Docker

The image holds only `oikade-host`, for linux/arm64 and linux/amd64, and runs it. Matter needs the machine's network, for mDNS and IPv6:

```sh
docker run -d --name oikade --restart unless-stopped --stop-timeout 15 --network host \
  -v oikade:/var/lib/oikade ghcr.io/harrymitchinson/oikade:<version> --api-addr :8443
docker exec oikade oikade-host add client/laptop
```

`--stop-timeout 15`, before the image, is Docker's: it gives the host longer than its own 10 seconds to stop before Docker kills it. The flags after the image are `oikade-host`'s. `add` prints the `oikade login` line to run on your laptop, and from then on you use the container from there, over the [network API](#the-network-api). With no runtime directory in a container, the host writes your values for its processes in the data directory. To upgrade, run `oikade backup`, then replace the container on the same volume.

### The host's flags

`oikade-host --help` lists them all:

| Flag | Default | |
|---|---|---|
| `--data-dir DIR` | `/var/lib/oikade` | The directory with all the host's state |
| `--api-addr ADDRESS` | none | Where to serve the [network API](#the-network-api) over TLS, such as `:8443`. Without it, the API is only on the socket |
| `--metrics-addr ADDRESS` | `:8080` | Where to serve [metrics and health](metrics.md). Empty serves neither |
| `--matter-port PORT` | `5540` | The [Matter bridge](devices.md#its-network)'s UDP port. 0 turns Matter off |
| `--matter-interfaces LIST` | every multicast interface | The interfaces to advertise Matter on, comma-separated |
| `--settle DURATION` | `5s` | How long a new version must stay up before a deploy counts |
| `--heartbeat-timeout DURATION` | `10s` | How long your process may go without answering |
| `--plugin-set-timeout DURATION` | `30s` | How long a plugin's write may run before the plugin counts as hung |
| `--stop-timeout DURATION` | `10s` | How long [stopping](#stopping) may take. Keep it under systemd's `TimeoutStopSec` |
| `--event-log-age DURATION` | `336h` | How long the [event log](working-with-a-home.md#how-far-back) keeps events |
| `--event-log-mib MIB` | `64` | The most the event log holds, in MiB |
| `--home BINARY` | none | Your home's binary, until one is deployed |
| `--broker URL`, `--outside-plugins LIST`, `--broker-password-secret NAME` | none | For [outside plugins](plugin-contract.md#an-outside-process), which speak the contract over a broker of yours |
| `--simulate FILE` | none | A JSON file of simulated devices, for testing |

### Stopping

When the host stops, it goes in order, and the log has a line for each:

1. The API, ending streams such as `oikade tail`.
2. Your automations.
3. The writes already under way.
4. Each plugin, which is sent `SIGTERM` and says it's offline before its socket closes.
5. The Matter bridge, and the store.

`--stop-timeout` bounds all of it. Past that the rest is cut off, the log says where, and the host kills what's left and exits with an error. No state that must survive is lost, because the host only ever crashes safe, though [`oikade replay`](testing.md#replaying-last-week) can miss the last few seconds.

`KillMode=mixed` in the unit is what leaves the order to the host: without it, systemd sends `SIGTERM` to your automations and plugins at the same moment as the host.

## The network API

`oikade-host --api-addr :8443` serves the API over TLS, for clients with a token, beside the socket. It's off without the flag, and an address with no port uses 8443. If the port is taken, the host logs that and carries on without it, and a restart tries again.

Its first start makes a key and a self-signed certificate, kept in the store. The log line `Serving API over TLS` gives its `pin`: the SHA-256 of its public key, which a client checks instead of a name or an expiry, so it works by IP address.

### Logging in from your laptop

On the host's machine, run:

```sh
sudo -u oikade oikade-host add client/laptop
```

It prints a line to run on the laptop within 10 minutes:

```console
On laptop, by 14:32:05, run:
  oikade login pi.home:8443 --pin sha256:9f2c… --code k7Qx…
The code works once. client/laptop is added once it's used, and oikade remove client/laptop cancels it. If the host's name doesn't reach it from there, use its address.
```

The pin is in the line, so the laptop trusts nothing it hasn't been told. It checks the pin before it sends the code, and the code works once.

The login saves the host as a context in `~/.config/oikade/config.json`, with mode 0600, and makes it current. From then on `oikade status`, `deploy` and the rest go to it. `--context NAME` picks another, and `--data-dir` means the host on this machine. A config file others can read is refused.

### Clients and tokens

```sh
oikade list clients              # who has a token, when each last used it, and who's waiting to log in
oikade remove client/laptop      # its token stops working at once
```

- **`oikade-host add` works only on the host's machine,** through the socket, so a stolen token can't make more clients. It has no `--context`.
- **Without `--token`, it adds nothing** until `oikade login` uses its code. Until then `oikade list clients` shows the client waiting, with when its code runs out, and `oikade remove client/NAME` cancels the code.
- **`remove` works over the network API too,** so any client can cut off a lost one, and it ends that client's open streams. It works while the listener is down, so a token can't come back when the port frees.
- **A token is shown once,** and the store keeps only its hash. The host logs which client made each call.
- **Every token can do everything.** A deploy runs code as the host's user, so **a token is as good as a login on the host.** It keeps a stranger on your network out, but it isn't for the open internet, so reach the host over your own VPN.

CI has no one to type a code, so it logs in with a token instead: see [Deploying from CI](#deploying-from-ci).

## Deploying

Your home is a Go module, and `oikade deploy` replaces the running version with it:

```console
$ oikade deploy
Checking your code
Building for linux/arm64
Uploading 11 MB: 11 MB sent
Waiting for the host to check and settle
Deployed version 3f9c2a1, with 3 values from .env
```

It checks your code as [`oikade check`](testing.md#checking) does, rejecting code that would break a flow's replay, builds your module for the host, and hands it over. Each part's line is on stderr as it starts, and stdout has only the result.

- **The swap.** The host runs your home's check, then swaps it in with automations stopped for about a second and your device state untouched. Holds are worked out again, and waiting flows carry on from where they were. A flow whose next step changed is dropped and logged.
- **The settle.** If the new version crashes within the settle period, 5 seconds by default, the last one comes back and the command fails.
- **Progress.** A build or check that takes a while says it's still going every 5 seconds, and on a terminal the upload counts its bytes as it goes. Anywhere else, such as a pipe or CI's log, the lines come whole, with no counter.
- **What else changed** comes on stderr: devices it [renamed or moved](devices.md#what-a-deploy-does-with-them), devices it took off Matter, and devices whose plugin it no longer imports.
- **`--module DIR`** deploys another module, and `--binary FILE` something already built for the host, skipping the checks that need your source.
- **Deploying the running version again** with the same values says `Already running version 3f9c2a1`.

### Rolling back

`oikade rollback` returns to the version before the running one. The host keeps the last five, the running one included. It keeps the host's values as they are: see [Changing values](#changing-values).

### Safety rules

A deploy or a rollback that would end a Safety rule's claims is refused, and lists them. That includes removing a [safety line](automations.md#safety-lines) and renaming one, because a rule's claims end with it and a latch lives under its name, which could reopen a valve. `--allow-safety` on either command goes ahead.

## Your values

What's yours rather than your home's, such as a login's username and password, a hub's address or an API key, goes in `.env` beside your `go.mod`. The `.gitignore` that `oikade init` writes keeps it out of Git, so the code can be pushed or shared as it stands. Your code reads each value by its name with `oikade.Var`, which returns a string:

```go
home.Plugin("tahoma", overkiz.New(overkiz.Local{
	Address: oikade.Var("TAHOMA_ADDRESS"), PIN: oikade.Var("TAHOMA_PIN"), Token: oikade.Var("TAHOMA_TOKEN"),
}))
```

```sh
# .env
TAHOMA_ADDRESS=gateway-1234-5678-9012.local:8443
export TAHOMA_PIN="1234-5678-9012"
# A token or a password is a value like any other
TAHOMA_TOKEN=c1f7e2a94b
```

Call `oikade.Var` where the home is built, such as in a plugin's config or a package-level `var`. That declares the name, so a deploy knows of it. One called in a rule as it runs is too late for that.

### The file

- **A line is `NAME=VALUE`,** with `export` before it if you like. Blank lines and lines starting with `#` are skipped, and in a value without quotes, a `#` after a space starts a comment.
- **A value may be quoted.** In double quotes, `\n`, `\"` and `\\` are escapes. Single quotes take it as it is. Either may span lines. Nothing is expanded, so a `$` is a `$`.
- **A name** is the same in your code, the file and the environment, and follows an environment variable's rule: upper-case letters, digits and underscores, starting with a letter, such as `TAHOMA_PIN`. Your home's check refuses any other name in your code, and `oikade deploy` any other in the file, and both suggest one.
- **The environment wins** over the file, as with other dotenv tools, for any name in the file or your code. So `TAHOMA_PIN=… oikade deploy`, or a CI job's `env:`, needs no file. Pick names your shell doesn't set already: `TOSHIBA_USER`, not `USER`.
- **`--env-file FILE`** reads another file in place of `.env`, such as a test host's.

### What a deploy sends

`oikade deploy` sends your values to the host with the binary, and says how many went: `Deployed version 3f9c2a1, with 3 values from .env`. A deploy's values replace the host's whole.

To learn which names your code uses, `oikade deploy` runs your home's check on this machine. It builds your module for this machine too when the host is another kind, and runs a `--binary FILE` itself when it can. A binary that can't run here, such as one built for another kind of machine, takes only the file's values, and the host refuses it if its code uses one they lack.

A value your code uses that neither the file nor the environment has stops the deploy before anything is sent:

```console
$ oikade deploy
Checking your code
Error: TAHOMA_PIN isn't set
Hint: add it to .env or the environment, or pass --keep-vars to use the host's, if it has it
```

### Deploying without them

From somewhere without your values, such as CI, another machine or a clean clone, `oikade deploy --keep-vars` sends none, and the host keeps its own. It doesn't read `.env` or the environment, so it can't go with `--env-file`. The host refuses a version whose code uses a value it lacks, naming it:

```console
$ oikade deploy --keep-vars
Checking your code
Building for linux/arm64
Uploading 11 MB: 11 MB sent
Waiting for the host to check and settle
Error: the version uses TAHOMA_PIN, which isn't among the host's values
Hint: add it to your .env, and deploy without --keep-vars, which sends your values
```

### Changing values

- **Deploy again.** Deploying the running version with values that changed starts your automations and plugins again with them, and says which changed: `Deployed version 3f9c2a1 again, with TAHOMA_PIN changed`.
- **A rollback keeps the host's values** as they are, so an old password never comes back, and refuses a version whose code uses one they lack, saying which. `oikade rollback --env-file FILE` replaces them with that file's as it goes back.

### How the host keeps them

The host keeps your values as it keeps [secrets](#secrets): never in a binary, a log, an answer or `--json`, which name them only. It keeps them in its store with the version they came with, so a crash during a deploy leaves a version and the values that go with it.

Before each process it starts from your binary, it writes them to a file only its user can read, and names it to the process. That's in its runtime directory, `/run/oikade`, which is in memory, or in the data directory where there's none, as in Docker. The host's check of a version has the values it would run with, the deploy's.

### In check, dev and tests

`oikade check` and `oikade dev` give your code your values too, from `.env` and the environment, or the file `--env-file` names, so what your code builds from them, such as a plugin's config, is as it will be on the host. `oikade types` gives it `.env`'s. A value your code uses that nothing gives is empty there, and only a warning, so a pull request passes without `.env`:

```console
$ oikade check
Warning: TAHOMA_PIN isn't set, so a deploy from here would stop
Hint: add it to .env, or set it in the environment
Your home is fine: 12 rules, 2 plugins
```

Under `go test` and `oikadetest`, an `oikade.Var` is empty.

## Deploying from CI

The workflow `oikade init` writes, `.github/workflows/deploy.yml`, checks and tests every change on GitHub's runners. Once you turn deploys on, it also deploys what reaches `main`, over the [network API](#the-network-api), from a runner that can reach the host. Until then a push to `main` passes, and leaves a notice that deploys are off, so a home you deploy from your laptop never fails its CI.

### Your values in CI

Both jobs write your values from the secret `OIKADE_ENV` to a file in the job's temporary directory, rather than the checkout, which a self-hosted runner keeps. `oikade check` and `oikade deploy` read it with `--env-file`. Set it to your `.env` whole:

```sh
gh secret set OIKADE_ENV < .env
```

To keep your values off GitHub instead, change the deploy job's `--env-file` to `--keep-vars`, so it deploys with the values your last deploy from your laptop sent. A pull request from a fork gets no secrets, so its check warns of the values it lacks and passes.

### A runner that reaches the host

GitHub's own runners reach the host only if its API is on the internet, which it shouldn't be, so the deploy job runs on `self-hosted`. Add a runner on your network first, in the repo's Settings → Actions → Runners: a deploy waits 24 hours for one, then GitHub cancels it. To use GitHub's runners instead, change the deploy job's `runs-on` to `ubuntu-latest`, and join the runner to your network over a VPN in a step before the deploy.

### Turning deploys on

CI has no one to type a code, so give it a token. On the host's machine:

```sh
sudo -u oikade oikade-host add client/ci --token     # prints a token and the pin
```

Then, in your home's repo, with [`gh`](https://cli.github.com) signed in, or in the repo's Settings → Secrets and variables → Actions:

```sh
gh secret set OIKADE_TOKEN                           # paste the token
gh variable set OIKADE_PIN --body sha256:9f2c…
gh variable set OIKADE_ADDRESS --body pi.home:8443   # last, because this turns deploys on
```

The deploy job runs `oikade login "$OIKADE_ADDRESS" --pin "$OIKADE_PIN"` with `OIKADE_TOKEN` set. That saves a context with no token in it, and `OIKADE_TOKEN` supplies one whenever that context is used.

- **A context with a token of its own never sends `OIKADE_TOKEN`,** so a token meant for one host doesn't reach another.
- **The job keeps its `oikade` and that context** in its own temporary directory, so a runner that runs as you leaves your own login alone.
- **One deploy runs at a time,** and a push while one waits takes its place, so the last push wins.
- **`oikade remove client/ci`** stops the token working.

## Setting up a plugin

A plugin that needs something only you can give it, such as an account's sign-in, has a setup. `oikade status` names a plugin that needs one, as `Plugin NAME needs setup: run oikade setup NAME`. Run that, from any machine that reaches the host:

```sh
oikade setup ewelink-lan
```

- **It runs on the host, inside the plugin,** so what it checks, such as a device on the LAN, it checks as the plugin sees it.
- **It asks only for what the plugin can't find out,** on your terminal, without echoing a secret.
- **A sign-in opens in your browser,** or `$BROWSER`, or it prints the page for you to open. It waits up to 5 minutes for the page to send you back to `127.0.0.1` on your machine.
- **What it learns, it keeps** as the host's [secrets](#secrets), which it names as it goes, and their values never reach your machine.
- **It ends with how it went,** on stdout, such as `ewelink-lan is set up: 2 LAN keys kept, 0 already up to date`, or, failing, what's still wrong. `--json` prints `ok`, `plugin`, and `result` or `error`.

Run it again whenever you like: it repeats its checks and changes only what's wrong. A setup that waits 10 minutes for you, or for the plugin, is ended by the host. So is one you stop with Ctrl-C, and starting another setup of the same plugin ends the first. A plugin without one says so. Each plugin's README says what its setup asks for.

## Secrets

What a plugin's setup learns on the host, such as eWeLink's device keys, it keeps there as secrets: a file each in `secrets` in the data directory, readable only by the host's user, so a backup has them. No command lists them, a deploy keeps them, and running the setup again changes them. They're never in a log or `--json`. What you'd type in yourself, a password included, is one of [your values](#your-values) instead.

A broker's password is never logged either:

- **A broker a plugin your home imports uses,** such as Zigbee2MQTT's, takes its password as one of your values: `zigbee2mqtt.Config{Broker: "mqtt://oikade@broker:1883", Password: oikade.Var("BROKER_PASSWORD")}`.
- **Outside plugins share the host's `--broker`,** whose password is a file you put in `secrets` yourself, readable only by the host's user. `--broker-password-secret broker` reads `secrets/broker`, and the host doesn't start without it.

## Removing a plugin

Taking an `oikade.Plugin` out of your home doesn't remove its devices. They stay on record with their names, rooms, claims and Matter endpoints, offline, and the Home app shows them as not responding. So a plugin missing from one build, or a rollback, loses nothing, and importing the plugin again brings them back as they were.

- **`oikade check` and `oikade deploy` list those devices,** and call out any a Safety claim holds.
- **`oikade list devices` and `oikade status` mark them** `plugin not imported`.
- **To be rid of them,** `oikade forget --plugin NAME` forgets all of that plugin's devices at once, as `oikade forget DEVICE` does one, and needs `--allow-safety` if a Safety claim holds any of them.

## Upgrading the host

Run the new release's `install`:

```sh
sudo ./oikade-host install     # the new release's oikade-host
```

On a machine with an install, `install` upgrades it:

1. It backs up the store through the running host, as `oikade backup` does. A host that isn't serving, such as one that crashes on start, is stopped and its store copied instead.
2. It keeps the old binary as `/usr/local/bin/oikade-host.previous`, and swaps in the new one.
3. It restarts the host. If the new version doesn't answer on the socket within a minute, it puts the old binary and unit back, restarts that, and fails.

It keeps your unit as it is, unless you give it `--api-addr`, and adds `RuntimeDirectory=oikade` to one that lacks it. A unit it didn't write is fine if it's at `/etc/systemd/system/oikade.service` and runs `/usr/local/bin/oikade-host`. Any other unit it leaves alone, and stops.

### What an upgrade stops

Unlike a deploy, an upgrade restarts the host, so Matter, your automations and your plugins all stop until it's back. The Home app shows the bridge as not responding until it reconnects. That's a few seconds: on a Raspberry Pi 4 with nine devices, `install` took 2 s, and the home hub had its subscription back 2 s after the old host stopped, without subscribing afresh. The new `oikade-host` opens the same data directory, so nothing needs pairing again.

### After an upgrade

- **Update `oikade`** on the machines you run commands from to the same version, as an older or newer client may not speak this host's API.
- **Bump `oikade.dev`** in your home's `go.mod`, and deploy. A new host may speak a new protocol to your code, and then your process is turned away until you do.

## State that must survive

Losing the host's data means pairing again: every controller forgets the bridge, and with it every room, scene and automation it had.

**The host's data directory,** `/var/lib/oikade` by default, holds everything a home has to keep, in one file:

- the Matter bridge's setup code, unique ID, attestation key and fabrics
- the devices on record, with their names, rooms, types and endpoint IDs
- latches, claims and flows
- your deployed versions, with the values the last deploy sent
- what plugins' setups kept
- the network API's key, and its tokens' hashes

**Zigbee's network key and database are yours,** because they belong to the Zigbee2MQTT you run. Back up Zigbee2MQTT's own data directory as well: Oikade's backup doesn't cover it, and losing it means pairing every Zigbee device again.

### Backing up

Back up one directory. A file copied while it changes may not open, so either stop the host first, or run `oikade backup` and then copy the directory. `oikade backup` writes a consistent snapshot of the store to `backups/oikade-<time>.db` in the data directory while the host runs, prints where, and keeps the last five.

Secrets and your values are in the copy in the clear, so keep it somewhere as private as the machine. Losing your values costs only a deploy, from the `.env` you have.

Take a snapshot before an [upgrade](#upgrading-the-host) too, which `install` does for you. A store from a newer format is refused rather than guessed at, so keep the backup until the upgrade has worked.

### Restoring

With the host stopped, copy the directory back and give it to the host's user. If the copy came from a running host, copy the newest `backups/oikade-<time>.db` over `oikade.db`.

- **A host on a restored directory is the same bridge,** with no pairing again, because the identity and fabrics come back with it. A controller added since that backup isn't in it, and needs pairing again.
- **A restore keeps the network API's pin and every token,** including any revoked since that backup, so revoke those again.
- **If the Matter identity or fabrics are unreadable,** the host never makes new ones. It runs the home without a bridge, and the log and `oikade status matter` say why, until you restore them.

### Starting again

Deleting the directory forgets the bridge. Every controller has lost it, so remove it from each one and pair again, and the next host gets new codes. Your next deploy gives your devices back the names, rooms and types your home [declares](devices.md#declaring-devices-in-code) as they report, and the rest come back under their apps' names, on new endpoints.

## Security notes

- **The API** is a Unix socket only the host's user can reach. With `--api-addr` it's on the network too, over TLS 1.3, and every call needs a token, which is as good as a login on the host. Keep it off the open internet.
- **The metrics listener** is read-only, but open to whoever can reach the port, and it shows device names and values. Move it with `--metrics-addr`.
- **Importing a plugin means trusting its code,** because it runs as the host's user.
- **An outside plugin and Zigbee2MQTT are as safe as your broker:** anything that can publish to it can control those devices. Give it a password: Zigbee2MQTT's is one of [your values](#your-values), and outside plugins' is a file in the host's [secrets](#secrets).
- **Each plugin's README** says what else it needs, such as the [eWeLink LAN plugin](../plugins/ewelinklan/README.md#security).
