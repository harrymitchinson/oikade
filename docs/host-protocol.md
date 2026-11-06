# Host protocol

How your process talks to the host: a Unix socket in the data directory, `host.sock`, carrying one JSON message per line. The host creates it with mode `0600`, and whoever can reach it can run the home. It has no network listener. `docs/architecture.md` says what each side owns, and [OIK-143](https://linear.app/oikade/issue/OIK-143) holds the design.

Nobody writes this protocol by hand. `framework.Run` speaks it, and the code is `internal/host/wire`.

## Sessions

The host starts your binary as `<binary> automations`, with the socket's path in `OIKADE_SOCKET`. A process has 5 seconds to send its `hello`:

```json
{"type":"hello","version":1,"role":"automations","rules":["fan","leak-shutoff"]}
```

`version` comes first so that it can change. The host answers `welcome` or `rejected`, and closes the connection after `rejected`:

- **`rejected`** with an `error`: the version or role is wrong, or an automations process is already connected. Exactly one runs at a time, because two would run everything twice.
- **`welcome`** with `devices`, every device with its room, capabilities, values and the capabilities the host arbitrates; `holds`, the saved state of each named rule that has any; `fires`, when each named rule's timers were last accounted for; and `runs`, how many `run` messages follow straight after it. Each of those carries one unfinished flow run and its journal in `unfinished`. They're separate because a journal can reach 4 MiB, and a message 16 MiB.

The host streams from the `welcome` on. A change landing as the session opens can arrive twice, so a process ignores a value it already has.

## From the host

| Type | Fields | Means |
|---|---|---|
| `change` | `device`, `capability`, `value`, `previous`, `time` | A value changed |
| `discovered` | `discovered` (a device) | A device reported for the first time |
| `ping` | `id` | Answer with a `pong` with this `id` |
| `reply` | `id`, `error`, and what the request returns | Answers a request |

## From your process

Each request has an `id`, and gets a `reply` with the same one. A reply with `error` set failed. Requests are carried out in order.

| Type | Fields | Does | Reply |
|---|---|---|---|
| `claim` | `device`, `capability`, `claim` | Makes a claim on an arbitrated capability. The reply means it's on disk | `arbitrated`: false if it isn't, so the process writes it instead |
| `release` | `device`, `capability`, `source` | Ends a source's claim. The reply means it's on disk | `arbitrated`, as for `claim` |
| `releaseAll` | `source`, optional `below` | Ends a source's claims everywhere, or with `below` only those under that level (3 is Safety). The reply means it's on disk | |
| `set` | `device`, `capability`, `value` | Writes a capability | |
| `saveHold` | `rule`, `hold` | Saves a hold's state. The reply means it's on disk | |
| `deleteHold` | `rule` | Forgets a hold's state. The reply means it's on disk | |
| `saveFire` | `rule`, `time` | Saves when a rule's timers were last accounted for. The reply means it's on disk | |
| `journal` | `run`, `data` | Appends a journal entry. The reply means it's on disk | `seq`, its place in the run |
| `finish` | `run` | Forgets a run's journal. The reply means it's on disk | |
| `pong` | `id` | Answers a `ping`. It isn't a request, so it gets no reply | |

## Heartbeats

The host pings every fifth of the heartbeat timeout, which is 10 seconds by default. A process that doesn't connect, or doesn't answer for that long, gets `SIGKILL` and is replaced.

A process answers only once its engine's lock is free. A condition stuck in a loop holds that lock, so it stops the answers, and the process is replaced rather than left silently doing nothing.

## When a session ends

A failed `claim`, `release`, `releaseAll` or `journal` ends the session too, because what's on disk is then unknown, and the next process works it out from what is.

A process that loses the host exits at once, since it can't act. If a request isn't answered in 30 seconds, the session counts as lost.

The host keeps running. It restarts your binary after 1 second, doubling up to 30 seconds, and goes back to 1 second once a process has lasted a minute. It waits for the old process to go and ends its session before starting the next one.

A process that falls 4096 messages behind is disconnected, and its replacement starts from a fresh `welcome`.

## Claims when a process goes away

Claims live in the host, so nothing happens to them when your process dies. Each ends by its own rule, whether or not a process is running:

| Claim | Ends |
|---|---|
| `for` | When its time is up |
| `until` next | When the value without the claim next changes |
| `until` released | At a release, so it waits for a process to make one |
| A hold's | When the hold ends, so it waits for a process to work that out |

A Safety hold's claim therefore stays in force while there's no process. A hold whose condition ended meanwhile is ended by the next process, once.
