# Design notes

Why the proxy behaves the way it does. Almost every rule here exists because a
simpler version was tried first and measured to be wrong. Numbers are from the
development machine (Ryzen AI MAX+ 395 / Radeon 8060S, unified memory, 27B
hybrid model, `q8_0` KV, 256K context, `-np 1`) unless stated otherwise.

---

## 1. The prefix law

llama-server reuses a saved state only when the saved token sequence is a
**strict prefix** of the incoming prompt. It reports the reused length as
`cache_n`, so the condition is:

```
reuse  ⟺  cache_n == n_saved  and  saved_tokens is a prefix of prompt_tokens
```

Everything follows from this.

**Consequence for the seed.** A new session diverges from every other session
exactly where the system message ends — because the next thing in the prompt is
a user turn, and that differs per session. So a seed must stop *at* the system
message. A seed extended by even one user turn would have `n_saved` past the
divergence point of every other session, and would never be reused.

This is not "the seed must not contain user messages". A session snapshot
contains plenty of user messages and is reused fine, because for the *same*
conversation the next prompt is a superset of the saved one. The rule is about
where the divergence is, not about message roles.

**Consequence for staleness.** A snapshot containing generated tokens (a save
after a completed turn does) is only valid if the client resends that
generation in its next request. Clients that echo the assistant message —
including its reasoning/thinking field — satisfy this. Clients that drop or
rewrite it do not, and get no reuse.

## 2. Restore is overwrite semantics

A restore replaces the slot's prompt state. It is **not** additive. Two
restores in one request leave only the second one's state, so restoring the seed
after a deeper session snapshot silently discards the better state.

An early version of this proxy did exactly that, in the wrong order, and cost a
real session a 67-second full recompute while every log line said "restored ok".
The current code probes both candidates **without touching the slot** (it reads
the token count out of each state file's header) and restores only the deeper
one.

## 3. Don't restore when the state is already resident

llama-server keeps the slot's state between requests. Restoring for a session
that is *already* the resident one is therefore pure overhead:

* it costs a full state-file read and deserialize (~0.13 s for a 640 MB file);
* it **destroys the server's in-process context checkpoints**, which are what
  let the server roll back cheaply when a prompt's tail is replaced.

Measured on continuous turns: the same number of tokens are processed either
way, but a redundant restore adds roughly 0.4 s per turn. So the fast path is
"forward, don't touch anything", and the proxy tracks which session owns the
slot (verifying against the server's `/slots` each time, since the server is the
only source of truth).

## 4. Why a bad snapshot is worse than no snapshot

If a snapshot is restored but not actually reused, the request pays *both* the
restore and a full recompute — and it has thrown away the checkpoints the server
would otherwise have used.

So the proxy counts consecutive non-reuses per session and retires a snapshot
after two of them. It would rather fall back to the server's own prefix matching
than keep forcing a losing restore.

## 5. When a restore is silently discarded

This one is subtle and cost a lot of debugging time.

After a restore, llama-server decides whether to even *look* for a checkpoint
using (in `server-context.cpp`):

```cpp
const bool has_new_tokens = (n_past < n_prompt_in);
const auto pos_min_thold = std::max(0, pos_next - n_swa - (has_new_tokens ? 0 : 1));

if (n_past > 0 && n_past <= slot.prompt.n_tokens()) {
    const auto pos_min = llama_memory_seq_pos_min(memory, slot.id);
    if (pos_min >= pos_min_thold) {
        // search the checkpoint list for a usable entry; if none -> full recompute
    }
}
```

A restore installs the saved state with `pos_min = n_saved - 1`. So:

| incoming prompt | `has_new_tokens` | `pos_min_thold` | `pos_min` | outcome |
|---|---|---|---|---|
| **longer** than the snapshot | true | `n_past` | `n_past - 1` | `pos_min < thold` → no search → **reuse** |
| **not longer** than the snapshot | false | `n_past - 1` | `n_past - 1` | search runs, and a restored slot's checkpoint list is empty → **full recompute** |

Two practical notes:

* A request that re-sends a prompt no longer than what was saved does *not*
  benefit, and pays full price. Echoing a generation back is normally enough to
  stay ahead, but if a client appends very little after a long generation the
  saved state can end up ahead of the new prompt.
* The checkpoint list is empty after a restore because
  `server_prompt::clear()` clears `checkpoints` along with `tokens`, and
  `SLOT_RESTORE` calls it. The proxy's snapshots therefore carry no
  rollback capability of their own — that is what the server's in-process
  checkpoints are for, which is another reason not to disturb them (§3).

### 5.1 Guarding against a stale snapshot shadowing a valid seed

The candidate comparison in §2 picks the deeper of the two, but "deeper" is not
the same as "usable". If a client reuses one session id for a new conversation —
or simply restarts one — the previous conversation's snapshot is much deeper
than the new prompt, wins the comparison, and then fails the prefix test. The
result is a full recompute, and the seed that would have worked is never tried.

An earlier version of this proxy did exactly that: re-sending a 15-message
prompt under a session whose snapshot held 19 messages cost a 52-second full
recompute, while every log line said the restore had succeeded.

This is decidable without tokenizing anything, because the sidecar already
records how many messages the conversation had when the snapshot was written. A
snapshot taken from a longer conversation cannot be a prefix of a shorter
request, so it is skipped — and the file is *kept*, because the client may
return with the longer prompt.

| same session, 19-message snapshot, 15-message request | tokens prefilled | wall |
|---|---|---|
| without the guard | 13,789 (full recompute) | 52 s |
| with the guard | 5,604 (seed used) | 23 s |

## 6. Save policy: content growth, not time

State files are large. Restoring a 640 MB file takes ~0.13 s, and saving one
takes ~0.14 s on tmpfs; doing that on every request at a moderate request rate
works out to hundreds of GB per day of writes.

Saving on a timer is the obvious alternative and is wrong in both directions: it
wastes writes while the user is idle, and misses saves during a burst.

Instead the proxy saves on **content growth** — every `N` new tokens or `M` new
messages, both measured from the server's own `/slots` token count — plus on
**slot handover**, which is the only moment the outgoing session's state can
still be captured. A continuous conversation therefore writes roughly once per
`N` tokens rather than once per request, and a conversation that never loses the
slot writes nothing at all in steady state.

## 7. Retiring snapshots a seed already covers

A session snapshot that is barely deeper than the seed it shares a system prompt
with buys a few hundred tokens while costing a full-size file on disk and an
extra copy on every flush. The proxy compares the two and drops the snapshot
when it is not meaningfully deeper (same thresholds as §6) *and* the session is
young. When it cannot answer the question — no seed, no snapshot, no sidecar —
it keeps the file. Retaining too much is recoverable; deleting the wrong thing
is not.

## 8. tmpfs, and a durable behind it

The working directory must be the server's `--slot-save-path`, because the proxy
only ever passes a *filename*: the server resolves it relative to its own
configured path and does the I/O itself. Three consequences:

* the two processes must share a filesystem, at the same path;
* the proxy can read a state file's header locally, cheaply, to compare
  candidates without restoring them;
* there is no upload path — a proxy in a different container needs a shared
  mount, not a protocol.

Given that, the working set belongs on tmpfs and durability is a separate
concern:

| medium | 2 GB write |
|---|---|
| tmpfs (`/dev/shm`) | 0.25 s (8.7 GB/s) |
| ext4 on a loop device | 2.07 s (1.0 GB/s) |

`-persist-dir` gets a write-behind copy, refreshed on graceful exit and used to
park evicted snapshots. Restores from it are lazy — nothing is preloaded — and
every copy goes through `*.tmp` + `rename` so an interrupted flush leaves the
old file intact rather than a truncated new one.

**Symbolic links instead of copies do not work here.** A symlink to the durable
copy restores fine (zero-copy, ~5x faster), but the *save* path writes through
it straight to disk, which reintroduces exactly the slow, write-amplifying
behaviour the tiering exists to avoid.

## 9. Two failure modes that cost real debugging time

**Pruning hung off the save's success.** The code pruned evicted snapshots only
after a successful save. Once tmpfs filled up, the save failed, so pruning never
ran, so nothing was ever freed — a permanent stall. Pruning now happens *before*
the write, reserving space for the snapshot about to be created, and again on
failure.

**Truncated files lie.** A save that runs out of space leaves a partial file
whose header still reports the full token count. It looks like the *deepest*
candidate and is only discovered to be broken at restore time. The proxy now
compares each file's size against the size law (`149.63 MiB + 34.0 KiB ×
n_tokens`) and treats anything far short as absent, and it removes the partial
file when a save fails.

## 10. State file format

The proxy reads, but never writes, llama state files. The header is:

```
u32 magic
u32 version
u32 n_token_count
```

followed by the token array and the serialized state. Only the third field is
used (and the file size, §9). A truncated or unreadable header is treated as
"no snapshot", never as an error — the request still goes through.

## 11. Deliberate non-goals

* **No request rewriting.** The proxy restores and then forwards the body
  untouched. This is what makes a cache failure a performance event rather than
  a correctness event.
* **No multi-slot modeling.** `-np 1` is assumed; the cache work is serialized
  behind one lock.
* **No warm-up.** Serving the first request promptly matters more than having
  every snapshot resident.
