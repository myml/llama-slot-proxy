# llama-slot-proxy

[中文](README.md) | **English**

**A memory layer for your local LLM: what has already been computed is not computed again, so a new conversation — or a restarted server — picks up where it left off.**

---

## The effect

With a local LLM, what hurts is usually not how fast it generates but how long you **wait** while it
reads everything you have sent so far from scratch. This proxy stores the state of what it has
already worked through, and reuses it next time.

| Situation | Without it | With it | |
|---|---|---|---|
| **Resuming a conversation after a server restart** | wait **10 minutes** | wait **5 seconds** | **122x faster** |
| **Starting a new conversation** (same system prompt) | wait **53 s** | wait **23 s** | **59%** less to compute |
| The next turn of the same conversation | 4.8 s | 4.8 s | already fast; the proxy stays out of the way |

> Numbers from a real agent conversation of about 104K tokens, on a Ryzen AI MAX+ 395 / Radeon
> 8060S, with a 27B hybrid model at 4-bit and a 256K context. Exact conditions under
> "Measurements" below.

**When it will not help**, so you can rule it out: if your conversations are short (a few hundred
tokens), or your client rewrites history on every turn, the saving is negligible. In that case the
proxy is essentially transparent — no downside, but no reason to install it either.

**That last table row matters too.** The proxy acts only when acting helps. While you keep asking
follow-ups in the same conversation it deliberately does *nothing*, because that path is already
fast and an extra step would only make it slower (see "When the proxy deliberately does nothing").

## Why it is slow

Every turn, the client resends the whole conversation: the system prompt, the tool definitions, and
the entire history. For an agent — a coding assistant, say — the system prompt alone can be tens of
thousands of characters, and a few dozen tool-call exchanges on top of that easily adds up to tens
or hundreds of thousands of tokens per request.

The good news is that llama.cpp caches the prefix it has already processed, so **the next turn of
the same conversation** is fast.

The bad news is that this cache lives in memory. Any one of the following throws it away and
everything has to be read again from the start:

* you **start a new conversation** — even if the system prompt is identical
* the **server restarts**, or you change a launch parameter
* you **switch back and forth between two conversations**, and one evicts the other

For a 100K-token conversation, "read it again from the start" means **ten-odd minutes of waiting**,
and all three of those things happen daily.

## How it works

It stores two kinds of computed state. Both are ordinary llama.cpp state files — the proxy only
asks the server to save and load them at the right moments.

* **Seed** — the state after processing *only the system prompt* (plus tool definitions), nothing
  else. Every conversation sharing that system prompt begins with it, so **one seed serves all of
  them**. It is keyed by a content hash, so a changed system prompt automatically becomes a new
  seed with no manual maintenance.

* **Session snapshot** — the state of one particular conversation at the point it reached. Valid
  for that conversation only, because its later turns are a continuation.

Each request is handled cheapest-first:

1. **Is it a small side request?** (a title generation, say) → pass it straight through, never
   touch the cache.
2. **Is this conversation's state already in memory?** → forward, do nothing. llama.cpp handles the
   rest.
3. **Is there a snapshot for this conversation, deeper than the seed?** → restore it.
4. **Is there a seed?** → restore it.
5. **Neither?** → build the seed first (one prefill), then forward.

## Measurements

Development machine: Ryzen AI MAX+ 395 / Radeon 8060S, 61 GiB unified memory; 27B hybrid model,
4-bit weights, `q8_0` KV, 256K context, single slot (`-np 1`). A real agent conversation of about
104K tokens:

| | tokens to recompute | wall time |
|---|---|---|
| **Cold**, no cache at all | 103,928 | **608.6 s** |
| Next turn of the same conversation | 19 | **4.8 s** |
| Same conversation, **after restarting llama-server** | 18 | **5.0 s** |

A new conversation sharing that system prompt, at about 13.8K tokens of context:

| | tokens to recompute | wall time |
|---|---|---|
| No cache | 13,789 | 52.8 s |
| Seed restored | 5,604 | **22.9 s** |

Restoring is cheap: a 640 MB state file reads back in about 130 ms. On this machine the state size
follows

```
state_bytes ≈ 149.63 MiB + 34.0 KiB × n_tokens
```

so a full 256K context is about 8.65 GiB per state file. Plan `-max-bytes` accordingly.

## When the proxy deliberately does nothing

This deserves its own section, because it decides whether the proxy helps or gets in the way.

llama.cpp already keeps the slot state between requests. So when a conversation's state is **already
in memory** and you are simply asking the next question, restoring again is pure waste:

* it spends time reading a few hundred MB of state back (about 0.13 s for 640 MB);
* worse, it **destroys llama.cpp's in-process context checkpoints** — which is what lets the server
  roll back cheaply when the tail of a prompt is replaced (a retry, an edit, a branch).

Measured over consecutive turns: both approaches process exactly the same number of tokens, but a
redundant restore adds about 0.4 s per turn. So the proxy's first fast path is "forward, touch
nothing". This is also why it composes with llama.cpp's own context checkpoints (`-ctxcp`, `-cms`)
rather than competing with them — see the last section.

## Requirements

* A llama.cpp build with the `--slot-save-path` option and the `/slots?action=save|restore`
  endpoints (upstream has had these for a long time).
* **llama-server must write to the same directory the proxy reads.** The proxy hands the server
  only a *filename*, which the server resolves against its own `--slot-save-path`. So `-cache-dir`
  must be the same path on both sides — same host, same mount namespace, same string.
* One slot (`-np 1`) is assumed. The proxy serializes cache work behind a single lock, because a
  restore overwrites whatever is resident.
* Nothing else. No cgo, no third-party modules, no build service.

## Quick start

Start llama-server with a slot-save-path (tmpfs recommended — see "Why tmpfs" below):

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

**Run it once with `-dry-run -v` first.** The proxy then degrades to a pure passthrough that still
logs the `cache_n` / `prompt_n` of every upstream response — the quickest way to see what your
workload would save, with no side effects at all.

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
| `-persist-dir` | `""` | Durable dir: gets a copy on exit and holds evicted snapshots. Empty = keep everything in `-cache-dir`. |
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

## When it saves

State files are large (hundreds of MB), so the proxy does **not** save on every request:

* **Save on handover.** When another conversation is about to take the slot, the outgoing one is
  saved first. This is the only moment its state can still be captured, and it is why
  steady-state turns cause **zero** disk writes.
* **Save on content growth.** A conversation that keeps the slot saves once its prompt has grown by
  `-save-every-tokens` tokens or `-save-every-msgs` messages. Growth is measured in content, never
  on a timer, so idle time costs nothing and bursts are still captured.
* **Retirement.** A snapshot that is restored but *not* actually reused is worse than no snapshot,
  because the restore destroyed the checkpoints llama.cpp would otherwise have rolled back to.
  After two consecutive misses it is dropped. Snapshots that a seed already covers are dropped as
  well — they take up room and buy almost nothing.

Nothing is preloaded at startup: a snapshot is pulled back from `-persist-dir` only when a request
needs it.

## Why tmpfs

State files are large and rewritten often, so writing them straight to disk is slow and
write-amplifying. Measured on the development machine:

| medium | 2 GB write |
|---|---|
| tmpfs (`/dev/shm`, i.e. RAM) | 0.25 s (8.7 GB/s) |
| ext4 on a loop device (i.e. disk) | 2.07 s (1.0 GB/s) |

So the working directory lives in tmpfs, and `-persist-dir` is a durable copy: refreshed on
graceful exit, and used to park snapshots evicted from the working set. Only files that changed are
copied, and every copy is written to `*.tmp` then renamed, so an interrupted flush can never
corrupt a snapshot.

On `SIGTERM`/`SIGINT` the proxy first waits for in-flight requests to finish (so the slot is not
captured mid-generation), then flushes. `SIGKILL` and power loss will lose whatever had not been
flushed yet — the inherent trade-off of using tmpfs.

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

* **The client must resend history verbatim.** Everything here rests on the saved sequence being a
  strict prefix of the next request. A client that truncates, summarises or rewrites earlier turns
  will simply miss the cache. That is safe (the server falls back to normal prefix matching and the
  proxy retires repeatedly-missing snapshots), but it does mean no benefit.
* **A shorter prompt under a reused session id falls back to the seed.** If a client reuses one
  session id for a new conversation, the old conversation's snapshot is deeper than the new request
  and would fail the prefix test. The proxy detects this from the recorded message count and uses
  the seed instead, so the new conversation is still fast.
* **State files are parsed for their header.** The proxy reads the first 12 bytes (magic, version,
  token count) to compare candidates without restoring them. If llama.cpp changed that on-disk
  format the proxy would treat files as absent — a graceful degradation, not a crash, but the cache
  would stop working until the header parsing is updated.
* **A snapshot's size is sanity-checked** against `149.63 MiB + 34.0 KiB × n_tokens`, because a save
  that ran out of space leaves a truncated file whose header still claims a full token count. Files
  far short of the estimate are ignored. The constants come from one hybrid model and are only used
  as a floor, not an exact check.
* **`-min-system-bytes` matters.** Many clients reuse a session id for small side requests (title
  generation and the like). Their tiny system prompt would build a useless seed and leave a shallow
  snapshot that poisons the real conversation on the next turn. The default filters them out.
* **One slot.** With `-np 1` a restore overwrites the resident state; the proxy serializes
  accordingly. Multiple slots are not modeled.
* **The proxy is about speed, not correctness.** If every cache operation fails it forwards the
  request unchanged. There is no path where a cache failure changes a response.

## Interaction with llama.cpp context checkpoints

The proxy and llama.cpp's own context checkpoints (`-ctxcp`, `-cms`) solve overlapping but
different problems, and they compose:

* The proxy handles **whole-state reuse** — a new conversation, or one whose slot was taken over or
  lost to a restart, restarting from a saved prefix.
* Checkpoints handle **rolling back within a conversation** — for example when the tail of a prompt
  is replaced (a retry, an edit, a branch), where only a suffix can be reused instead of the whole
  thing.

Both are worth having. This is also the second reason for the previous section: a slot restore
replaces the prompt state, so when a conversation is already resident the proxy deliberately does
not restore, in order not to throw away the server's in-process checkpoints.

## Files

```
main.go      request handling, decision order, forwarding, config
cache.go     seeds, session snapshots, save policy, pruning, slot API client
persist.go   tmpfs work dir + durable copy, eviction, lazy pull-back
docs/DESIGN.en.md   the mechanisms behind the rules, with measurements
```

## License

MIT — see [LICENSE](LICENSE).
