# The host API

This is how `oikade` talks to a running host. It's HTTP on `api.sock` in the data directory, a Unix socket with mode `0600`, so only the host's user can reach it. With `oikade-host --api-addr`, the same routes are served over TLS too, for clients with a token. It's apart from `host.sock`, which only your process uses and which [`host-protocol.md`](host-protocol.md) describes. OIK-171 says why it's HTTP.

## Calls

A `GET` takes no body. A `POST` takes a JSON object, and every field is optional at this level: a call that needs one refuses without it. `internal/host/api` has the fields as `Request`, and the answers as `Response`.

| Route | Fields | Answers with |
|---|---|---|
| `GET /v1/info` | | `goos`, `goarch`, `hostVersion`, oikade-host's own version |
| `GET /v1/status` | | `status` |
| `GET /v1/matter` | | `matter` |
| `GET /v1/devices` | | `registry`; `unimported`, the devices of plugins the running version doesn't import; `outsidePlugins`; `safetyHeld`, the plugins' devices a Safety claim holds; `hiddenFromMatter`, the devices your home keeps off Matter |
| `GET /v1/secrets` | | `secrets`, never their values |
| `POST /v1/deploy` | The binary itself, as `application/octet-stream`, and `?allowSafetyRemoval=true` in the query | `version`, `unchanged`, `restored`, `findings`; `unimported`, the devices of plugins the version doesn't import, once it's running; `unsetSecrets`, the secrets the version uses that aren't set; `takenOffMatter`, the devices on the bridge it hides from Matter |
| `POST /v1/rollback` | `allowSafetyRemoval` | `version`, `findings`, `unimported`, `unsetSecrets`, `takenOffMatter` |
| `POST /v1/set` | `device`, `capability`, `value` | `outcome`, the capability with its claims once the device answered and nothing wrote it back, after up to 3 seconds; none if the device hadn't answered |
| `POST /v1/pause`, `POST /v1/resume` | `rule`, the case's or flow's [ref](devices.md#refs), such as `case/fan/on/humid`, and `allowSafety` | `needsAllowSafety`; a pause's `ended`, the claims it ended, and `kept`, its Safety claims, each as the capability's ref, such as `device/fan/on` |
| `POST /v1/matter/open` | `windowSeconds`, 180 to 900, and 900 if it's left out | `matterWindow`: `manualCode`, `qrCode`, `passcode`, `fresh` and `expires`. It answers once the window is advertised |
| `POST /v1/backup` | | `backup`, the file it wrote on the host |
| `POST /v1/setup` | `plugin` | A stream of the plugin's setup steps: see [Setup](#setup) |
| `POST /v1/setup/reply` | `plugin`, `session`, `seq`, `answer` | |
| `POST /v1/secrets/set` | `secret`, `secretValue` | |
| `POST /v1/secrets/remove` | `secret` | |
| `POST /v1/devices/forget` | `devices`, by name, or `plugin` for every device of that plugin, and `allowSafety`. Every device is checked before any is forgotten | `forgotten`, each with its `device`, `ref`, `plugin`, the claims it `ended` by their sources' refs, `offMatter` if it left the bridge, `comesBack` if its driver's next report brings it back, as a simulated device's does, and `unimported` if your home doesn't import its plugin; `needsAllowSafety`; `warning` |
| `GET /v1/clients` | | `clients`, never their tokens, with those waiting for their login, each with `expires`, when its code runs out |
| `POST /v1/clients/add` | `client` | `code`, `expires`, `pin` and `address`. Only on the socket. |
| `POST /v1/clients/add` with `token: true` | `client` | `token`, shown only once, and `pin` |
| `POST /v1/clients/remove` | `client` | `cancelled` if it was waiting for its login, which cancels its code |
| `POST /v1/login` | `code` | `token`, shown only once, and `client` |

A call has 5 seconds to send its body, except a deploy, which may send its binary for as long as it keeps sending: up to 30 seconds with nothing, and 1 GiB in all. The host keeps the upload in `uploads` in its data directory until the deploy has copied it, and an upload that's cut off, or a host that stops during one, leaves nothing there and deploys nothing. Secret values go only in a body, and never in a path or a log.

**A call outlives its connection.** Once the host has a request it finishes it, so a deploy that has begun goes on after the command that asked is killed. The answer then has nowhere to go, and `oikade status` shows how it ended.

## Status

`GET /v1/status` answers with what `oikade status --json` prints, one object (OIK-355):

| Field | Means |
|---|---|
| `home` | The home: `version`, `deploying`, `process`, `since` (when your automations last connected or went down, or the host started), and `unsetSecrets`, the secrets the running version uses that aren't set (OIK-297). |
| `devices` | Each device, with its `room`, `online`, its `capabilities`, its `plugin` if a plugin reports it, and `offlineSince` while it's offline: when the host first heard it was. |
| `rules` | Each rule, below. |
| `plugins` | Each plugin the running binary imports or that reports a device on record: its `name`, its `ref` (`plugin/NAME`), its `problem` if it says something's wrong with it as a whole (OIK-17), `needsSetup` if it says its [setup](#setup) needs running, and the `devices` it reports. |
| `matter` | The Matter bridge, as `GET /v1/matter` has it, or `null` when the host has Matter turned off. |

`oikade status REF --json` narrows it, and keeps its shape: `home` is always there, a kind no ref names is an empty array, and `matter` is `null` unless it's named. A capability's claims each have a `source`, the [ref](devices.md#refs) of who made it, such as `case/fan/on/humid` or `matter`. Each rule has its `name`, its `ref`, which `pause` and `resume` take, its `kind` (`case` or `flow`, or none for a paused rule no running code names), `safety` and `paused`. A hold that isn't paused also has its state, from what it last saved (OIK-215):

| Field | Means |
|---|---|
| `active` | Whether its start is written. Always there for a hold. |
| `since` | When it last became active. Missing while it isn't. |
| `onAt`, `offAt` | When its on-delay or off-delay ends. Missing when none is running. One in the past ran out while it was stopped. |
| `latched` | A latched hold, from its start until a release. |
| `stopped` | Why its `When` can't be read, so it keeps its state, such as `no device in room hall has reported occupancy`, or why its budget couldn't be planned, which keeps its device off. Only a connected process says. |
| `budget` | A budget hold's day: `date`, `targetMinutes`, `plannedMinutes` and `runMinutes`, which is saved at most every 5 minutes. `date` may be a day that's over, if it hasn't planned since. |

## Setup

`POST /v1/setup` runs a plugin's setup (OIK-276) and answers with its steps, as lines of JSON. The first line has only `session`, and says it has started. The rest are the plugin's steps, as the [plugin contract](plugin-contract.md#setup) has them: a `say`, an `ask` or an `open`, each with its `seq`, and last a `done`, with a `result` or an `error`. A `keep` is the host's to do, so the stream has only the secret's name as `kept`, once it's on disk, and never its value. A refused setup, such as one of a plugin that has none, is a `422` with an `error`, as a call is.

Answer each `ask` and `open` with `POST /v1/setup/reply`, naming the `session` and the step's `seq`: `answer` is what the person typed, or an `open`'s redirect query, such as `code=abc&state=xyz`. A reply is taken once, and only for a step that's waiting. An answer may be a secret, so it's never logged.

A plugin runs one setup at a time, so a second ends the first, whose stream ends with a `done` saying so. Closing the stream ends the setup, and so does 10 minutes with no step and no answer. `api.SetupStep` has the stream's lines: fields are added, never renamed.

## Over TLS

The host's certificate is self-signed, and a client trusts it by its pin: `sha256:` and the hex SHA-256 of the certificate's `SubjectPublicKeyInfo`. TLS 1.3 only. Every call needs `Authorization: Bearer <token>`, which is checked before any body is read. A missing, wrong or revoked token gets a `401`, and counts in `oikade_api_auth_failures_total`. `POST /v1/clients/add` is refused over TLS, so a token can't make more of them.

`POST /v1/login` is the one call that takes no token. It trades a code from `clients/add` for a new client's token. A code works once, for 10 minutes, and only in memory, so a host restart ends it. A wrong, used or expired code gets a `422` that doesn't say which, and counts as an auth failure.

## Answers

| Status | Means |
|---|---|
| `200` | It worked. |
| `400` | The body isn't JSON, or is over 16 MiB. |
| `401` | Over TLS, the call has no valid token. |
| `404` | No such route, which includes the wrong method. `error` says which API this host speaks. |
| `422` | It didn't do what was asked, and `error` says why: the host refused it, or it failed there. The other fields can still say what happened, such as `restored` on a deploy whose version failed and was rolled back, and `findings` and `needsAllowSafety` on a refusal. |
| `500` | The event log can't be read. |

An answer is `application/json`, and a failure's is a `Response` with `error` set.

## Streams

These answer `application/x-ndjson`, one JSON object per line. A reader that takes more than 5 seconds over a line is cut off.

- **`GET /v1/tail`** sends an event with no `type`, which says the host is listening, then an `Event` as each thing happens, until the caller goes or the host stops. A tail that falls behind is cut off. With `since`, the log's events from then come first, before the event with no `type`, and each event comes once, whether the log had it or it happened as the tail began.
- **`GET /v1/events`** sends an `EventLog`, then an `Event` for each thing the log holds, oldest first. The `EventLog` has `now`, the host's `zone` and `offset`, `start`, when the oldest event the log holds is from, whatever the filter, and `baseline`, each device's values before that. The last line has the type `end`, and a `message` if it didn't get to the end, such as `the client was revoked`, so a cut-off read isn't taken for a short one. A host that stops partway through ends the stream with no `end` line, as one that crashed can't send it, and `oikade replay` says the host stopped.

Both take a filter in the query, and the host does the filtering, so a caller doesn't read the whole log to show one device (OIK-355). With no query, each sends everything, as `oikade replay` needs.

| Parameter | Lets through |
|---|---|
| `since`, `until` | Events from and to these times, both included, as RFC 3339, such as `2026-11-18T03:00:00Z`. A tail has no `until` |
| `ref`, as often as you like | Events about any of the [refs](devices.md#refs) given |

| A `ref` | Its events |
|---|---|
| `device/porch-light` | Everything about the device and its capabilities, under the names it had before a deploy renamed it: the log's `rename` events are followed back. A name is its only from the rename that gave it to the one that took it away, as another device may have had it before, or have it since. A tail carries on with a device that's renamed as it follows. A name nothing has now is the device that last had it, up to the rename that took it away |
| `device/porch-light/on` | The capability's, and what happened to the device as a whole: it went offline, or was forgotten or renamed |
| `room/hall`, `room/hall/on` | The same, for each device in the room now, not those that were in it then |
| `case/porch-light/on/dusk`, `flow/morning` | The rule's runs, pauses and outside calls, and the writes it made (`by`). A case's are found under each name its device had |
| `plugin/overkiz` | The plugin's own problems |
| `matter` | The bridge's events, and each controller's writes, refused or not |

The home's own events, `process` and `deploy`, pass whatever the refs, because they're often why the rest happened. A ref of any other kind, one that doesn't parse, a time that isn't one, and an `until` before its `since` are refused with a `422` that says why. A room, device, rule or plugin the host has no record of isn't refused: it has the home's own events and whatever the log holds of it.

An `Event`'s `type` is one of:

| `type` | What happened | Fields |
|---|---|---|
| `change` | A device reported a new value | `device`, `capability`, `value`, `previous`: the value before, if it had one |
| `write` | The host wrote to a device, whether or not the device took it. `by` is the ref of whose write it is: the rule whose claim won or whose step set it (`case/porch-light/on/dusk`, `flow/morning`), `matter` for a Matter controller's, such as a Home app tap, or `command` for `oikade set`'s. It has no `by` when no rule's claim decided it, as when arbitration puts a capability back to its default. A `change` with no write before it was made at the device, by its switch or its maker's app. `oikade replay` counts a write only if its `by` is a rule's or it has none | `device`, `capability`, `value`, `by` |
| `refused` | A controller's write, such as a Home app tap, that a claim turned away. The device wasn't written, and `oikade replay` doesn't count it as a write | `device`, `capability`, `value`, `message`: which claim holds it, and at what level |
| `rule` | A rule's run ended | `rule`, `result` |
| `pause` | A rule was paused or resumed | `rule`, `message` |
| `call` | A flow made an outside call, which a replay answers rather than makes | `rule`: the flow, `message`: the call, `value`: its result, `result`: its error |
| `availability` | A device went offline, or came back. Not each report of the same, nor a device first heard of online. The host doesn't keep how a device was, so one that's offline when the host starts is said to have gone offline then | `device`, `message`: `offline` or `online`, `result`: its driver's problem with it as it went offline, such as a missing LAN key, if it has one |
| `plugin` | A plugin's own problem started, changed or cleared, such as a refused login. A plugin that's down says nothing of its problem, so going down doesn't clear one, and coming back with the same one doesn't start it again. A host that starts again says a problem still there again | `ref`: the plugin's, `message`: the problem, or none once it has cleared |
| `matter` | The Matter bridge's commissioning window opened or closed, or a controller paired or left. A window has opened once it's advertised, and one still open when the host stops closes then. The bridge says each once it has advertised the change, which can be a second or two after | `message`: `window opened`, `window closed`, `controller paired` or `controller left`, `value`: the controller's fabric |
| `forget` | A device was forgotten, so its history stops here | `device` |
| `rename` | A deploy renamed a device, or gave its hardware the name your home declares | `device`: its name from then on, `previous`: its name before |
| `process` | Your process connected or disconnected | `message` |
| `deploy` | A new version took over | `message`: the version |
| `end` | The stream ended: the event log's last line, or the host ending a tail | `message`: why, if it stopped short |

Every event but `process`, `deploy` and `end` has a `ref`, the ref of what it's about: a capability's, a device's, a rule's or a plugin's, or `matter` for the bridge. `oikade replay` reads `change`, `write`, `call`, `process`, `deploy` and `pause`, and ignores the rest.
