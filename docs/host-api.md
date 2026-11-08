# The host API

This is how `oikade` talks to a running host. It's HTTP on `api.sock` in the data directory, a Unix socket with mode `0600`, so only the host's user can reach it. With `oikade-host --api-addr`, the same routes are served over TLS too, for clients with a token. It's apart from `host.sock`, which only your process uses and which [`host-protocol.md`](host-protocol.md) describes. [OIK-171](https://linear.app/oikade/issue/OIK-171) says why it's HTTP.

## Calls

A `GET` takes no body. A `POST` takes a JSON object, and every field is optional at this level: a call that needs one refuses without it. `internal/host/api` has the fields as `Request`, and the answers as `Response`.

| Route | Fields | Answers with |
|---|---|---|
| `GET /v1/info` | | `goos`, `goarch` |
| `GET /v1/status` | | `status` |
| `GET /v1/matter` | | `matter` |
| `GET /v1/devices` | | `registry`; `unimported`, the devices of plugins the running version doesn't import; `outsidePlugins`; `safetyHeld`, the plugins' devices a Safety claim holds |
| `GET /v1/secrets` | | `secrets`, never their values |
| `POST /v1/deploy` | The binary itself, as `application/octet-stream`, and `?allowSafetyRemoval=true` in the query | `version`, `unchanged`, `restored`, `findings`; `unimported`, the devices of plugins the version doesn't import, once it's running |
| `POST /v1/rollback` | `allowSafetyRemoval` | `version`, `findings`, `unimported` |
| `POST /v1/set` | `device`, `capability`, `value` | `outcome`, the capability with its claims once the device answered and nothing wrote it back, after up to 3 seconds; none if the device hadn't answered |
| `POST /v1/pause`, `POST /v1/resume` | `rule`, `allowSafety` | `needsAllowSafety`; a pause's `ended`, the claims it ended, and `kept`, its Safety claims, each as `device capability` |
| `POST /v1/backup` | | `backup`, the file it wrote on the host |
| `POST /v1/secrets/set` | `secret`, `secretValue` | |
| `POST /v1/secrets/remove` | `secret` | |
| `POST /v1/devices/name` | `device`, `to`, `allowSafety` | `needsAllowSafety` |
| `POST /v1/devices/room` | `device`, `room` | |
| `POST /v1/devices/type` | `device`, `deviceType`, empty to go back to its driver's, `allowSafety` | `needsAllowSafety` |
| `POST /v1/devices/reserve` | `device`, `room` | |
| `POST /v1/devices/remove` | `device`, or `plugin` for every device of that plugin, and `allowSafety` | `removed`, `needsAllowSafety` |
| `POST /v1/devices/replace` | `device`, `to`, `allowSafety` | `needsAllowSafety` |
| `POST /v1/devices/restore` | `registry`, `force` | `moved`, `needsForce` |
| `GET /v1/clients` | | `clients`, never their tokens |
| `POST /v1/clients/add` | `client` | `code`, `expires`, `pin` and `address`. Only on the socket. |
| `POST /v1/clients/add` with `token: true` | `client` | `token`, shown only once, and `pin` |
| `POST /v1/clients/revoke` | `client` | |
| `POST /v1/login` | `code` | `token`, shown only once, and `client` |

A call has 5 seconds to send its body, except a deploy, which may send its binary for as long as it keeps sending: up to 30 seconds with nothing, and 1 GiB in all. The host keeps the upload in `uploads` in its data directory until the deploy has copied it, and an upload that's cut off, or a host that stops during one, leaves nothing there and deploys nothing. Secret values go only in a body, and never in a path or a log.

**A call outlives its connection.** Once the host has a request it finishes it, so a deploy that has begun goes on after the command that asked is killed. The answer then has nowhere to go, and `oikade status` shows how it ended.

## Status

`GET /v1/status` answers with what `oikade status --json` prints: `version`, `deploying`, `process`, `since` (when your automations went down), `devices` and `rules`. Fields are added, never renamed. Each rule has its `name`, `kind` (`hold`, `moment` or `flow`, or none for a paused rule no running code names), `safety` and `paused`. A hold that isn't paused also has its state, from what it last saved ([OIK-215](https://linear.app/oikade/issue/OIK-215)):

| Field | Means |
|---|---|
| `active` | Whether its start is written. Always there for a hold. |
| `since` | When it last became active. Missing while it isn't. |
| `onAt`, `offAt` | When its on-delay or off-delay ends. Missing when none is running. One in the past ran out while it was stopped. |
| `latched` | A latched hold, from its start until a release. |
| `stopped` | Why it can't tell whether it should be active, so it keeps its state, such as `no device in room hall has reported occupancy`, or why its budget couldn't be planned. Only a connected process says. |
| `budget` | A budget hold's day: `date`, `targetMinutes`, `plannedMinutes` and `runMinutes`, which is saved at most every 5 minutes. `date` may be a day that's over, if it hasn't planned since. |

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
| `422` | It didn't do what was asked, and `error` says why: the host refused it, or it failed there. The other fields can still say what happened, such as `restored` on a deploy whose version failed and was rolled back, and `findings`, `needsAllowSafety` and `needsForce` on a refusal. |
| `500` | The event log can't be read. |

An answer is `application/json`, and a failure's is a `Response` with `error` set.

## Streams

These answer `application/x-ndjson`, one JSON object per line. A reader that takes more than 5 seconds over a line is cut off.

- **`GET /v1/tail`** starts with an event with no `type`, which says the host is listening, then sends an `Event` as each thing happens, until the caller goes or the host stops. A tail that falls behind is cut off.
- **`GET /v1/events`** sends an `EventLog`, then an `Event` for each thing the log holds, oldest first. The last line has the type `end`, and a `message` if it didn't get to the end, such as `the client was revoked`, so a cut-off read isn't taken for a short one. A host that stops partway through ends the stream with no `end` line, as one that crashed can't send it, and `oikade replay` says the host stopped.

An `Event`'s `type` is one of:

| `type` | What happened | Fields |
|---|---|---|
| `change` | A device reported a new value | `device`, `capability`, `value`, `previous` |
| `write` | Your automations wrote to a device. A person's write shows up as the `change` it makes | `device`, `capability`, `value` |
| `refused` | A controller's write, such as a Home app tap, that a claim turned away. The device wasn't written, and `oikade replay` doesn't count it as a write | `device`, `capability`, `value`, `message`: which claim holds it, and at what level |
| `rule` | A rule's run ended | `rule`, `result` |
| `pause` | A rule was paused or resumed | `rule`, `message` |
| `call` | A flow made an outside call, which a replay answers rather than makes. Only in the event log, not a tail | `rule`: the flow, `message`: the call, `value`: its result, `result`: its error |
| `process` | Your process connected or disconnected | `message` |
| `deploy` | A new version took over | `message`: the version |
| `end` | The stream ended: the event log's last line, or the host ending a tail | `message`: why, if it stopped short |
