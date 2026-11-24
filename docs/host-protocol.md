# Host protocol

How your process and the host talk: a Unix socket in the data directory, `host.sock`, carrying one JSON message per line. It's for anyone changing either side. Nobody writes it by hand: a home's `Run` speaks it, and the code is `internal/host/wire`, with a plugin's part in `internal/host/pluginwire`. What each side owns is in [architecture.md](architecture.md#the-host), and OIK-143 holds the design.

The host creates the socket with mode `0600`, and whoever can reach it can run the home. It has no network listener. A message can be up to 16 MiB, and a write that takes over 5 seconds fails.

## Sessions

The host starts your binary as `<binary> automations`, with its own environment and these (`Host.processEnv`):

| Variable | Value |
|---|---|
| `OIKADE_SOCKET` | The socket's path |
| `OIKADE_SECRETS_DIR` | The directory of what plugins' setups kept, one file each |
| `OIKADE_VARS_FILE` | Your values' file: a JSON object of names to values, which `oikade.Var` reads, and which only the host's user can read. The host writes it before each process it starts from your binary, plugins' too, from the values the last deploy sent (OIK-365) |

A process has 5 seconds to send its `hello`:

```json
{"type":"hello","version":7,"role":"automations","rules":["hall-fan on humid","morning"]}
```

`version` comes first, so that it can change. It rises with any change to what a message means, because a host outlives a deploy and can meet a binary built against something else. The host refuses any other version, and says so in its log.

### What a hello carries

| Field | Means |
|---|---|
| `rules` | The process's rules by key, so the `welcome` can return their saved state |
| `averages` | The rolling averages its conditions read, by name, so the `welcome` can return their saved state |
| `signals` | The home's signals, by name, so the `welcome` can return their saved state |
| `info` | Each rule's `name`, `kind` (`case` or `flow`), `safety` and `latch`, and a case's `device`, `capability` and `case`, which `oikade status` shows and `oikade pause` checks. A Safety rule's `rooms` are each `room`, `capability` and `deviceTypes` (none for any) it writes at Safety, which a deploy checks the declared rooms and types against |
| `control` | Each `device` and `entry` the home arbitrates, or a `room`'s, with its `deviceTypes`, which each device there of those types with the capability takes unless it has its own |
| `inventory` | Every device the home declares with `Home.Device`: its `name`, and its `plugin` and `id`, `room` and `deviceType` if it gives them. None leaves every device under its app's name (OIK-295) |
| `matterName` | The Matter bridge's name from `oikade.Options`, empty for `Oikade` (OIK-296) |
| `hiddenFromMatter` | The plugins whose devices the home keeps off Matter, each `plugin` with the devices it `shown` (OIK-294) |

The host takes these up before the `welcome`, in this order, and refuses the `hello` if it can't:

1. **`inventory`:** it saves it, then applies it to the registry. It can refuse, as with a rename of a device something holds.
2. **`control`:** it adds and changes settings. One the home has dropped stays until a deploy of that version settles.
3. **`matterName`:** it saves it, then renames the bridge in place.
4. **`hiddenFromMatter`:** it saves it, then takes the devices off the bridge. A process whose version is still settling has no say here: the host takes the version's `hiddenFromMatter` from its manifest once it's confirmed, so a deploy that fails takes nothing off.

A process the host is going to turn away has no say in any of them.

### The answer

The host answers `rejected` or `welcome`.

**`rejected`** has an `error`, and the host closes the connection after it. The version or role is wrong, the host couldn't take up the `hello`, or an automations process is already connected: exactly one runs at a time, because two would run everything twice.

**`welcome`** has:

| Field | Means |
|---|---|
| `paused` | The rules not to run |
| `devices` | Every device, with its `room`, `capabilities`, `deviceTypes`, `values`, `lastSeen`, `online` once its plugin has said, and the capabilities the host arbitrates (`arbitrated`) |
| `holds` | The saved state of each rule in `rules` that has any |
| `fires` | When each rule's timers were last accounted for |
| `averageStates` | The saved state of each average in `averages` |
| `signalStates` | The saved state of each signal in `signals`: its `value`, `since`, and when a delay or linger ends (`onAt`, `offAt`) |
| `runs` | How many `run` messages follow straight after it. Each carries one unfinished flow run and its journal in `unfinished`. They're separate because a journal can reach 4 MiB, and a message 16 MiB |

The host streams from the `welcome` on. A change landing as the session opens can arrive twice, so a process ignores a value it already has.

## From the host

| Type | Fields | Means |
|---|---|---|
| `run` | `unfinished` | One unfinished flow run, after the `welcome` |
| `change` | `device`, `changes` (each a `capability`, `value` and `previous`), `time` | One report from the device changed these values. Take them in together, before any rule looks |
| `discovered` | `discovered` (a device) | A device reported for the first time |
| `seen` | `device`, `time` | A device reported, changed or not |
| `availability` | `device`, `online` | A device went offline, or came back. A device first heard of online sends none |
| `ping` | `id` | Answer with a `pong` with this `id` |
| `pause` | `rule` | Stop running this rule until a `resume`, and end its claims below Safety |
| `resume` | `rule` | Run this rule again |
| `reply` | `id`, `error`, and what the request returns | Answers a request. One with `error` set failed |

## From your process

Each request has an `id`, and gets a `reply` with the same one. Requests are carried out in order. The rest are messages with no reply.

### Requests

| Type | Fields | Does | Reply |
|---|---|---|---|
| `claim` | `device`, `capability`, `claim` | Makes a claim on an arbitrated capability. The reply means it's on disk | `arbitrated`: false if it isn't, so the process writes it instead |
| `release` | `device`, `capability`, `source` | Ends a source's claim. The reply means it's on disk | `arbitrated`, as for `claim` |
| `releaseAll` | `source`, optional `below` | Ends a source's claims everywhere, or with `below` only those under that level (3 is Safety). The reply means it's on disk | |
| `set` | `device`, `capability`, `value`, and `source`, the rule it's for | Writes a capability | |
| `saveHold` | `rule`, `hold` | Saves a hold's state. The reply means it's on disk | |
| `deleteHold` | `rule` | Forgets a hold's state. The reply means it's on disk | |
| `saveFire` | `rule`, `time` | Saves when a rule's timers were last accounted for. The reply means it's on disk | |
| `saveAverage` | `rule`, `average` | Saves a rolling average a condition reads, by its name in `rule`, such as `greenhouse temperature 24h`: the reading's sums in buckets, and its last value and when. The reply means it's on disk | |
| `saveSignal` | `rule`, `signal` | Saves a signal's state, by its name in `rule`, such as `anyone-home`. The reply means it's on disk | |
| `journal` | `run`, `data` | Appends a journal entry. The reply means it's on disk | `seq`, its place in the run |
| `finish` | `run` | Forgets a run's journal. The reply means it's on disk | |

### Messages

| Type | Fields | Means |
|---|---|---|
| `pong` | `id` | Answers a `ping` |
| `ruleRun` | `rule`, `result`, `time` | How a rule's run ended, for `oikade tail`. Runs whose condition was false aren't sent |
| `roomReads` | `rule`, `reads` | Every room a hold's `When` has read since the process started, each `room` with its `types`, sent when that grows. The host keeps them for the session, so `oikade forget`, and a deploy's declared rooms and types, can warn when they empty one |
| `stopped` | `rule`, `stopped` | Why a hold can't tell whether it should be active: its `When` failed, so it keeps its state, or its budget couldn't be planned, so its device stays off. Sent when that changes, with no `stopped` once it can tell again. The host keeps them for the session, for `oikade status` |
| `whyNot` | `rule`, `whyNot` | Why a case is inactive, as its conditions say, sent when that changes, with none once it's active or can't tell. The host keeps them for the session, for `oikade status` |
| `metrics` | `metrics` | The process's rule metrics in Prometheus's text format, for the host to serve |

## Plugins

A plugin your home imports has its own session on the same socket. The host starts your binary as `<binary> plugin <name>`, with the same environment as your automations, and `<name>` comes from `plugins` in what [check mode](#check-mode) prints. The process says who it is:

```json
{"type":"hello","version":7,"role":"plugin","plugin":"ewelink-lan"}
```

The `welcome` has `topic`, the plugin's topic root, `declared`, the devices on record for it, and `setTimeout`. The host refuses a plugin it doesn't know, and a second connection for one that's connected.

After that the session carries `publish` both ways, with `topic`, `data` and `retain`: one message of the [oikade/v1 contract](plugin-contract.md) each, with the same topics and payloads. It carries `ping` and `pong` too.

- A process doesn't answer a `ping` while a `Set` has run past `setTimeout`, so a stuck one counts as gone.
- When a session ends, the host treats the plugin as offline, as a Last Will would.
- A `publish` from the host with no session is an error to whoever wrote, not a message waiting.

## Heartbeats

The host pings every fifth of the heartbeat timeout, which is 10 seconds unless `--heartbeat-timeout` says. A process that doesn't connect, or doesn't answer for that long, gets `SIGKILL` and is replaced.

A process answers only once its engine's lock is free. A condition stuck in a loop holds that lock, so it stops the answers, and the process is replaced rather than left silently doing nothing.

## When a session ends

- **A failed `claim`, `release`, `releaseAll` or `journal` ends the session,** because what's on disk is then unknown, and the next process works it out from what is.
- **A process that loses the host exits at once,** since it can't act. If a request isn't answered in 30 seconds, the session counts as lost.
- **A process that falls 4096 messages behind is disconnected,** and its replacement starts from a fresh `welcome`.

The host keeps running. It restarts your binary after 1 second, doubling up to 30 seconds, and goes back to 1 second once a process has lasted a minute. It waits for the old process to go, and ends its session, before starting the next one. Going away during a deploy's settle period is different: see [Deploys](#deploys).

## Claims when a process goes away

Claims live in the host, so nothing happens to them when your process dies. Each ends by its own rule, whether or not a process is running:

| Claim | Ends |
|---|---|
| `for` | When its time is up |
| `until` next | When the value without the claim next changes |
| `until` released | At a release, so it waits for a process to make one |
| A hold's | When the hold ends, so it waits for a process to work that out |

So a Safety hold's claim stays in force while there's no process. A hold whose condition ended meanwhile is ended by the next process, once.

A rule's claims end with the rule, but only through a deploy, which is what checks for a Safety rule going. See [Deploys](#deploys). A binary changed some other way ends nothing.

## Check mode

The host runs a binary as `<binary> check` before it stops anything. That loads the home with no writes and no session, and prints one line of JSON, `wire.Manifest`, as its last line of output, then exits 0:

```json
{"protocol":7,"rules":[{"name":"washer-plug on leak","kind":"case","safety":true,"latch":true,"device":"washer-plug","capability":"on","case":"leak"}]}
```

| Field | Means |
|---|---|
| `protocol` | The protocol version it was built against. One other than the host's rejects the version |
| `rules` | Each rule, as a `hello`'s `info` has it |
| `plugins` | The plugins the home imports |
| `control` | Its device settings, as a `hello` has them |
| `inventory` | The devices it declares, as a `hello` has them |
| `warnings` | What `oikade check` warns of, which doesn't stop a deploy |
| `vars` | The values its code read with `oikade.Var` as it was built (OIK-365). A deploy or a rollback whose values lack one is refused |
| `hiddenFromMatter` | The plugins whose devices it keeps off Matter |

Check mode is given the values the version would run with, the deploy's for a deploy, in a file of its own named in `OIKADE_VARS_FILE`, which is removed once it's done. Run by `oikade` on your machine, for its check and `oikade dev`, a binary also gets `OIKADE_VARS_FROM_ENVIRONMENT=1`, and the environment wins over the file for any name, as it does for the values a deploy sends. The host never sets it.

A failure, such as a value's name that isn't an environment variable's, rejects the version. So does a check that runs over 30 seconds.

## Deploys

`oikade deploy` and `oikade rollback` talk to the host through its [API](host-api.md), on `api.sock`. A deploy goes in this order, and any failure leaves or brings back the last version:

1. **Upload.** The command streams your values and the binary to the host, which keeps the binary in `uploads` until it has all of it, then copies it to `versions/<id>` in the data directory. The ID comes from its contents, so deploying what's running with the same values changes nothing, and with others starts its processes again with them, through the same steps.
2. **Check.** The host runs check mode, and compares the result with the running version. A version whose code uses a value the deploy lacks is refused. A Safety rule that's gone, or a rule that's gone and holds a Safety claim, refuses the deploy unless it says `allowSafety`. A rename counts, because a hold's latch lives under its name.
3. **Swap.** The host records the version as unconfirmed, with its values and the last ones to go back to in the same write, and every rule that has anything on record. It sends the old process `SIGTERM`, kills it after 5 seconds if it's still there, and ends its session. It reads the claims again, because the old process could claim until it stopped, and a new finding brings the old version back unless the deploy said `allowSafety`. Then it copies every flow run's journal, and only then does the new process start, so two never overlap. Waiting runs keep their journals and resume.
4. **Settle.** The host waits for the new process to connect, then for the settle period, 5 seconds unless `--settle` says. Until the version is confirmed, the non-Safety claims a flow gives up when its run is dropped or fails stay in force: the host holds the release back. If the process goes away at any point before that's up, the last version comes back at once, and the command says why, with the journals as they were copied and the held claims never having ended.
5. **Confirm.** The host confirms the version and keeps the five latest. It ends the claims it held back, except any the flow has claimed again since. It ends what's left of rules the version doesn't have: their claims, Safety ones included, their saved state, and the runs of flows it doesn't have, including one that's now another kind of rule with the same name, because nothing else would. The record says this is due, so a host that dies first does it when it starts.

A host that dies during steps 3 or 4 finds the unconfirmed record when it starts, and goes back to the last version. A version that failed goes the same way: once the last version is back, what its own rules left ends, because no code is left to end it. Rules on record in step 3 are spared, so it can only end what appeared after that. If there's no version to go back to, or the one that comes back has rules the host can't read, nothing ends: what the failed version left stays until the next confirmed deploy.

A process never ends a rule or a flow it doesn't have. Without its flow, a binary leaves a run alone, with its journal and claims, and the run carries on if a later binary has the flow. Only step 5 ends it. So a failed version, or a binary changed some other way, ends no rule and no flow.

A version that settled and fails later restarts as usual, and stays until a rollback. `rollback` is a deploy of the version before the running one, on the same terms, and it forgets the running one, so a second rollback goes back further.

### What going back restores

Going back restores a version, and the flow runs and claims a flow gave up, but not everything the failed version did:

- **A waiting run** comes back with its journal as it was when the old process stopped, even if the failed version dropped it, finished it or took more steps in it. A run of that flow the failed version started goes.
- **A run the failed version started of a flow with none waiting** stays, so the last version carries it on or drops it, ending its claims.
- **A step it took** in that time did happen, and runs again when the last version replays the run.
- **A claim the failed version made or changed,** including one a new run made on a device the old run held, stays until the last version's flows or a removal end it. A claim a step released on its own has ended.
- **Rules' saved state** is as the failed version left it.

Only a deploy gets this. A version that's confirmed and rolled back later is a deploy of its own, so what it dropped before then stays dropped. A binary changed some other way has no version to wait for: a run it drops stays dropped, with its non-Safety claims, and steps it took stay in the journal. If those don't match the code that comes back, that run is dropped too.
