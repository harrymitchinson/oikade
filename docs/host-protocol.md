# Host protocol

How your process talks to the host: a Unix socket in the data directory, `host.sock`, carrying one JSON message per line. The host creates it with mode `0600`, and whoever can reach it can run the home. It has no network listener. `docs/architecture.md` says what each side owns, and [OIK-143](https://linear.app/oikade/issue/OIK-143) holds the design.

Nobody writes this protocol by hand. `framework.Run` speaks it, and the code is `internal/host/wire`.

## Sessions

The host starts your binary as `<binary> automations`, with the socket's path in `OIKADE_SOCKET`. A process has 5 seconds to send its `hello`:

```json
{"type":"hello","version":2,"role":"automations","rules":["fan","leak-shutoff"]}
```

`version` comes first so that it can change, and it rises with any change to what a message means, because a host outlives a deploy and can meet a binary built against something else. The host refuses any other version, which it says in its log, so a process from before `below` existed can never reach `releaseAll`. The host answers `welcome` or `rejected`, and closes the connection after `rejected`:

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

The host keeps running. Going away during a deploy's settle period is different: see [Deploys](#deploys). Otherwise it restarts your binary after 1 second, doubling up to 30 seconds, and goes back to 1 second once a process has lasted a minute. It waits for the old process to go and ends its session before starting the next one.

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

A rule's claims end with the rule, but only through a deploy, which is what checks for a Safety rule going. See [Deploys](#deploys). A binary changed some other way ends nothing.

## Check mode

The host runs a binary as `<binary> check` before it stops anything. That loads the home with no writes and no session, and prints one line of JSON as its last line of output, then exits 0:

```json
{"protocol":2,"rules":[{"name":"leak-shutoff","kind":"hold","safety":true,"latch":true}]}
```

A failure, or a `protocol` other than the host's, rejects the version.

## Deploys

`oikade deploy` and `oikade rollback` talk to the host over `api.sock`, also in the data directory and also mode `0600`, one JSON request and one JSON response per connection. `internal/host/api` is the code. A deploy goes in this order, and any failure leaves or brings back the last version:

1. The host copies the binary to `versions/<id>` in the data directory. The ID comes from its contents, so deploying what's running changes nothing.
2. It runs check mode, and compares the result with the running version. A Safety rule that's gone, or a rule that's gone and holds a Safety claim, refuses the deploy unless it says `allowSafetyRemoval`. A rename counts, because a hold's latch lives under its name.
3. It records the version as unconfirmed, along with every rule that has anything on record, sends the old process `SIGTERM`, kills it after 5 seconds if it's still there, and ends its session. It reads the claims again, because the old process could claim until it stopped, and a new finding brings the old version back unless it said `allowSafetyRemoval`. Only then does the new process start, so two never overlap. Waiting runs keep their journals and resume.
4. It waits for the new process to connect, then for the settle period, 5 seconds by default. If the process goes away at any point before that's up, the last version comes back at once, and the command says why.
5. It confirms the version, keeps the five latest, and ends what's left of rules the version doesn't have: their claims, Safety ones included, their saved state and the runs of their flows, because nothing else would. The record says this is due, so a host that dies first does it when it starts.

A host that dies during steps 3 or 4 finds the unconfirmed record when it starts, and goes back to the last version. A version that failed goes the same way: once the last version is back, what its own rules left ends, because no code is left to end it. Rules on record in step 3 are spared, so it can only end what appeared after that.

If there's no version to go back to, or the one that comes back has rules the host can't read, nothing ends: what the failed version left stays until the next confirmed deploy.

A process never ends a rule or a flow it doesn't have. Without its flow, a binary leaves a run alone, with its journal and claims, and the run carries on if a later binary has the flow. Only step 5 ends it. So a failed version, or a binary changed some other way, ends no rule and no flow.

A version that settled and fails later restarts as usual, and stays until a rollback.

`rollback` is a deploy of the version before the running one, on the same terms, and it forgets the running one, so a second rollback goes back further.

Going back restores a version, not everything it did. A version with a changed flow has already replayed that flow's waiting runs, so a run it dropped stays dropped, as do its non-Safety claims, and steps it took stay in the run's journal. The same goes for a binary changed some other way. If those don't match the version that comes back, that run is dropped too. Its claims and rules' saved state are otherwise as they were.
