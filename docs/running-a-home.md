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

Removing a Safety rule is refused, and so is renaming one, because a rule's claims end with it and a hold's latch lives under its name, which could reopen a valve. `--allow-safety` on either command goes ahead. `--binary FILE` deploys something already built for the host, and skips the checks that need your source.

### Your values

What's yours rather than your home's, such as a login's username, a hub's address or an API key, goes in `.env` beside your `go.mod`, which Git ignores, so the code can be pushed or shared as it stands (OIK-365). Your code reads each value by its name with `oikade.Var`, which returns a string:

```go
home.Plugin("tahoma", overkiz.New(overkiz.Local{
	Address: oikade.Var("TAHOMA_ADDRESS"), PIN: oikade.Var("TAHOMA_PIN"), Token: oikade.Secret("tahoma-token"),
}))
```

```sh
# .env
TAHOMA_ADDRESS=gateway-1234-5678-9012.local:8443
export TAHOMA_PIN="1234-5678-9012"
```

`oikade deploy` sends them to the host with the binary, and says how many went:

```console
$ oikade deploy
Checking your code
Building for linux/arm64
Uploading 11 MB: 11 MB sent
Waiting for the host to check and settle
Deployed version 3f9c2a1, with 2 values from .env
```

A line is `NAME=VALUE`, with `export` before it if you like. Blank lines and lines starting with `#` are skipped, and in a value without quotes, a `#` after a space starts a comment. A value may be in double quotes, where `\n`, `\"` and `\\` are escapes, or in single quotes, which take it as it is, and either may span lines. Nothing is expanded, so a `$` is a `$`. A name is the same in your code, the file and the environment, and follows an environment variable's rule: upper-case letters, digits and underscores, starting with a letter, such as `TAHOMA_PIN`. Your home's check refuses any other name in your code, and `oikade deploy` any other in the file, and both suggest one. The environment wins over the file, as with other dotenv tools, for any name in the file or your code, so `TAHOMA_PIN=… oikade deploy`, or a CI job's `env:`, needs no file. Pick names your shell doesn't set already: `TOSHIBA_USER`, not `USER`. `--env-file FILE` reads another file in place of `.env`, such as a test host's.

A deploy's values replace the host's whole. One your code uses that neither the file nor the environment has stops the deploy before anything is sent:

```console
$ oikade deploy
Checking your code
Error: TAHOMA_PIN isn't set
Hint: add it to .env, or set it in the environment
```

To learn which names your code uses, `oikade deploy` runs your home's check here: it builds your module for this machine too when the host is another kind, and runs `--binary FILE` itself when it can. A binary that can't run here, such as one built for another kind of machine, takes only the file's values, and the host refuses it if its code uses one they lack.

Deploying the running version with values that changed starts your automations and plugins again, with them, and says which changed: `Deployed version 3f9c2a1 again, with TAHOMA_PIN changed`. `oikade rollback` keeps the host's values as they are, so an old password never comes back, and refuses a version whose code uses one they lack, saying which. `oikade rollback --env-file FILE` replaces them with that file's as it goes back.

The host keeps your values as it keeps secrets: never in a binary, a log, an answer or `--json`, which name them only. It keeps them in its store with the version they came with, so a crash during a deploy leaves a version and the values that go with it. Before each process it starts from your binary, it writes them to a file only its user can read, and names it to the process: in its runtime directory, `/run/oikade`, which is in memory, or in the data directory where there's none, as in Docker. Your code reads them as the home is built, so call `oikade.Var` in a plugin's config or a package-level `var`: that declares the name, so a deploy knows of it, and one called in a rule as it runs is too late for that. In `oikade check`, `oikade dev` and `go test`, a `Var` is empty.

### Deploying from CI

The workflow `oikade init` writes, `.github/workflows/deploy.yml`, checks and tests every change on GitHub's runners. Once you turn deploys on, it also deploys what reaches `main`, over the [network API](#the-network-api), from a runner that can reach the host. Until then a push to `main` passes, and leaves a notice that deploys are off, so a home you deploy from your laptop never fails its CI (OIK-286).

GitHub's own runners reach the host only if its API is on the internet, which it shouldn't be, so the deploy job runs on `self-hosted`. Add a runner on your network first, in the repo's Settings → Actions → Runners: a deploy waits 24 hours for one, then GitHub cancels it. To use GitHub's runners instead, change the deploy job's `runs-on` to `ubuntu-latest`, and join the runner to your network over a VPN in a step before the deploy.

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

The deploy job runs `oikade login "$OIKADE_ADDRESS" --pin "$OIKADE_PIN"` with `OIKADE_TOKEN` set. That saves a context with no token in it, and `OIKADE_TOKEN` supplies one whenever that context is used. A context with a token of its own never sends `OIKADE_TOKEN`, so a token meant for one host doesn't reach another. The job keeps its `oikade` and that context in its own temporary directory, so a runner that runs as you leaves your own login alone. One deploy runs at a time, and a push while one waits takes its place, so the last push wins. `oikade remove client/ci` stops the token working.

## Running the host

The host is one long-running process, `oikade-host`, with one data directory, secrets as files, logs on stdout, and metrics and health over HTTP. Take `oikade-host` from a [release](https://github.com/harrymitchinson/oikade/releases), in `oikade-host-linux-<arch>.tar.gz`, and on a machine with systemd it installs itself:

```sh
sudo ./oikade-host install     # --api-addr :8443 and --client laptop, unless you say
```

It copies itself to `/usr/local/bin/oikade-host`, makes the system user `oikade`, which nobody logs in as, writes `/etc/systemd/system/oikade.service`, starts it and waits until the host answers on its socket. Then it prints the `oikade login` line for your laptop, and the firewall rule if ufw is on, which it leaves to you. `--json` prints what it did, for scripts. The unit it writes is this:

```ini
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

The unit says only where the data lives, where the host writes [your values](#your-values) for its processes (`/run/oikade`, in memory), and what to listen on. What's about the home, such as the Matter bridge's name or where Zigbee2MQTT's broker is, is in its code, so rebuilding the machine is this file and `oikade deploy` (OIK-296). Add any other flags for `oikade-host`, such as `--metrics-addr`, to `ExecStart` in a drop-in, with `sudo systemctl edit oikade`. `install` writes the unit itself only on a first install or when it's given `--api-addr`, and never touches a drop-in. `--api-addr` serves the [network API](#the-network-api), which is how you reach the host from another machine. That machine needs only `oikade-host`. If you put `oikade` there too, its commands use the socket in the data directory, which only the host's user can reach, so run them with `sudo`. Your home and its plugins run as that user too, so give it nothing it doesn't need.

When the host stops, it goes in order: the API, ending streams such as `oikade tail`, then your automations, then the writes already under way, then each plugin, which is sent `SIGTERM` and says it's offline before its socket closes, then the Matter bridge and the store. The log has a line for each. `--stop-timeout` (10 seconds) bounds all of it: past that the rest is cut off, the log says where, and the host kills what's left and exits with an error. No state that must survive is lost, because the host only ever crashes safe, though `oikade replay` can miss the last few seconds. `KillMode=mixed` is what leaves the order to the host: by default systemd sends `SIGTERM` to your automations and plugins at the same moment as the host.

### In Docker

The image holds only `oikade-host`, for linux/arm64 and linux/amd64, and runs it. Matter needs the machine's network, for mDNS and IPv6:

```sh
docker run -d --name oikade --restart unless-stopped --stop-timeout 15 --network host \
  -v oikade:/var/lib/oikade ghcr.io/harrymitchinson/oikade:<version> --api-addr :8443
docker exec oikade oikade-host add client/laptop
```

`--stop-timeout 15`, before the image, is Docker's: it gives the host longer than its own 10 seconds to stop before Docker kills it. The flags after the image are `oikade-host`'s. `add` prints the `oikade login` line to run on your laptop, and from then on you use the container from there, over the [network API](#the-network-api). To upgrade, run `oikade backup`, then replace the container on the same volume.

### Secrets

```sh
oikade set secret/api-key         # asks for the value, or reads it from stdin
oikade list secrets               # each with when it changed, and those your home uses that aren't set
oikade remove secret/api-key
```

Values are never in logs or `--json`, and never on a command line, where `ps` and your shell's history would keep them. Each secret is a file in `secrets` in the data directory, readable only by the host's user, so a backup has them.

Your code names a secret with `oikade.Secret`, and passes it where it's used, such as a plugin's config (OIK-297):

```go
var weatherKey = oikade.Secret("weather-api-key") // a step reads weatherKey.Read() when it runs

home.Plugin("cozytouch", overkiz.New(overkiz.Cozytouch{
	Brand: overkiz.Thermor, Username: "someone@example.com", Password: oikade.Secret("cozytouch"),
}))
```

Making one declares it, so `oikade deploy` and the top of `oikade status` name each one a deployed version uses that isn't set, with the command that sets it:

```
$ oikade deploy
Deployed version 14
Secret cozytouch isn't set: oikade set secret/cozytouch
```

A missing secret doesn't stop a deploy: the plugin says what it's waiting for, and reads the secret once it's set. `Read` reads the value each time, so `oikade set secret/NAME` needs no deploy and restarts nothing. Make secrets where the home is built, in a package-level `var` or a plugin's config: one made in a rule as it runs is too late for the host to know of it. A plugin reads the secrets it names itself, such as a device's key, with `plugin.ReadSecret`.

`--secrets-dir DIR` on `oikade-host` points at another directory, which can be read-only, such as a systemd credentials directory. `oikade list secrets` shows what's in it, and `set` says to change it where it's mounted from.

A broker's password is a secret too, passed to the plugin that uses the broker, such as `zigbee2mqtt.Config{Broker: "mqtt://oikade@broker:1883", Password: oikade.Secret("broker")}`. Outside plugins share the host's `--broker`, whose password is `--broker-password-secret broker`. Neither is ever logged.

### Upgrading the host

```sh
sudo ./oikade-host install     # the new release's oikade-host
```

On a machine with an install, `install` upgrades it (OIK-277). It backs up the store through the running host, as `oikade backup` does, keeps the old binary as `/usr/local/bin/oikade-host.previous`, swaps in the new one and restarts the host. If the new version doesn't answer on the socket within a minute, it puts the old binary and unit back, restarts that, and fails. A host that isn't serving, such as one that crashes on start, is stopped and its store copied instead. A unit it didn't write is fine if it's at `/etc/systemd/system/oikade.service` and runs `/usr/local/bin/oikade-host`. Any other unit, it leaves alone and stops.

Unlike a deploy, this restarts the host, so Matter, your automations and your plugins all stop until it's back, and the Home app shows the bridge as not responding until it reconnects. That's a few seconds: on a Raspberry Pi 4 with nine devices, `install` took 2 s, and the home hub had its subscription back 2 s after the old host stopped, without subscribing afresh. The new `oikade-host` opens the same data directory, so nothing needs pairing again. Update `oikade` on the machines you run commands from to the same version, as an older or newer client may not speak this host's API.

A new host may speak a new protocol to your code, in which case your process is turned away until you bump `oikade` in your module and deploy. Do that straight after upgrading.

## Working with a running home

These talk to the host through a Unix socket in its data directory, which only the user running the host can reach. The [host API](host-api.md) is also served over TLS if you ask for it, with a [token](#the-network-api).

```sh
oikade list devices               # what's on record, and how each is set up: rules, plugins, secrets, clients and contexts too
oikade status                     # the running version, a device a line, and your rules
oikade status valve               # one device, why each value is what it is, and its last 10 events
oikade status room/utility plugin/overkiz   # a room's devices, a plugin and its devices
oikade status --check             # exits 1 if something a command fixes is wrong
oikade history valve --since 3d   # what one device did, and who wrote to it
oikade tail                       # changes, writes, refusals and rule runs as they happen
oikade tail valve --since 10m     # one device's last ten minutes, then what it does next
oikade set device/fan/on off      # turn it off
oikade pause case/fan/on/humid    # stop a case, by its ref
oikade resume case/fan/on/humid
oikade pause flow/bedtime         # or a flow
```

`oikade status` opens with the running version and your automations, such as `Home f113018 · automations connected 8 min ago`, or when they went down. Then it shows a device a line, with its room, its values in their units (`setpoint 21.5°C`) and what's wrong with it, such as `went offline 8 min ago`, fitted to the terminal: the room goes first, then values shorten, and `--wide` shows it all. A device your code doesn't declare says `not in your code` once, and a line after the table counts them: `oikade list devices` shows how to declare them. `oikade status DEVICE` shows one device in full, and says why as well as what: an arbitrated capability shows the line that won, such as `case/valve/on/leak-shutoff wants false`, and a limit holding it back, such as `case/pool-pump/on/frost wants true, resting until 03:20`. An offline device says when it went offline, and the view ends with its last 10 events, as `oikade history` prints them. `oikade status` takes any [ref](devices.md#refs) of something that's doing something, several at once, in the order given (OIK-355): a device, a capability (`device/valve/on`, only it), a room (`room/utility`, its devices a line each), a case or a flow (its line, and for a case the capability it decides), a plugin (`plugin/overkiz`: its problem, whether it needs setup, and its devices) and `matter`, the Matter bridge. A ref that names nothing fails the command, with exit 1, and one of another kind, such as `secret/overkiz`, exits 2. Without a ref, `oikade status` starts with what's left to set up, a line each on stderr with the command that fixes it, and says nothing when there's nothing left (OIK-287): no home deployed (`oikade deploy`), a Matter bridge that is down or has no controller (`oikade status matter`), plugins that need their setup run (`oikade setup`), devices with a problem (their STATE says what), and devices whose plugin isn't imported. A ref, and `--json`, leave them out. `--check` prints the same, and exits 1 if something a command fixes is wrong: any of those but a bridge with no controller, as well as a secret that isn't set and automations that are down. A device that's only offline doesn't count, as that's its hardware's and no deploy fixes it, unless you name it, a capability of it or its room: `oikade status --check boiler room/hall` exits 1 if either has a device offline. A device's not being in your code never counts, as nothing is broken. An empty `status` or `list` says what would fill it, on stderr, and `--json` prints an empty list. Colour and glyphs are only for a terminal, and never with `NO_COLOR` set or `TERM=dumb`. An error is a sentence, and often a `Hint:` with the command that fixes it; `--debug`, or `OIKADE_DEBUG=1`, adds its causes in full, and a command given the wrong arguments exits 2, with that command's usage. `oikade help` lists the commands by what you're doing, `oikade help KIND`, such as `oikade help device`, lists the commands that take one, and `oikade COMMAND --help` says what one does, with its flags and an example (OIK-331). A command typed wrong exits 2: an unknown command or flag, a ref that doesn't parse, or a kind the command doesn't take, which it names. A word from another tool, such as `rm` or `logs`, or the name of a command that's gone, such as `matter`, names the command to use. Anything else that fails exits 1, such as a ref that parses but names nothing, a refusal that needs `--allow-safety`, or a host out of reach. A case says whether it applies, why not, and why it isn't sure, by its ref, such as `case/lounge-lamp/on/motion  case  active since 23:07, off at 23:10`, `case/pool-pump/on/frost  case  Safety  inactive: device/pool-water/temperature is 12, not below 3`, or `case/hall-heater/on/occupied  case  inactive  stopped: no device in room hall has reported occupancy`. A time that isn't today has its date, and an on or off time that has gone by says `(passed)`. A case whose conditions can't be read keeps its state until they can. A budget that can't be planned keeps its device off.

`oikade tail` says `Following home's events` on stderr once the host is listening, naming the context, so a quiet house doesn't look hung. Each event is a line: its time, the ref of what it's about, what happened, and why, dimmed, such as `14:02:07  device/desk-plug/on  true  was false` or `14:02:08  device/desk-plug/on  refused a Matter write of true  case/desk-plug/on/hall-too-humid holds it at Safety`. A write says who made it: `wrote true  by case/desk-plug/on/occupied` for a rule, `by matter` for a controller such as the Home app, and `by command` for `oikade set`. A write with no `by` is the host putting a capability back to its default once no rule holds it, and a change with no write before it was made at the device, by its switch or its maker's app.

`oikade history` prints what happened, in those lines, oldest first under a heading for each day (OIK-355). It starts 24 hours back unless `--since` says, and `--until` stops it short. Each takes how long ago (`24h`, `90m`, `3d`), a time of day (`03:00`, the last one), or a date and time (`"2026-11-18 03:00"`), in your machine's time zone. An `--until` time of day that would fall before `--since` is the next one instead, so `--since 10:00 --until 16:00` is one afternoon. The host keeps 14 days by default (`oikade-host --event-log-age` and `--event-log-mib`), and a line on stderr says where the log starts if you ask for more. `oikade history` and `oikade tail` take what to show as [refs](devices.md#refs), as every output prints them, so a ref from one pastes into the other:

| A ref | Shows |
|---|---|
| `device/porch-light`, or `porch-light` | The device: its changes, the writes to it, when it went offline and came back, and when it was forgotten or renamed. It's followed back through the names it had |
| `device/porch-light/on` | One capability, and what happened to the device as a whole |
| `room/hall` | The devices in the room now |
| `case/porch-light/on/dusk`, `flow/morning` | The rule's runs, pauses and outside calls, and the writes it made |
| `plugin/overkiz` | The plugin's own problems, such as a refused login |
| `matter` | The Matter bridge: its pairing window, controllers pairing and leaving, and what controllers wrote. A device called `matter` is `device/matter` |

Several refs show them together. A deploy, and your automations connecting or going down, show whatever you ask for, dimmed, because they're often the reason. A history with nothing in it says so, and whether the host has such a thing on record. `--json` prints a line of JSON an event, the [host API](host-api.md#streams)'s `Event`, and nothing when there's no event of what you asked for. `oikade tail` with `--since` prints that history first, then follows, and says so if a ref names nothing the host has on record, since it would otherwise follow in silence. A command that changes something ends on a line saying what changed, with a green `✓` on a terminal: `Deployed version f113018`, `Paused flow/bedtime`. `oikade version` prints its own version, then the host's when one answers: `oikade-host v0.2.0 on this machine, linux/arm64`.

`oikade set` checks the value before it sends anything, against what the capability takes: `true`, `false`, `on` or `off` for a bool, a number in its range, or one of its names, so `oikade set device/salon/on maybe` says `device/salon/on takes true or false, not maybe` (OIK-331). It takes the capability's [ref](devices.md#refs), and a device and a capability as two words are refused with the ref to use. It acts like tapping the device. A decided capability treats it as an outside change, shown as `oikade set`, so the decision's [override line](automations.md#arbitration) decides how long it lasts. A capability a safety line holds refuses it, rather than writing it back a moment later. It waits up to 3 seconds for the device to answer. A report within half a step of the value is its answer (0.5 for brightness, 0.25 for a setpoint), as a driver rounds brightness 33 to 33.1, but a report of another value on the way, as from a lamp mid-fade, isn't. Then it says what the capability ended up at and why, or that the device hasn't answered yet, which isn't a failure: the write was sent. If something holds it at another value, `set` fails, with exit 1, and its hint says what lets go: `device/towel-rail/on is false, not true: no line applies, so its Default wins` when `NoOverride` wrote it back, or for a rule, `oikade pause flow/bedtime lets go of it until you resume it`. A Safety rule's claim ends only with its release, as a pause keeps it, and a limit holding it back, such as a MinimumOff, lets go when it's done.

A pause is state, like tapping a light: it's in the store, needs no Git and lasts through deploys and restarts until `oikade resume`. A paused rule stops at once: a flow doesn't start, a flow's run in progress ends, and a case stops working out whether it applies and leaves its device as it is. Its claims end, except a safety line's, which end only with a release, so a pause can't reopen a valve. Times that pass during a pause aren't caught up. Pausing a safety line stops that protection, so it needs `--allow-safety`. So does pausing a rule holding a Safety claim, and anything while no process is connected and no deployed version says what it is. A Safety claim made by a flow you paused stays until something releases it, because the run that would have is gone. A rule deleted by a deploy loses its pause. `oikade pause` names the claims it ended, and the Safety ones it kept. It takes the rule's [ref](devices.md#refs), as a claim's `source` in `oikade status --json` gives it, and a rule's name on its own, or a case's three words, are refused with the ref to use.

`--json` on `list`, `status`, `history`, `tail`, `replay`, `set`, `pause`, `resume`, `forget`, `remove`, `pair` and `setup` prints JSON whose fields stay stable for scripts. `list`'s is an array, each thing with its `ref`, and `[]` when there's none. A command that changes something prints `{"ok": true, "ref": …}`, with what it did, such as `set`'s `outcome`, `pause`'s `ended` and `kept`, or `pair`'s `window`. One that fails prints `"ok": false` and its `error`, however it failed, the host out of reach included. `forget` and `remove`, which take several refs, print `refs` instead, what they forgot or removed even if one then failed, and `forget` adds what became of each device as `forgotten`.

From another machine, these and `deploy` and `rollback` go over the [network API](#the-network-api), once you've logged in.

### The network API

`oikade-host --api-addr :8443` also serves the API over TLS, for clients with a token. It's off without the flag, and an address with no port uses 8443. If the port is taken, the host logs that and carries on without it, and a restart tries again. Its first start makes a key and a self-signed certificate, kept in the store, and the log line `Serving API over TLS` gives its `pin`: the SHA-256 of its public key, which a client checks instead of a name or an expiry, so it works by IP address.

To use it from your laptop, on the host's machine run:

```sh
sudo -u oikade oikade-host add client/laptop
```

It prints a line to run on the laptop within 10 minutes:

```sh
oikade login pi.home:8443 --pin sha256:9f2c… --code k7Qx…
```

The pin is in the line, so the laptop trusts nothing it hasn't been told. It checks the pin before it sends the code, and the code works once. The login saves the host as a context in `~/.config/oikade/config.json`, with mode 0600, and makes it current. From then on `oikade status`, `deploy` and the rest go to it. `--context NAME` picks another, and `--data-dir` means the host on this machine. `oikade-host add` always means the host on its own machine, through the socket, so it has no `--context`. A config file others can read is refused.

CI has no one to type a code, so it logs in with a token instead: see [Deploying from CI](#deploying-from-ci).

```sh
oikade list clients              # who has a token, when each last used it, and who's waiting to log in
oikade remove client/laptop      # its token stops working at once
```

`oikade-host add` works only on the host's machine, through the socket, so a stolen token can't make more of them. Without `--token` it adds nothing until `oikade login` uses its code: until then `oikade list clients` shows the client waiting, with when its code runs out, and `oikade remove client/NAME` cancels the code. `remove` works over the network API too, so any client can cut off a lost one, and it ends that client's open streams. It works while the listener is down, so a token can't come back when the port frees. The token is shown once, and the store keeps only its hash. Every token can do everything: a deploy runs code as the host's user, so **a token is as good as a login on the host.** It keeps a stranger on your network out. It isn't for the open internet, so reach the host over your own VPN. The host logs which client made each call.

## Setting up a plugin

A plugin that needs something only you can give it, such as an account's sign-in, has a setup (OIK-276). `oikade status` names a plugin that needs one, as `Plugin NAME needs setup: run oikade setup NAME`. Run that, from any machine that reaches the host:

```sh
oikade setup ewelink-lan
```

It runs on the host, inside the plugin, so what it checks, such as the broker or a device on the LAN, it checks as the plugin sees it. It asks only for what the plugin can't find out, on your terminal, without echoing a secret. If it needs a sign-in, it opens the page in your browser (or `$BROWSER`, or prints it for you to open) and waits up to 5 minutes for the page to send you back to `127.0.0.1` on your machine. What it learns it keeps as the host's [secrets](#secrets), which it names as it goes, and their values never reach your machine. It ends with how it went, on stdout, such as `ewelink-lan is set up: 2 LAN keys kept, 0 already up to date`, or, failing, what's still wrong. Run it again whenever you like: it repeats its checks and changes only what's wrong. `--json` prints `ok`, `plugin`, and `result` or `error`.

A setup that waits 10 minutes for you, or for the plugin, is ended by the host. So is one you stop with Ctrl-C, and starting another setup of the same plugin ends the first. A plugin without one says so, and its README says how to set it up. Each plugin's README says what its setup asks for.

## Removing a plugin

Taking an `oikade.Plugin` out of your home doesn't remove its devices (OIK-225). They stay on record with their names, rooms, claims and Matter endpoints, offline, and the Home app shows them as not responding, so that a plugin missing from one build, or a rollback, loses nothing. `oikade check` and `oikade deploy` list those devices, and call out any a Safety claim holds. `oikade list devices` and `oikade status` mark them `plugin not imported`. Import the plugin again and they come back as they were. To be rid of them, `oikade forget --plugin NAME` forgets all of that plugin's devices at once, as `oikade forget DEVICE` does one, and needs `--allow-safety` if a Safety claim holds any of them.

## State that must survive

Losing these means re-pairing, and then the Home app forgets every room, scene and automation, or every Zigbee device has to be paired again.

- **The host's data directory** (`/var/lib/oikade` by default) is everything a home has to keep, in one file: the Matter bridge's setup code, unique ID, attestation key and fabrics, the device registry with its endpoint IDs, latches, claims, flows, your deployed versions with the values the last deploy sent, your secrets, and the network API's key and token hashes. **Back up one directory.** A file copied while it changes may not open, so either stop the host first, or run `oikade backup` and then copy the directory. `oikade backup` writes a consistent snapshot of the store to `backups/oikade-<time>.db` in the directory while the host runs, and keeps the last five. Secrets and your values are in there in the clear, so keep the copy somewhere as private as the machine. Losing your values costs only a deploy, from the `.env` you have.
  - **Upgrade:** take that snapshot first with `oikade backup`, and a new `oikade-host` opens the same directory, so nothing needs pairing again. A file from a newer format is refused rather than guessed at, so keep the backup until the upgrade has worked.
  - **Delete:** deleting the directory forgets the bridge. Every controller has lost it, so remove it from each one and pair again. The next host gets new codes.
  - **Restore:** with the host stopped, copy the directory back, give it to the host's user, and if it came from a running host, copy the newest `backups/oikade-<time>.db` over `oikade.db`. A host on a restored directory is the same bridge, with no pairing again, because the identity and fabrics come back with it. A controller added since that backup isn't in it, and needs pairing again. If the Matter identity or fabrics are unreadable, the host never makes new ones: it runs the home without a bridge, and the log and `oikade status matter` say why, until you restore them. A restore keeps the network API's pin and every token, including any revoked since that backup, so revoke those again.
- **The device registry** is in the data directory, and holds each device's name, room, device type and Matter endpoint ID. Endpoint IDs are never reused or reordered, and forgotten devices keep theirs. Back up the directory. Without a backup, a deploy gives your devices back the names, rooms and types your home [declares](devices.md#declaring-devices-in-code), and the rest come back under their apps' names, on new endpoints.
- **The Zigbee network key and database are yours,** because they belong to the Zigbee2MQTT you run. They aren't in Oikade's data directory, so back up Zigbee2MQTT's own data directory as well. Oikade's backup doesn't cover it. Losing it means pairing every device again.

## Security notes

- The API is a Unix socket only the host's user can reach. With `--api-addr` it's on the network too, over TLS 1.3, and every call needs a token, which is as good as a login on the host. Keep it off the open internet.
- The metrics listener is read-only, but open to whoever can reach the port, and it shows device names and values. Move it with `--metrics-addr`.
- Importing a plugin means trusting its code, because it runs as the host's user.
- An outside plugin and Zigbee2MQTT are as safe as your broker: anything that can publish to it can control those devices. Give it a password, and keep it in a [secret](#secrets).
- Each plugin's README says what else it needs, such as the [eWeLink LAN plugin](../plugins/ewelinklan/README.md#security).
