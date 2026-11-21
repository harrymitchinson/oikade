# Working with a running home

The commands that ask your host what your home is doing and why, and the ones that drive it: `list`, `status`, `history`, `tail`, `set`, `pause` and `resume`. Installing the host and deploying to it are in [Running a home](running-a-home.md).

## Reaching the host

On the host's machine, these talk to the host through a Unix socket in its data directory, which only the host's user can reach, so run them with `sudo`. From any other machine, they go over the [network API](running-a-home.md#the-network-api) once you've logged in.

- **`--context NAME`** reaches a host you logged in to, and **`--data-dir DIR`** the host on this machine.
- **Without either,** a command uses the context `oikade login` last saved, or the host on this machine.
- **`oikade version`** prints its own version, then the host's when one answers: `oikade-host v0.2.0 on this machine, linux/arm64`.

## At a glance

```sh
oikade list devices                         # what's on record, and how each is set up: rules, plugins, clients and contexts too
oikade status                               # the running version, a device a line, and your rules
oikade status bathroom-fan                  # one device, why each value is what it is, and its last 10 events
oikade status room/utility plugin/overkiz   # a room's devices, a plugin and its devices
oikade status --check                       # exits 1 if something a command fixes is wrong
oikade history bathroom-fan --since 3d      # what one device did, and who wrote to it
oikade tail                                 # changes, writes, refusals and rule runs as they happen
oikade tail bathroom-fan --since 10m        # one device's last ten minutes, then what it does next
oikade set device/bathroom-fan/on off       # turn it off
oikade pause case/bathroom-fan/on/humid     # stop a case, by its ref
oikade resume case/bathroom-fan/on/humid
oikade pause flow/bedtime                   # or a flow
```

What's on record, `oikade list devices`, is in [Devices](devices.md#listing-devices).

## Status

`oikade status` says what your home is doing now, and what's wrong:

```console
$ oikade status
Plugin overkiz: login refused: check overkiz.Local's Token
Devices with a problem: porch-light. Their STATE says what's wrong.
Home f113018 · automations connected 8 min ago

NAME         ROOM   VALUES                                                    STATE
hall-lamp    hall   on false
office-ac           mode cool, on true, setpoint 21.5°C, temperature 24.25°C  not in your code
porch-light                                                                   no LAN key: oikade setup ewelink-lan
study-blind  study  position 40%                                              went offline today 12:25
office-ac isn't in your code: oikade list devices shows how to declare it
Hint: oikade status office-ac says why each of its values is what it is

case/office-ac/on/too-warm  case  active since 13:52
flow/morning  flow
```

- **What's left to set up** comes first, on stderr. See [below](#whats-left-to-set-up).
- **Then the running version** and your automations, or when they went down.
- **Each device** is a line, with its room, its values in their units, and what's wrong with it, such as `went offline 8 min ago`. The table fits the terminal: the room goes first, then values shorten, and `--wide` shows it all.
- **A device your code doesn't declare** says `not in your code`, and a line after the table says how to declare it.
- **Your rules** follow, a line each, with what each is doing. See [Rules](#rules).

### What's left to set up

Without a ref, `oikade status` starts with what's left to set up, a line each on stderr with the command that fixes it, and says nothing when there's nothing left:

| Line | Fixed by |
|---|---|
| `No home deployed.` | `oikade deploy` |
| `The Matter bridge is down` or `has no controller` | `oikade status matter` says why, or how to pair one |
| `Plugin NAME needs setup` | [`oikade setup NAME`](running-a-home.md#setting-up-a-plugin) |
| `Plugin NAME: …` | What the plugin says is wrong, such as a refused login |
| `Devices with a problem` | Their STATE says what's wrong |
| `Devices whose plugin isn't imported` | Import it again, or [forget them](running-a-home.md#removing-a-plugin) |

A ref, and `--json`, leave them out.

### One thing in full

`oikade status` takes [refs](devices.md#refs) of anything that's doing something, several at once, shown in the order given:

| Ref | Shows |
|---|---|
| `device/office-ac`, or `office-ac` | The device in full: [why each value is what it is](#why-a-value-is-what-it-is), and its last 10 events. An offline device says when it went offline |
| `device/office-ac/on` | Only that capability |
| `room/utility` | The room's devices, a line each |
| `case/office-ac/on/too-warm`, `flow/morning` | The rule's line, and for a case the capability it decides |
| `plugin/overkiz` | The plugin's problem, whether it needs setup, and its devices |
| `matter` | The Matter bridge: whether it's serving, its pairing codes, and its controllers |

A ref that names nothing fails the command, with exit 1, and one of a kind `status` doesn't take, such as `client/laptop`, exits 2.

### Why a value is what it is

`oikade status DEVICE` shows each capability, and the line of its [decision](automations.md#decisions) that won it:

```console
$ oikade status office-ac
office-ac: not in your code

CAPABILITY   VALUE    WHY
mode         cool
on           true     case/office-ac/on/too-warm wants true, on for its MinimumOn until 14:04
setpoint     21.5°C
temperature  24.25°C

Tue 17 Nov, yesterday
12:00:00  device/office-ac  came back online
Wed 18 Nov, today
13:52:00  device/office-ac/on  wrote true  by case/office-ac/on/too-warm
13:52:01  device/office-ac/on  true  was false
13:59:00  device/office-ac/temperature  24.25°C
```

- **WHY** names the line that won, such as `case/mains-valve/on/leak-shutoff wants false`, and a [limit](automations.md#limits) holding it back, such as `resting until 03:20`. An outside change is `oikade set`, `a change from Matter` or `a change on the device`, and `Default` is the decision's last line.
- **`--json`** names the `case` that won, and lists every claim, with its `source` (a [ref](devices.md#refs)), its `level`, `expires` when it ends at a set time, and `latched` for a latched case.
- **Over time,** the metric `oikade_device_control_level` has the winning level: `Safety` for a safety line, `Override` for an outside change, `Normal` for the rest, and `Default`. See [Metrics](metrics.md).

### Rules

A rule's line says whether it applies, why not, and why it isn't sure:

```text
case/lounge-lamp/on/motion  case  active since 23:07, off at 23:10
case/greenhouse-heater/on/frost  case  Safety  inactive: device/greenhouse/temperature is 12, not below 3
case/hall-heater/on/occupied  case  inactive  stopped: no device in room hall has reported occupancy
```

A time that isn't today has its date, and an on or off time that has gone by says `(passed)`. A `ConditionFunc` shows only its label. A case whose conditions can't be read keeps its state until they can, and a budget that can't be planned keeps its device off.

### In CI

`oikade status --check` prints the same, and exits 1 if something a command fixes is wrong, so a CI job can fail after a deploy. That's any of the [lines above](#whats-left-to-set-up) but a bridge with no controller, and automations that are down too.

- **A device that's only offline doesn't count,** as that's its hardware's and no deploy fixes it, unless you name it, a capability of it, or its room: `oikade status --check boiler room/hall` exits 1 if either has a device offline.
- **A device's not being in your code never counts,** as nothing is broken.

## History

`oikade history` says what the house did and why, oldest first, under a heading for each day:

```console
$ oikade history porch-light --since 16:00
Wed 18 Nov, yesterday
16:20:03  deployed version f113018
16:20:04  automations connected
16:41:00  device/porch-light/on  wrote true  by case/porch-light/on/dusk
16:41:01  device/porch-light/on  true  was false
23:05:12  device/porch-light/on  wrote false  by matter
23:05:12  device/porch-light/on  false  was true
Thu 19 Nov, today
03:00:40  device/porch-light  went offline  no answer on the LAN
03:02:09  device/porch-light  came back online
07:00:00  device/porch-light/brightness  wrote 40%  by flow/morning
07:00:01  device/porch-light/brightness  40%  was 100%
09:30:00  automations disconnected
```

Each event is a line: its time, the ref of what it's about, what happened, and why, dimmed on a terminal.

- **A write says who made it:** `by case/…` or `by flow/…` for a rule, `by matter` for a controller such as the Home app, and `by command` for `oikade set`.
- **A write with no `by`** is the host putting a capability back to its default once no rule holds it.
- **A change with no write before it** was made at the device, by its switch or its maker's app.
- **A refusal** names what holds the capability: `refused a Matter write of true  case/desk-plug/on/hall-too-humid holds it at Safety`.
- **A deploy, and your automations connecting or going down,** show whatever you ask for, dimmed, because they're often the reason.

A history with nothing in it says so, and whether the host has such a thing on record.

### What to show

`oikade history` and `oikade tail` take what to show as [refs](devices.md#refs), as every output prints them, so a ref from one pastes into the other. Several refs show them together:

| A ref | Shows |
|---|---|
| `device/porch-light`, or `porch-light` | The device: its changes, the writes to it, when it went offline and came back, and when it was forgotten or renamed. It's followed back through the names it had |
| `device/porch-light/on` | One capability, and what happened to the device as a whole |
| `room/hall` | The devices in the room now |
| `case/porch-light/on/dusk`, `flow/morning` | The rule's runs, pauses and outside calls, and the writes it made |
| `plugin/overkiz` | The plugin's own problems, such as a refused login |
| `matter` | The Matter bridge: its pairing window, controllers pairing and leaving, and what controllers wrote. A device called `matter` is `device/matter` |

### When

`oikade history` starts 24 hours back unless `--since` says, and `--until` stops it short. Each takes:

- how long ago: `24h`, `90m`, `3d`
- a time of day, the last one: `03:00`
- a date and time: `"2026-11-18 03:00"`

All are in your machine's time zone. An `--until` time of day that would fall before `--since` is the next one instead, so `--since 10:00 --until 16:00` is one afternoon.

### How far back

The host's event log keeps each change, write and rule run, for [`oikade history`](#history), [`oikade tail`](#following-it-live) and [`oikade replay`](testing.md#replaying-last-week). It's bounded by age and size, oldest first: 14 days and 64 MiB by default, which `oikade-host --event-log-age` and `--event-log-mib` change. A line on stderr says where the log starts if you ask for more. Losing it costs nothing else, and Prometheus stays the place for longer than the log keeps.

### Following it live

`oikade tail` follows the same events as they happen. It says `Following home's events` on stderr once the host is listening, naming the context, so a quiet house doesn't look hung. With `--since`, it prints that history first, then follows, and it says so if a ref names nothing the host has on record, since it would otherwise follow in silence.

## Setting a capability

`oikade set` takes a capability's ref and a value, and acts like tapping the device:

```console
$ oikade set device/hall-heater/setpoint 19.5
device/hall-heater/setpoint is 19.5°C: oikade set holds it until 15:30
```

- **It checks the value first,** against what the capability takes: `true`, `false`, `on` or `off` for a bool, a number in its range, or one of its names. So `oikade set device/salon/on maybe` says `device/salon/on takes true or false, not maybe`. A device and a capability as two words are refused, with the ref to use.
- **On a decided capability, it's an outside change,** shown as `oikade set`, so the decision's [override line](automations.md#override-lines) decides how long it lasts. A capability a safety line holds refuses it, rather than writing it back a moment later.
- **It waits up to 3 seconds for the device to answer.** A report within half a step of the value is its answer (0.5 for brightness, 0.25 for a setpoint), as a driver rounds brightness 33 to 33.1, but a report of another value on the way, as from a lamp mid-fade, isn't. Then it says what the capability ended up at and why, or that the device hasn't answered yet, which isn't a failure: the write was sent.

If something holds the capability at another value, `set` fails, with exit 1, and its hint says what lets go:

- **A decision's own lines:** `device/towel-rail/on is false, not true: no line applies, so its Default wins`, when `NoOverride` wrote it back.
- **A rule's claim:** `oikade pause flow/bedtime lets go of it until you resume it`. A Safety rule's claim ends only with its release, as a pause keeps it.
- **A limit,** such as a `MinimumOff`, lets go when it's done.

## Pausing a rule

`oikade pause` takes a rule's [ref](devices.md#refs), as a claim's `source` in `oikade status --json` gives it. A rule's name on its own, or a case's three words, are refused, with the ref to use.

```console
$ oikade pause flow/bedtime
Paused flow/bedtime, and ended its claims on device/lounge-lamp/on
```

A pause is state, like tapping a light: the host keeps it, it needs no Git, and it lasts through deploys and restarts until `oikade resume`. A rule a deploy deletes loses its pause.

- **A paused rule stops at once.** A flow doesn't start, a flow's run in progress ends, and a case stops working out whether it applies and leaves its device as it is. Times that pass during a pause aren't caught up.
- **Its claims end,** except a safety line's, which end only with a release, so a pause can't reopen a valve. `oikade pause` names the claims it ended, and the Safety ones it kept.
- **Some pauses need `--allow-safety`:** a [safety line](automations.md#safety-lines), since that stops its protection, a rule holding a Safety claim, and anything while no process is connected and no deployed version says what it is.
- **A Safety claim made by a flow you paused** stays until something releases it, because the run that would have is gone.

## Output for scripts

`--json` on `list`, `status`, `history`, `tail`, `replay`, `set`, `pause`, `resume`, `forget`, `remove`, `pair` and `setup` prints JSON whose fields stay stable for scripts, on stdout. Progress and hints stay on stderr.

- **`list`** prints an array, each thing with its `ref`, and `[]` when there's none.
- **`history` and `tail`** print a line of JSON an event, the [host API](host-api.md#streams)'s `Event`, and nothing when there's no event of what you asked for.
- **A command that changes something** prints `{"ok": true, "ref": …}`, with what it did, such as `set`'s `outcome`, `pause`'s `ended` and `kept`, or `pair`'s `window`.
- **A command that fails** prints `"ok": false` and its `error`, however it failed, the host out of reach included.
- **`forget` and `remove`,** which take several refs, print `refs` instead: what they forgot or removed, even if one then failed. `forget` adds what became of each device as `forgotten`, and a room left empty as `warning`.

## Errors, exit codes and help

Every command prints for a person first, and leaves `--json` and stdout clean for scripts.

- **An empty answer says so,** and what would fill it, on stderr: `No clients. oikade-host add client/NAME, on the host's machine, adds one.`
- **A command that changes something** ends on a line saying what changed, with a green `✓` on a terminal: `Deployed version f113018`, `Paused flow/bedtime`.
- **An error is a sentence,** and often a `Hint:` with the command that fixes it. `--debug`, or `OIKADE_DEBUG=1`, adds its causes in full.
- **Colour and glyphs** are only for a terminal, and never with `NO_COLOR` set or `TERM=dumb`.

| Exit | When |
|---|---|
| 0 | It did what you asked |
| 1 | Something failed: a ref that parses but names nothing, a refusal that needs `--allow-safety`, a host out of reach, or `--check` finding something wrong |
| 2 | The command was typed wrong: an unknown command or flag, a ref that doesn't parse, or a kind the command doesn't take, which it names. It prints that command's usage |

`oikade help` lists the commands by what you're doing. `oikade help KIND`, such as `oikade help device`, lists the commands that take one, and `oikade COMMAND --help` says what one does, with its flags and an example. A word from another tool, such as `rm` or `logs`, names the command to use, and a command typed wrong names the one it's likely a slip for.
