# The host API

This is how the `oikade` command talks to a running host. It's HTTP on `api.sock` in the data directory, a Unix socket with mode `0600`, so only the host's user can reach it. `oikade --host` reaches the same socket over SSH. It's apart from `host.sock`, which only your process uses and which [`host-protocol.md`](host-protocol.md) describes. [OIK-171](https://linear.app/oikade/issue/OIK-171) says why it's HTTP.

## Calls

A `GET` takes no body. A `POST` takes a JSON object, and every field is optional at this level: a call that needs one refuses without it. `internal/host/api` has the fields as `Request`, and the answers as `Response`.

| Route | Fields | Answers with |
|---|---|---|
| `GET /v1/info` | | `goos`, `goarch` |
| `GET /v1/status` | | `status` |
| `GET /v1/matter` | | `matter` |
| `GET /v1/devices` | | `registry` |
| `GET /v1/secrets` | | `secrets`, never their values |
| `POST /v1/deploy` | `binary`, `allowSafetyRemoval` | `version`, `unchanged`, `restored`, `findings` |
| `POST /v1/rollback` | `allowSafetyRemoval` | `version`, `findings` |
| `POST /v1/set` | `device`, `capability`, `value` | |
| `POST /v1/pause`, `POST /v1/resume` | `rule`, `allowSafety` | `needsAllowSafety` |
| `POST /v1/backup` | | `backup`, the file it wrote on the host |
| `POST /v1/secrets/set` | `secret`, `secretValue` | |
| `POST /v1/secrets/remove` | `secret` | |
| `POST /v1/devices/name` | `device`, `to`, `allowSafety` | `needsAllowSafety` |
| `POST /v1/devices/room` | `device`, `room` | |
| `POST /v1/devices/reserve` | `device`, `room` | |
| `POST /v1/devices/remove` | `device`, `allowSafety` | `needsAllowSafety` |
| `POST /v1/devices/replace` | `device`, `to`, `allowSafety` | `needsAllowSafety` |
| `POST /v1/devices/restore` | `registry`, `force` | `moved`, `needsForce` |

A call has 5 seconds to send its body. Secret values go only in a body, and never in a path or a log.

**A call outlives its connection.** Once the host has a request it finishes it, so a deploy that has begun goes on after the command that asked is killed. The answer then has nowhere to go, and `oikade status` shows how it ended.

## Answers

| Status | Means |
|---|---|
| `200` | It worked. |
| `400` | The body isn't JSON, or is over 16 MiB. |
| `404` | No such route, which includes the wrong method. `error` says which API this host speaks. |
| `422` | It didn't do what was asked, and `error` says why: the host refused it, or it failed there. The other fields can still say what happened, such as `restored` on a deploy whose version failed and was rolled back, and `findings`, `needsAllowSafety` and `needsForce` on a refusal. |
| `500` | The event log can't be read. |

An answer is `application/json`, and a failure's is a `Response` with `error` set.

## Streams

These answer `application/x-ndjson`, one JSON object per line. A reader that takes more than 5 seconds over a line is cut off.

- **`GET /v1/tail`** starts with an event with no `type`, which says the host is listening, then sends an `Event` as each thing happens, until the caller goes or the host stops. A tail that falls behind is cut off.
- **`GET /v1/events`** sends an `EventLog`, then an `Event` for each thing the log holds, oldest first. The last line has the type `end`, and a `message` if it didn't get to the end, so a cut-off read isn't taken for a short one.
