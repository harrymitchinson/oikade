# The host API

The API `oikade` calls to work a running host: deploy, status, set, the event log and the rest. It's for anyone changing `oikade` or the host, or writing a client of their own. The code is `internal/host/api`, whose `Request` has every field a call takes and whose `Response` has every answer. OIK-171 says why it's HTTP.

## Reaching it

| Where | Who | Authentication |
|---|---|---|
| `api.sock` in the data directory, a Unix socket with mode `0600` | Anything on the host's machine running as the host's user | None of its own: the socket's mode |
| `oikade-host --api-addr ADDR`, over TLS (port 8443 if the address has none) | A client on another machine, such as `oikade` on a laptop | A pinned key and a bearer token: see [Over TLS](#over-tls) |

Both serve the same routes, but for adding a client, which only the socket does. The API is apart from `host.sock`, which only your process and plugins use, and which [host-protocol.md](host-protocol.md) describes.

## Calls

A `GET` takes no body. A `POST` takes a JSON object, and every field is optional at this level: a call that needs one refuses without it.

| Route | Takes | Answers with |
|---|---|---|
| `GET /v1/info` | | `goos`, `goarch`, and `hostVersion`, oikade-host's own version |
| `GET /v1/status` | | `status`: see [Status](#status) |
| `GET /v1/matter` | | `matter`, the bridge as `status` has it |
| `GET /v1/devices` | | `registry`; `unimported`, the devices of plugins the running version doesn't import; `outsidePlugins`; `safetyHeld`, the plugins' devices a Safety claim holds; `hiddenFromMatter`, the devices your home keeps off Matter |
| `POST /v1/deploy` | Your values and the binary, as `multipart/form-data` ([Deploys](#deploys)), and `?allowSafety=true` in the query | See [what a deploy answers](#what-a-deploy-answers) |
| `POST /v1/rollback` | `allowSafety`; `vars`, values that replace the host's | The same as a deploy, but `unchanged` |
| `POST /v1/set` | `device`, `capability`, `value` | `outcome`: the capability with its claims, once the device answered and nothing wrote it back, after up to 3 seconds; none if the device hadn't answered. A set a Safety claim refuses has `outcome` as the capability is, beside `error` |
| `POST /v1/pause`, `POST /v1/resume` | `rule`, the case's or flow's [ref](devices.md#refs), such as `case/fan/on/humid`; `allowSafety` | `needsAllowSafety`. A pause has `ended`, the claims it ended, and `kept`, its Safety claims, each as the capability's ref, such as `device/fan/on` |
| `POST /v1/matter/open` | `windowSeconds`, 180 to 900, and 900 if it's left out | `matterWindow`: `manualCode`, `qrCode`, `passcode`, `fresh` and `expires`, once the window is advertised |
| `POST /v1/backup` | | `backup`, the file it wrote on the host |
| `POST /v1/setup` | `plugin` | A stream of the plugin's setup steps: see [Setup](#setup) |
| `POST /v1/setup/reply` | `plugin`, `session`, `seq`, `answer` | |
| `POST /v1/devices/forget` | `devices`, by name, or `plugin`, for every device of that plugin; `allowSafety` | `forgotten`; `needsAllowSafety`; `warning`, such as a room a hold reads that's left empty |
| `GET /v1/clients` | | `clients`, never their tokens, with those waiting for their login, each with `expires`, when its code runs out |
| `POST /v1/clients/add` | `client` | `code`, `expires`, `pin` and `address`. Only on the socket |
| `POST /v1/clients/add` | `client`, `token: true` | `token`, shown only once, and `pin`. Only on the socket |
| `POST /v1/clients/remove` | `client` | `cancelled` if it was waiting for its login, which cancels its code |
| `POST /v1/login` | `code` | `token`, shown only once, and `client` |
| `GET /v1/tail` | A [filter](#filters) in the query | A stream of events: see [Streams](#streams) |
| `GET /v1/events` | A [filter](#filters) in the query | The event log, as a stream |

A forget checks every device before it forgets any. Each in `forgotten` has its `device`, `ref` and `plugin`, the claims it `ended` by their sources' refs, `offMatter` if it left the bridge, `comesBack` if its driver's next report brings it back, as a simulated device's does, and `unimported` if your home doesn't import its plugin.

**A call outlives its connection.** Once the host has a request it finishes it, so a deploy that has begun goes on after the command that asked is killed. The answer then has nowhere to go, and `oikade status` shows how it ended.

### Limits

| Limit | Value |
|---|---|
| Sending a call's body | 5 seconds, but a deploy's, which may take as long as it keeps sending: up to 30 seconds with nothing |
| A call's body | 16 MiB, and 4 KiB for a login over TLS, which takes no token |
| A deploy's body | 1 GiB in all, and 1 MiB of values |
| Writing an answer, or one line of a stream | 5 seconds, after which a reader is cut off |

Your values and setup's answers go only in a body, never in a path or a log.

## Deploys

A deploy's body has its parts in this order:

1. `vars`: your values, as a JSON object of names to strings. A deploy with no `vars` part, as `oikade deploy --keep-vars` sends, keeps the host's.
2. `binary`: the binary.

Only the closing boundary says it's all there. The host keeps the upload in `uploads` in its data directory until the deploy has copied it, and an upload that's cut off, or a host that stops during one, leaves nothing there and deploys nothing.

- **The values replace the host's whole.** A version whose code uses one they lack is refused before anything stops, with `missingVars` (OIK-365), whether they're the deploy's or the host's.
- **A rollback keeps the host's values,** unless it sends `vars` of its own.
- **A name is an environment variable's:** upper-case letters, digits and underscores, starting with a letter. A set with any other is refused.
- **No value is ever in an answer or a log,** only names.

### What a deploy answers

| Field | Means |
|---|---|
| `version` | The version running now |
| `unchanged` | A deploy of the version already running. With `varsChanged`, its processes started again with the new values. Without, nothing changed |
| `restored` | The last version came back: the new one failed once it started, or a rule the new one doesn't have made a Safety claim while the old one stopped, which `findings` names |
| `findings` | What would remove Safety protection, which refused it unless it said `allowSafety` |
| `varsChanged` | The names of the values it added, changed or removed |
| `missingVars` | The values a refused version uses that its set lacks |
| `deviceChanges` | What the version's device declarations changed, a line each |
| `unseen` | The names it declares whose hardware hasn't reported |
| `warning` | Rooms a hold reads that the declarations leave empty, and devices they put in a room whose decision they don't take |
| `unimported` | The devices on record of plugins the running version doesn't import, each with its `device`, `ref`, `plugin`, and `safety` if a Safety claim holds it |
| `takenOffMatter` | The devices on the bridge the version hides from Matter |

## Status

`GET /v1/status` answers with what `oikade status --json` prints, one object (OIK-355). `api.Status` has its fields.

| Field | Means |
|---|---|
| `home` | Your home: `version`, `deploying`, `process`, and `since`, when your automations last connected or went down, or the host started |
| `devices` | Each device, with its `room`, `online`, its `capabilities`, its `deviceTypes`, its `plugin` if a plugin reports it, its `problem` if its driver says one, `offlineSince` while it's offline (when the host first heard it was), `pluginNotImported`, and `declare`, the line that would declare it, if your code doesn't |
| `rules` | Each rule: see below |
| `signals` | Each [signal](automations.md#signals), in the order your home made them: see below |
| `plugins` | Each plugin the running binary imports or that reports a device on record: its `name`, its `ref` (`plugin/NAME`), its `problem` if it says something's wrong with it as a whole (OIK-17), `needsSetup` if it says its [setup](#setup) needs running, and the `devices` it reports |
| `matter` | The Matter bridge, as `GET /v1/matter` has it, or `null` when the host has Matter turned off |

`oikade status REF --json` narrows it, and keeps its shape: `home` is always there, a kind no ref names is an empty array, and `matter` is `null` unless it's named.

A capability has its `name`, its `ref`, such as `device/desk-plug/on`, and its `value`, and, if it's arbitrated, its winning `level` and the value it `wanted`, the `case` that wins, a `limit` holding the winner back until `limitUntil`, and its `claims`, highest first. Each claim has a `source`, the [ref](devices.md#refs) of who made it, such as `case/fan/on/humid` or `matter`, the `case` that made it if one did, its `level` and `value`, and `winner`, `latched` and `expires` where they apply.

Each rule has its `name`, its `ref`, which `pause` and `resume` take, its `kind` (`case` or `flow`, or none for a paused rule no running code names), `safety` and `paused`, and a case's `device`, `capability` and `case`. A case that isn't paused also has its state, from what it last saved (OIK-215):

| Field | Means |
|---|---|
| `active` | Whether its start is written. Always there for a case |
| `since` | When it last became active. Missing while it isn't |
| `onAt`, `offAt` | When its on-delay or off-delay ends. Missing when none is running. One in the past ran out while it was stopped |
| `latched` | A latched hold, from its start until a release |
| `stopped` | Why its `When` can't be read, so it keeps its state, such as `no device in room hall has reported occupancy`, or why its budget couldn't be planned, which keeps its device off. Only a connected process says |
| `whyNot` | Why an inactive case's conditions are false, such as `device/greenhouse/temperature is 5, not below 3`. Only a connected process says |
| `budget` | A budget hold's day: `date`, `targetMinutes`, `plannedMinutes`, `runMinutes` and `missedMinutes`, the planned minutes that went by with the device off while the budget's conditions were true. The last two are saved at most every 5 minutes. `date` may be a day that's over, if it hasn't planned since |

Each signal has its `name`, its `ref` (`signal/NAME`), and its state, from what your process last saved. The host knows them from your process's `hello`, or with none connected, from the running version. A signal with no saved state yet, as just after a deploy adds one, has only its name and ref:

| Field | Means |
|---|---|
| `value` | What it is |
| `since` | When it took that value, or when it started |
| `onAt`, `offAt` | When its `Delay` ends, and it turns true, or its `Linger` ends, and it turns false. Missing when none is running |
| `why` | What its conditions say now, such as `its conditions are false: group/presence/occupied isn't true on any of them`. Only a connected process says |
| `dead` | The devices it leaves out as [dead](automations.md#dead-sensors), by ref. Only a connected process says |

## Setup

`POST /v1/setup` runs a plugin's setup (OIK-276), and answers with its steps as lines of JSON (`api.SetupStep`):

1. The first line has only `session`, and says it has started.
2. Then the plugin's steps, as the [plugin contract](plugin-contract.md#setup) has them: a `say`, an `ask` or an `open`, each with its `seq`.
3. Last, a `done`, with a `result` or an `error`.

A `keep` is the host's to do, so the stream has only the secret's name, as `kept`, once it's on disk, and never its value. A refused setup, such as one of a plugin that has none, is a `422` with an `error`, as a call is.

Answer each `ask` and `open` with `POST /v1/setup/reply`, naming the `session` and the step's `seq`. `answer` is what the person typed, or an `open`'s redirect query, such as `code=abc&state=xyz`. A reply is taken once, and only for a step that's waiting. An answer may be a secret, so it's never logged.

A plugin runs one setup at a time, so a second ends the first, whose stream ends with a `done` saying so. Closing the stream ends the setup, and so does 10 minutes with no step and no answer. The stream's fields are added to, never renamed.

## Over TLS

- **The host's certificate is self-signed,** and a client trusts it by its pin: `sha256:` and the hex SHA-256 of the certificate's `SubjectPublicKeyInfo`. TLS 1.3 only.
- **Every call needs `Authorization: Bearer <token>`,** which is checked before any body is read. A missing, wrong or revoked token gets a `401`, and counts in `oikade_api_auth_failures_total`. Removing a client ends its streams.
- **`POST /v1/clients/add` is refused over TLS,** so a token can't make more of them.
- **`POST /v1/login` is the one call that takes no token.** It trades a code from `clients/add` for a new client's token. A code works once, for 10 minutes, and only in memory, so a host restart ends it. A wrong, used or expired code gets a `422` that doesn't say which, and counts as an auth failure.

## Answers

An answer is `application/json`, and a failure's is a `Response` with `error` set.

| Status | Means |
|---|---|
| `200` | It worked |
| `400` | The body isn't JSON, is over its limit, or is a deploy that was cut off |
| `401` | Over TLS, the call has no valid token |
| `404` | No such route, which includes the wrong method. `error` says which API this host speaks |
| `413` | A deploy over 1 GiB |
| `415` | A deploy whose body isn't `multipart/form-data`, from a client of another version |
| `422` | It didn't do what was asked, and `error` says why: the host refused it, or it failed there. The other fields can still say what happened, such as `restored` on a deploy whose version failed and was rolled back, and `findings` and `needsAllowSafety` on a refusal |
| `500` | The event log can't be read |

## Streams

These answer `application/x-ndjson`, one JSON object per line.

- **`GET /v1/tail`** sends an event with no `type`, which says the host is listening, then an `Event` as each thing happens, until the caller goes or the host stops. A tail that falls behind is cut off, with an `end` saying so. With `since`, the log's events from then come first, before the event with no `type`, and each event comes once, whether the log had it or it happened as the tail began.
- **`GET /v1/events`** sends an `EventLog`, then an `Event` for each thing the log holds, oldest first. The `EventLog` has `now`, the host's `zone` and `offset`, `start`, when the oldest event the log holds is from, whatever the filter, and `baseline`, each device's values before that. The last line has the type `end`, and a `message` if it didn't get to the end, such as `the client was removed`, so a cut-off read isn't taken for a short one. A host that stops partway through ends the stream with no `end` line, as one that crashed can't send it, and `oikade replay` says the host stopped.

### Filters

Both take a filter in the query, and the host does the filtering, so a caller doesn't read the whole log to show one device (OIK-355). With no query, each sends everything, as `oikade replay` needs.

| Parameter | Lets through |
|---|---|
| `since`, `until` | Events from and to these times, both included, in RFC 3339, such as `2026-11-18T03:00:00Z`. A tail has no `until` |
| `ref`, as often as you like | Events about any of the [refs](devices.md#refs) given |

| A `ref` | Its events |
|---|---|
| `device/porch-light` | Everything about the device and its capabilities, under each name it had: the log's `rename` events are followed back. A name is a device's only from the rename that gave it to the one that took it away, as another device may have had it before, or have it since. A tail carries on with a device that's renamed as it follows. A name nothing has now is the device that last had it, up to the rename that took it away |
| `device/porch-light/on` | The capability's, and what happened to the device as a whole: it went offline, or was forgotten or renamed |
| `room/hall`, `room/hall/on` | The same, for each device in the room now, not those that were in it then |
| `case/porch-light/on/dusk`, `flow/morning` | The rule's runs, pauses and outside calls, and the writes it made (`by`). A case's are found under each name its device had |
| `signal/anyone-home` | The signal's changes |
| `plugin/overkiz` | The plugin's own problems |
| `matter` | The bridge's events, and each controller's writes, refused or not |

The home's own events, `process` and `deploy`, pass whatever the refs, because they're often why the rest happened. A ref of any other kind, one that doesn't parse, a time that isn't one, and an `until` before its `since` are refused with a `422` that says why. A room, device, rule or plugin the host has no record of isn't refused: it has the home's own events and whatever the log holds of it.

### Events

An `Event` has its `time` and `type`. Every event but `process`, `deploy` and `end` has a `ref`, the ref of what it's about: a capability's, a device's, a rule's, a signal's or a plugin's, or `matter` for the bridge.

| `type` | What happened | Fields |
|---|---|---|
| `change` | A device reported a new value | `device`, `capability`, `value`, and `previous`, the value before, if it had one |
| `write` | The host wrote to a device, whether or not the device took it. `by` is the ref of whose write it is: the rule whose claim won or whose step set it (`case/porch-light/on/dusk`, `flow/morning`), `matter` for a Matter controller's, such as a Home app tap, or `command` for `oikade set`'s. It has no `by` when no rule's claim decided it, as when arbitration puts a capability back to its default. A `change` with no write before it was made at the device, by its switch or its maker's app | `device`, `capability`, `value`, `by` |
| `refused` | A controller's write, such as a Home app tap, that a claim turned away. The device wasn't written | `device`, `capability`, `value`, and `message`: which claim holds it, and at what level |
| `rule` | A rule's run ended | `rule`, `result` |
| `pause` | A rule was paused or resumed | `rule`, `message` |
| `call` | A flow made an outside call, which a replay answers rather than makes | `rule`, the flow; `message`, the call; `value`, its result; `result`, its error |
| `availability` | A device went offline, or came back. Not each report of the same, nor a device first heard of online. The host doesn't keep how a device was, so one that's offline when the host starts is said to have gone offline then | `device`; `message`, `offline` or `online`; `result`, its driver's problem with it as it went offline, such as a missing LAN key, if it has one |
| `plugin` | A plugin's own problem started, changed or cleared, such as a refused login. A plugin that's down says nothing of its problem, so going down doesn't clear one, and coming back with the same one doesn't start it again. A host that starts again says a problem still there again | `ref`, the plugin's; `message`, the problem, or none once it has cleared |
| `matter` | The Matter bridge's commissioning window opened or closed, or a controller paired or left. A window has opened once it's advertised, and one still open when the host stops closes then. The bridge says each once it has advertised the change, which can be a second or two after | `message`: `window opened`, `window closed`, `controller paired` or `controller left`; `value`, the controller's fabric |
| `signal` | A [signal](automations.md#signals) changed, as the host saved it. A new or renamed signal's first value isn't a change. Your process saves the latest state only, so a change and its undoing before the first reaches the host can be neither | `ref`, the signal's; `value`, what it is now; `previous`, what it was |
| `forget` | A device was forgotten, so its history stops here | `device` |
| `rename` | A deploy renamed a device, or gave its hardware the name your home declares | `device`, its name from then on; `previous`, its name before |
| `process` | Your process connected or disconnected | `message` |
| `deploy` | A new version took over | `message`, the version |
| `end` | The stream ended: the event log's last line, or the host ending a tail | `message`: why, if it stopped short |

`oikade replay` reads `change`, `write`, `call`, `process`, `deploy` and `pause`, and ignores the rest. It counts a `write` only if its `by` is a rule's or it has none, and never counts a `refused`.
