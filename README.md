# llama-slot-proxy

A small, dependency-free Go proxy that sits in front of
[llama-server](https://github.com/ggml-org/llama.cpp) and makes its prompt cache
reusable **across new sessions** and **across server restarts**.

It does this by driving llama.cpp's own `--slot-save-path` / slot save+restore
API. The proxy never touches the KV cache itself and never rewrites your
requests — it only asks the server to restore a previously saved state before
forwarding.

```
client ──► llama-slot-proxy :8080 ──► llama-server :8081
                    │
                    └── shares the state directory with --slot-save-path
```

---

## The problem

Agent clients resend the whole conversation on every turn: a long system
prompt, the full tool schema, and the entire history. llama-server already
reuses a matching prefix from its slot, so *continuing* a conversation is
cheap. What is expensive is everything else:

| Situation | What llama-server does on its own |
|---|---|
| **New session**, same system prompt | prefills the whole shared prefix again |
| **Switching between sessions** | the other session's state was evicted |
| **After a server restart** | the slot is empty again |

With a ~36 KB agent system prompt that means re-processing thousands of tokens
every time a new conversation starts, and re-processing the *entire* history if
a long session's state is lost.

## What this proxy does

Two kinds of state are cached, both as ordinary llama.cpp state files:

* **Seed** — the state after pre-encoding just the system message (+ tool
  schema), and nothing else. It is a strict prefix of *every* conversation that
  shares that system prompt, so it is valid for all of them, and it is keyed by
  a hash of `system + tools` so a changed system prompt simply produces a
  different seed.
* **Session snapshot** — the state at the end of a particular conversation.
  Valid only for that conversation, because a later turn is a superset of an
  earlier one.

On each request the proxy decides, from cheapest to most expensive:

1. **Auxiliary request?** (tiny system prompt, e.g. title generation) → plain
   passthrough, never touch the cache.
2. **Is this session already the resident one?** → forward, no restore at all.
   llama-server's own prefix matching handles the continuation.
3. **A session snapshot exists and is deeper than the seed?** → restore it.
4. **A seed exists?** → restore it.
5. **Nothing yet?** → build the seed (one prefill), then forward.

## Measured results

On the development machine (Ryzen AI MAX+ 395, Radeon 8060S, 61 GiB unified
memory; Qwen3-class 27B hybrid model, 4-bit weights, `q8_0` KV, 256K context,
single slot), serving a real ~104K-token agent conversation:

| | tokens prefilled | wall time |
|---|---|---|
| **Cold**, no cache at all | 103,928 | **608.6 s** |
| Same conversation, next turn | 19 | **4.8 s** |
| Same conversation, **after restarting llama-server** | 18 | **5.0 s** |

A new session sharing the system prompt, at ~13.8K tokens of context:

| | tokens prefilled | wall time |
|---|---|---|
| No cache | 13,789 | 52.8 s |
| Seed restored | 5,604 | **22.9 s** |

Restore itself is cheap: a 640 MB state file restores in ~130 ms, and on this
machine the state size follows

```
state_bytes ≈ 149.63 MiB + 34.0 KiB × n_tokens
```

so a full 256K context is roughly 8.65 GiB per state file. Plan your `-max-bytes`
accordingly.

## Requirements

* A llama.cpp build with the `--slot-save-path` server option and the
  `/slots?action=save|restore` endpoints (present in upstream for a long time).
* **llama-server must write to the same directory the proxy reads.** The proxy
  passes only a *filename* to the server, which resolves it relative to its own
  `--slot-save-path`. So `-cache-dir` must be the same path on both sides —
  same host, same mount namespace, same string.
* One slot (`-np 1`) is assumed. The proxy serializes cache work behind a single
  lock, because a restore overwrites whatever is resident.
* Nothing else. No cgo, no third-party modules, no build service.

## Quick start

Start llama-server with a slot-save-path (tmpfs recommended — see below):

```bash
llama-server -m model.gguf -c 32768 -np 1 \
  --slot-save-path /dev/shm/llama-slot-proxy
```

Start the proxy in front of it, and point your client at the proxy:

```bash
llama-slot-proxy \
  -listen 0.0.0.0:8080 \
  -upstream 127.0.0.1:8081 \
  -cache-dir /dev/shm/llama-slot-proxy \
  -persist-dir /var/lib/llama-slot-proxy
```

Try it once with `-dry-run -v` first: that turns the proxy into a pure
passthrough that still logs the `cache_n` / `prompt_n` of every upstream
response, which is the quickest way to see what your workload would save.

### Build

```bash
make build          # or: go build -o llama-slot-proxy .
```

Requires Go 1.21+. There are no dependencies outside the standard library.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `0.0.0.0:8080` | Address to listen on. |
| `-upstream` | `127.0.0.1:8081` | llama-server address. |
| `-cache-dir` | `/dev/shm/llama-slot-proxy` | Working dir. **Must equal** llama-server's `--slot-save-path`. |
| `-persist-dir` | `""` | Durable dir: receives a copy on exit and holds evicted snapshots. Empty = keep everything in `-cache-dir`. |
| `-max-bytes` | 16 GiB | Cap for `-cache-dir`. |
| `-durable-bytes` | 32 GiB | Cap for `-persist-dir`. |
| `-max-age` | 24h | Age after which a session snapshot is discarded. |
| `-max-seeds` | 4 | How many distinct seeds to keep. |
| `-session-header` | `Session_id` | Request header carrying the session id. `X-Client-Request-Id` is tried as a fallback. |
| `-min-system-bytes` | 2000 | Requests with a shorter system message are never cached (filters out auxiliary requests). 0 = cache everything. |
| `-require-stream` | false | Also require `stream: true` to cache a request. |
| `-save-every-tokens` | 8000 | Save a session snapshot after this much new content. 0 = save only on slot handover. |
| `-save-every-msgs` | 10 | Save after this many new messages. 0 = disable. |
| `-lock-wait` | 120s | Max wait for the slot lock; on timeout the request is forwarded uncached. |
| `-log` | `""` | Log file; empty means stderr. |
| `-dry-run` | false | Never touch the cache (pure passthrough). |
| `-v` | false | Verbose logging. |

## How the save policy works

Saving a state file is not free (hundreds of MB), so the proxy does **not** save
on every request:

* **Strategy A — save on handover.** When a different session is about to take
  the slot, the outgoing session is saved first. This is the only moment its
  state can still be captured, and it is the reason steady-state turns cause
  **zero** disk writes.
* **Strategy B — save on content growth.** A session that keeps the slot saves
  once its prompt has grown by `-save-every-tokens` tokens or
  `-save-every-msgs` messages. Growth is measured in content, never on a timer,
  so idle time costs nothing and bursts are still captured.
* **Retirement.** A snapshot that is restored but *not* actually reused is worse
  than no snapshot, because the restore destroys the checkpoints llama-server
  would otherwise have rolled back to. After two consecutive misses the
  snapshot is dropped. Snapshots that a seed already covers are dropped as well.

Nothing is preloaded at startup: a snapshot is pulled back from `-persist-dir`
only when a request needs it.

## tmpfs + durable tiering

State files are large and rewritten often, so writing them straight to disk is
slow and write-amplifying. Measured on the development machine:

| medium | 2 GB write |
|---|---|
| tmpfs (`/dev/shm`) | 0.25 s (8.7 GB/s) |
| ext4 on a loop device | 2.07 s (1.0 GB/s) |

So the working directory lives on tmpfs and `-persist-dir` is a durable,
write-behind copy: refreshed on graceful exit, and used to park snapshots that
are evicted from the working set. Only files whose mtime advanced are copied,
and every copy is written to `*.tmp` then renamed, so an interrupted flush can
never corrupt a snapshot.

`SIGTERM`/`SIGINT` triggers a drain (wait for in-flight requests, so the slot
isn't captured mid-generation), then the flush. `SIGKILL` and power loss will
lose whatever had not been flushed.

## Configuration example

```bash
llama-slot-proxy \
  -listen 0.0.0.0:8080 \
  -upstream 127.0.0.1:8081 \
  -cache-dir /dev/shm/llama-slot-proxy \
  -persist-dir /var/lib/llama-slot-proxy \
  -max-bytes $((5*1024*1024*1024)) \
  -durable-bytes $((24*1024*1024*1024)) \
  -save-every-tokens 8000 \
  -save-every-msgs 10 \
  -lock-wait 120s \
  -max-seeds 4 \
  -min-system-bytes 2000 \
  -v
```

A minimal systemd unit:

```ini
[Unit]
Description=llama-slot-proxy
After=network.target llama-server.service

[Service]
ExecStart=/usr/local/bin/llama-slot-proxy -cache-dir /dev/shm/llama-slot-proxy -persist-dir /var/lib/llama-slot-proxy -log /var/log/llama-slot-proxy.log
Restart=always
KillSignal=SIGTERM
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
```

## Caveats

Read these before deploying.

* **The client must resend history verbatim.** Everything here rests on the
  saved sequence being a strict prefix of the next prompt. A client that
  truncates, summarises or rewrites earlier turns will simply miss the cache.
  That is safe (the server falls back to normal prefix matching) and the proxy
  retires repeatedly-missing snapshots, but it does mean no benefit.
* **A shorter prompt under a reused session id falls back to the seed.** If a
  client reuses one session id for a new conversation, the old conversation's
  snapshot is deeper than the new prompt and would lose the prefix test. The
  proxy detects this from the recorded message count and uses the seed instead,
  so the new conversation still starts warm.
* **State files are parsed for their header.** The proxy reads the first 12
  bytes (magic, version, token count) to compare candidates without restoring
  them. A change to that on-disk format in llama.cpp would make the proxy treat
  files as absent — a graceful degradation, not a crash, but the cache would
  stop working until the header parsing is updated.
* **A snapshot's size is sanity-checked** against `149.63 MiB + 34.0 KiB ×
  n_tokens`, because a save that ran out of space leaves a truncated file whose
  header still claims a full token count. Files far short of the estimate are
  ignored. The constants come from one hybrid model and are only used as a
  floor, not an exact check.
* **`-min-system-bytes` matters.** Many clients reuse a session id for small
  side requests (title generation and the like). Their tiny system prompt would
  build a useless seed and leave a shallow snapshot that poisons the real
  conversation on the next turn. The default filters them out.
* **One slot.** With `-np 1` a restore overwrites the resident state; the proxy
  serializes accordingly. Multiple slots are not modeled.
* **The proxy is not a cache for correctness — only for speed.** If every cache
  operation fails it forwards the request unchanged. There is no path where a
  cache failure changes a response.

## Interaction with llama.cpp context checkpoints

The proxy and llama.cpp's own context checkpoints (`-ctxcp`, `-cms`) solve
overlapping but different problems, and they compose:

* The proxy handles **whole-state reuse** — a new session, or a session whose
  slot was taken over, restarting from a saved prefix.
* Checkpoints handle **rolling back within a conversation** — for example when
  the tail of a prompt is replaced (a retry, an edit, a branch), where only a
  suffix can be reused instead of the whole thing.

Both are worth having. Note that a slot restore replaces the prompt state, so
the proxy deliberately avoids restoring when the session is already resident, in
order not to throw away the server's in-process checkpoints.

## Files

```
main.go      request handling, decision order, forwarding, config
cache.go     seeds, session snapshots, save policy, pruning, slot API client
persist.go   tmpfs work dir + durable copy, eviction, lazy pull-back
docs/DESIGN.md   the mechanisms behind the rules, with measurements
```

## License

MIT — see [LICENSE](LICENSE).
