// llama-slot-proxy: a transparent caching proxy in front of llama-server.
//
// The problem it solves: an agent client resends the whole conversation
// (system prompt + tool definitions + history) on every turn. llama-server
// reuses a matching prefix from its slot, but a brand-new session — or one
// whose state was evicted by another session — has to prefill the shared
// system prompt from scratch, which at a ~36 KB system prompt costs tens of
// seconds per new session. This proxy restores a previously saved slot state
// before forwarding the request, and keeps those states on disk so they also
// survive a llama-server restart.
//
// Design constraints, all of them learned from measurement:
//
//   - The proxy NEVER rewrites the request body. It only issues a restore
//     BEFORE forwarding. So even if every cache operation fails, the forwarded
//     request is still correct — just slower. Caching is never load-bearing.
//
//   - Reuse requires the saved token sequence to be a strict PREFIX of the new
//     prompt (llama-server reports this as cache_n == n_saved). Hence the seed
//     must end exactly where the system message ends, while a session snapshot
//     may contain anything, because a later turn of the same conversation is a
//     superset of an earlier one.
//
//   - A restore OVERWRITES the slot. Restores are therefore never additive:
//     of the seed and the session snapshot only the deeper one may be applied.
//
//   - The chat template refuses to render a system-only conversation ("No user
//     query found in messages"), so the seed is built by rendering
//     {system, user:MARK} and truncating at MARK. That leaves a prefix ending
//     in the assistant-generation prompt, which any real prompt starts with.
//
//   - `tools` render INSIDE the system message, so the cache key must cover
//     them as well as the system content.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const seedMark = "ZZQQ_PROXY_SEED_MARK_9F3A2C"

// ---------------------------------------------------------------- config

type Config struct {
	Listen   string
	Upstream string
	CacheDir string // work dir; must equal llama-server's --slot-save-path (tmpfs)
	Persist  string // durable copy, written on exit and on eviction
	LogFile  string
	LockWait time.Duration
	MaxBytes int64 // total cap for sess-* files
	MaxAge   time.Duration
	MaxSeeds int
	DryRun   bool
	Verbose  bool

	// Auxiliary requests (title generation, summarisation, ...) share the
	// session id but carry a tiny system prompt. Touching the slot for them is
	// pure loss: they build a useless seed and their snapshot would later be
	// restored over the real one. Skip caching entirely below this size.
	MinSystemBytes int
	// Optionally also skip non-streaming requests entirely.
	RequireStream bool

	// Content-growth thresholds for strategy B (0 disables).
	SaveEveryTokens int64
	SaveEveryMsgs   int

	// Cap for the durable directory (CacheDir is capped by MaxBytes).
	DurableBytes int64

	// HTTP header carrying the session identity.
	SessionHeader string
}

func (c *Config) Addr() string { return "http://" + c.Upstream }

// ---------------------------------------------------------------- main

func main() {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", "0.0.0.0:8080", "listen address")
	flag.StringVar(&cfg.Upstream, "upstream", "127.0.0.1:8081", "llama-server address")
	flag.StringVar(&cfg.CacheDir, "cache-dir", "/dev/shm/llama-slot-proxy", "working dir; MUST be the same path given to llama-server as --slot-save-path")
	flag.StringVar(&cfg.Persist, "persist-dir", "", "durable dir: gets a copy on exit and receives evicted snapshots (\"\" = keep everything in cache-dir)")
	flag.StringVar(&cfg.LogFile, "log", "", "log file (\"\" = stderr)")
	flag.DurationVar(&cfg.LockWait, "lock-wait", 120*time.Second, "max wait for the slot lock")
	flag.Int64Var(&cfg.MaxBytes, "max-bytes", 16<<30, "total cap for the cache directory")
	flag.DurationVar(&cfg.MaxAge, "max-age", 24*time.Hour, "max age of a session snapshot")
	flag.IntVar(&cfg.MaxSeeds, "max-seeds", 4, "max number of seed files kept")
	// B: save on CONTENT growth, never on a timer.
	flag.Int64Var(&cfg.SaveEveryTokens, "save-every-tokens", 8000, "save a session snapshot after this many new tokens (0 = only on slot handover)")
	flag.IntVar(&cfg.SaveEveryMsgs, "save-every-msgs", 10, "save a session snapshot after this many new messages (0 = disable)")
	flag.Int64Var(&cfg.DurableBytes, "durable-bytes", 32<<30, "cap for the durable directory")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "never touch the cache (pure passthrough)")
	flag.BoolVar(&cfg.Verbose, "v", false, "verbose logging")
	flag.IntVar(&cfg.MinSystemBytes, "min-system-bytes", 2000, "never cache requests whose system message is shorter than this (0 = cache all)")
	flag.BoolVar(&cfg.RequireStream, "require-stream", false, "only cache streaming requests (non-streaming ones pass through)")
	flag.StringVar(&cfg.SessionHeader, "session-header", "Session_id", "request header carrying the session id")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	p := &Proxy{
		cfg:      cfg,
		client:   &http.Client{Timeout: 0}, // no timeout: generations are long
		mu:       make(chan struct{}, 1),
		rec:      newRecorder(),
		expect:   map[string]int64{},
		badHits:  map[string]int{},
		savedTok: map[string]int64{},
		savedMsg: map[string]int{},
		msgCount: map[string]int{},
	}
	p.mu <- struct{}{} // unlocked

	if !cfg.DryRun {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			log.Fatalf("cache dir: %v", err)
		}
		if cfg.Persist != "" {
			if err := os.MkdirAll(cfg.Persist, 0o755); err != nil {
				log.Fatalf("persist dir: %v", err)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
		// long generations + long restores
		ReadHeaderTimeout: 30 * time.Second,
	}

	shutdown := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down; flushing snapshots to disk")
		// Refuse new work, then wait for in-flight requests so the slot we are
		// about to capture is not mid-generation.
		srv.Close()
		p.drain(p.cfg.LockWait)
		p.flushToDisk()
		close(shutdown)
	}()

	log.Printf("llama-slot-proxy listening on %s -> %s (cache=%s dry_run=%v)",
		cfg.Listen, cfg.Upstream, cfg.CacheDir, cfg.DryRun)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
	// Wait for the shutdown handler to finish the disk flush before returning:
	// otherwise main() exits and the copy is cut off mid-file.
	select {
	case <-shutdown:
	case <-time.After(60 * time.Second):
		log.Printf("WARN shutdown flush timed out")
	}
	log.Printf("bye")
}

// drain waits until no request holds the slot lock.
func (p *Proxy) drain(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if p.lock(200 * time.Millisecond) {
			p.unlock()
			return
		}
	}
}

// ---------------------------------------------------------------- proxy

type Proxy struct {
	cfg    Config
	client *http.Client
	mu     chan struct{} // capacity 1, used as a mutex with timeout
	rec    *recorder

	// Snapshot health. A restore that does not actually get reused is worse
	// than no restore at all: it overwrites the slot and destroys the
	// in-process checkpoints the server would otherwise have rolled back to.
	// So we watch for that and retire the offending snapshot.
	smu     sync.Mutex
	expect  map[string]int64 // session -> tokens the snapshot claims to hold
	badHits map[string]int   // session -> consecutive non-reuses

	// Slot continuity. llama-server already keeps the slot state between
	// requests, so restoring for a session that is ALREADY resident is pure
	// overhead — and it destroys the in-process checkpoints the server would
	// otherwise roll back to. Measured: steady-state turns process the same
	// number of tokens either way, while a redundant restore adds ~0.4 s.
	slotOwner  string // session whose state currently sits in the slot
	slotTokens int64  // tokens resident (0 = empty/unknown)

	// Strategy B bookkeeping: how much of each session is already on disk.
	// Growth is measured in tokens and messages, never on a timer.
	savedTok map[string]int64
	savedMsg map[string]int
	msgCount map[string]int
}

type reqInfo struct {
	Session string
	Key     string // seed key (system + tools)
	System  string // the raw system content
	Tools   json.RawMessage
	Body    []byte
	IsChat  bool
	Stream  bool
	NMsgs   int
}

func (p *Proxy) lock(d time.Duration) bool {
	select {
	case <-p.mu:
		return true
	case <-time.After(d):
		return false
	}
}

func (p *Proxy) unlock() { p.mu <- struct{}{} }

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Only /v1/chat/completions is eligible for caching. Everything else
	// (health, props, models, tokenize, embeddings, ...) is a plain passthrough.
	if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
		p.passthrough(w, r, start, nil)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	info, err := p.parseRequest(r, body)
	if err != nil || !info.IsChat {
		// Degrade A: nothing we can key on.
		p.passthrough(w, r, start, body)
		return
	}

	if p.cfg.DryRun {
		p.passthrough(w, r, start, body)
		return
	}

	// Auxiliary request? Forward without ever touching the slot.
	if p.isAuxiliary(info) {
		log.Printf("INFO aux request skipped session=%s msgs=%d sys_bytes=%d stream=%v",
			info.Session, info.NMsgs, len(info.System), info.Stream)
		p.forgetSlot() // an untracked request may have overwritten the slot
		p.passthrough(w, r, start, body)
		return
	}

	// Degrade D: lock timeout -> forward without touching the cache.
	if !p.lock(p.cfg.LockWait) {
		log.Printf("WARN lock timeout session=%s action=passthrough", info.Session)
		p.passthrough(w, r, start, body)
		return
	}
	defer p.unlock()

	// --- strategy A: if the slot currently holds a DIFFERENT session, persist
	// that session before we overwrite it. This is the only moment its state
	// can still be captured, and it is why steady-state turns need no save. ---
	if prev, tok := p.slotOwnerOf(); prev != "" && prev != info.Session && tok > 0 {
		p.prune() // make room first: a full tmpfs makes the save fail
		if err := p.sessionSaveName(prev); err != nil {
			log.Printf("WARN handover save failed session=%s err=%v", prev, err)
		} else if p.cfg.Verbose {
			log.Printf("INFO handover saved session=%s tokens=%d", prev, tok)
		}
	}

	// --- cache phases: every failure is non-fatal ---
	//
	// ⚠️ A restore OVERWRITES the slot, so these two are NOT additive: the
	// deeper one wins and the shallower one must be skipped. Restoring the
	// seed after a session snapshot (or vice versa) destroys the better state
	// — that bug cost a real session a 67 s full recompute.
	//
	// Rule: try the session snapshot first (it is usually much deeper), then
	// fall back to the seed only if the snapshot is absent or shallower.
	seedState := "none"
	sessState := "none"

	// ---- slot continuity: the cheapest and best path ----
	// If this session's state is already in the slot, llama-server's own
	// prefix matching handles the continuation. Do NOT restore.
	if p.slotHolds(info.Session) {
		sessState = "resident"
	} else {
		sessN := p.sessionSnapshotN(info) // does not touch the slot
		seedN := p.seedProbe(info)        // does not touch the slot

		switch {
		case sessN <= 0 && seedN <= 0:
			// Nothing usable on disk: build the seed (leaves it resident).
			if p.seedEnsure(info) {
				seedState = "hit"
			} else {
				seedState = "miss"
			}
		case sessN >= seedN:
			// Snapshot is deeper (or equal): restore it and skip the seed.
			if p.restoreBySession(info) {
				sessState = "hit"
			} else if p.seedEnsure(info) {
				seedState = "hit"
			} else {
				seedState = "miss"
			}
		default:
			// Seed is deeper: restore the seed, skip the snapshot.
			if p.seedEnsure(info) {
				seedState = "hit"
			} else {
				seedState = "miss"
			}
		}
	}

	// --- forward (never rewritten) ---
	up := p.forward(w, r, body, start)

	// --- record who owns the slot now (for the continuity fast path) ---
	if up != nil {
		p.noteReuse(info.Session, up.cacheN)
		p.claimSlot(info.Session)
		p.noteMsgs(info.Session, info.NMsgs)
		// Stamp the fingerprint now, not only on save: a session that stays
		// resident never triggers a save, and without a sidecar a later
		// handover could not archive it at all.
		p.stampKey(info)
		if p.shouldSave(info) {
			// Free room BEFORE writing, reserving space for this very snapshot.
			// Pruning only after a successful save deadlocks once tmpfs is
			// full: the save fails, so prune never runs, so nothing is freed.
			p.pruneReserve(p.expectedSnapshotBytes())
			if err := p.sessionSave(info); err != nil {
				// Degrade C: log only. Drop the partial file: a truncated state
				// file reports the full token count in its header and would be
				// picked as "deeper" only to fail on restore.
				_ = os.Remove(p.cachePath(p.sessionName(info.Session)))
				log.Printf("WARN save failed session=%s err=%v (partial file removed)", info.Session, err)
				p.prune()
			}
		}
	}

	p.rec.add(rec{
		Session: info.Session,
		Seed:    seedState,
		Sess:    sessState,
		DurMS:   time.Since(start).Milliseconds(),
		CacheN:  up.cacheN,
		PromptN: up.promptN,
	})
	if p.cfg.Verbose {
		log.Printf("OK session=%s seed=%s sess=%s cache_n=%d prompt_n=%d total_ms=%d",
			info.Session, seedState, sessState, up.cacheN, up.promptN, time.Since(start).Milliseconds())
	}
}

// parseRequest extracts the cache key material. It never modifies the body.
func (p *Proxy) parseRequest(r *http.Request, body []byte) (*reqInfo, error) {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools  json.RawMessage `json:"tools"`
		Stream bool            `json:"stream"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if len(doc.Messages) == 0 || doc.Messages[0].Role != "system" {
		return nil, fmt.Errorf("no leading system message")
	}

	// Session identity: the configured header, then a common fallback. Without
	// one the proxy still works, it just cannot recognize returning sessions.
	session := strings.TrimSpace(r.Header.Get(p.cfg.SessionHeader))
	if session == "" {
		session = strings.TrimSpace(r.Header.Get("X-Client-Request-Id"))
	}

	sys := rawToString(doc.Messages[0].Content)

	// The key must cover system AND tools: tools render inside the system
	// message, so a change in tools changes the rendered prefix.
	h := sha256.New()
	h.Write([]byte(sys))
	h.Write([]byte{0})
	h.Write(bytes.TrimSpace(doc.Tools))
	key := hex.EncodeToString(h.Sum(nil))[:16]

	return &reqInfo{
		Session: sanitize(session),
		Key:     key,
		System:  sys,
		Tools:   doc.Tools,
		Body:    body,
		IsChat:  true,
		Stream:  doc.Stream,
		NMsgs:   len(doc.Messages),
	}, nil
}

// rawToString accepts both a plain string and the multimodal array form.
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

var unsafeName = strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_",
	"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", " ", "_")

// sanitize makes a session id safe for a flat filename. llama-server's
// fs_validate_filename rejects path separators and control characters.
func sanitize(s string) string {
	s = unsafeName.Replace(s)
	var b strings.Builder
	for _, r := range s {
		if r > 0x1f && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "anonymous"
	}
	if len(out) > 120 {
		sum := sha256.Sum256([]byte(s))
		out = out[:100] + "-" + hex.EncodeToString(sum[:])[:12]
	}
	return out
}

// isAuxiliary reports whether this request should bypass caching entirely.
//
// An agent client often reuses one session id for its main loop AND for small
// side requests (title generation, summarisation, ...). Those carry a tiny
// system prompt; caching them would (a) build a useless seed and (b) leave a
// shallow snapshot that poisons the real conversation on the next turn.
func (p *Proxy) isAuxiliary(info *reqInfo) bool {
	if p.cfg.RequireStream && !info.Stream {
		return true
	}
	if p.cfg.MinSystemBytes > 0 && len(info.System) < p.cfg.MinSystemBytes {
		return true
	}
	return false
}

// ---------------------------------------------------------------- forward

type upResult struct {
	cacheN  int64
	promptN int64
}

// forward streams the upstream response to the client. It returns nil only if
// nothing could be sent.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte, start time.Time) *upResult {
	url := p.cfg.Addr() + r.URL.RequestURI()
	method := r.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(r.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "proxy: bad request", http.StatusBadGateway)
		return nil
	}
	copyHeaders(req.Header, r.Header)
	if body != nil {
		req.Header.Set("Content-Length", fmt.Sprint(len(body)))
		req.ContentLength = int64(len(body))
	} else {
		req.Header.Del("Content-Length")
		req.ContentLength = 0
	}

	resp, err := p.client.Do(req)
	if err != nil {
		// Degrade E: upstream unreachable.
		log.Printf("ERROR upstream: %v", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return nil
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Tee so we can read `timings` out of the tail without buffering the whole
	// stream. Keeps a bounded tail only.
	tail := &tailBuffer{max: 256 << 10}
	_, _ = io.Copy(io.MultiWriter(w, tail), resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return &upResult{cacheN: tail.cacheN(), promptN: tail.promptN()}
}

func (p *Proxy) passthrough(w http.ResponseWriter, r *http.Request, start time.Time, body []byte) {
	// GET/HEAD and other body-less methods: forward verbatim, no body handling.
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		p.forward(w, r, nil, start)
		return
	}
	if body == nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}
	}
	p.forward(w, r, body, start)
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch k {
		case "Content-Length", "Host", "Connection", "Transfer-Encoding":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// tailBuffer keeps the last `max` bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// cacheN/promptN dig the usage numbers out of the tail of an SSE stream or a
// plain JSON body. Absent values yield 0.
func (t *tailBuffer) cacheN() int64  { return t.num("cache_n") }
func (t *tailBuffer) promptN() int64 { return t.num("prompt_n") }

func (t *tailBuffer) num(field string) int64 {
	i := bytes.LastIndex(t.buf, []byte(`"`+field+`":`))
	if i < 0 {
		return 0
	}
	j := i + len(field) + 3
	var n int64
	neg := false
	if j < len(t.buf) && t.buf[j] == '-' {
		neg = true
		j++
	}
	for ; j < len(t.buf); j++ {
		c := t.buf[j]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n
}

// ---------------------------------------------------------------- misc

// cachePath joins a flat filename onto the cache dir.
func (p *Proxy) cachePath(name string) string {
	return filepath.Join(p.cfg.CacheDir, name)
}
