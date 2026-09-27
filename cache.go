package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Cache phases.
//
// Every function here returns false / logs on failure. None of them may abort a
// request: if the cache is broken the proxy still forwards, just slower.
// ============================================================================

// ---------------------------------------------------------------- seed

// seedName is the content-addressed seed filename for a request.
func (p *Proxy) seedName(key string) string { return "seed-" + key + ".bin" }

// seedEnsure makes sure a seed for this system+tools exists in the slot.
//
// Returns true if the slot is now (or already was) holding the seed state.
// On a miss it builds the seed in-process, leaving the slot holding exactly the
// seed — no extra restore is needed afterwards.
func (p *Proxy) seedEnsure(info *reqInfo) bool {
	name := p.seedName(info.Key)
	if !p.ensureLocal(name) {
		// build below
	} else if _, ok := p.slotRestore(name); ok {
		return true
	}
	path := p.cachePath(name)

	if _, err := os.Stat(path); err == nil {
		// Hit: put it in the slot.
		if _, ok := p.slotRestore(name); ok {
			return true
		}
		// Degrade B: corrupt/expired file -> treat as a miss and rebuild.
		log.Printf("WARN seed restore failed, rebuilding file=%s", name)
		_ = os.Remove(path)
	}

	// Miss: build it. This costs one prefill of the system prompt (~8k tokens).
	if err := p.buildSeed(info, name); err != nil {
		log.Printf("WARN seed build failed key=%s err=%v", info.Key, err)
		return false
	}
	return true
}

// buildSeed renders {system, user:MARK} via /apply-template, truncates at MARK,
// prefills it with n_predict=0 and saves the slot state.
func (p *Proxy) buildSeed(info *reqInfo, name string) error {
	payload := map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": info.System},
			{"role": "user", "content": seedMark},
		},
		"add_generation_prompt": false,
	}
	if len(info.Tools) > 0 {
		var tools any
		if err := json.Unmarshal(info.Tools, &tools); err == nil {
			payload["tools"] = tools
		}
	}

	var out struct {
		Prompt string `json:"prompt"`
	}
	if err := p.postJSON("/apply-template", payload, &out); err != nil {
		return fmt.Errorf("apply-template: %w", err)
	}
	i := strings.Index(out.Prompt, seedMark)
	if i < 0 {
		return fmt.Errorf("marker not found in rendered prompt")
	}
	seed := out.Prompt[:i]
	if strings.TrimSpace(seed) == "" {
		return fmt.Errorf("empty seed")
	}

	// Prefill only: n_predict=0 stops right after the prompt is evaluated,
	// leaving the seed state resident in the slot.
	var cmp struct {
		Timings struct {
			PromptN int64 `json:"prompt_n"`
		} `json:"timings"`
	}
	err := p.postJSON("/completion", map[string]any{
		"prompt":       seed,
		"n_predict":    0,
		"cache_prompt": false,
	}, &cmp)
	if err != nil {
		return fmt.Errorf("prefill seed: %w", err)
	}

	if !p.slotSave(name) {
		return fmt.Errorf("save seed")
	}
	log.Printf("INFO built seed name=%s bytes=%d prefill_tokens=%d", name, len(seed), cmp.Timings.PromptN)
	return nil
}

// ---------------------------------------------------------------- session

func (p *Proxy) sessionName(session string) string {
	return "sess-" + session + ".bin"
}

// sessionKeyName is a sidecar recording which system+tools the snapshot was
// captured under, so a snapshot from a different system can be discarded.
func (p *Proxy) sessionKeyName(session string) string {
	return "key-" + session + ".txt"
}

// sessionSnapshotN reports how deep this session's snapshot is, in tokens.
// Returns 0 when there is none (or it is too old, in which case it is removed).
// This does NOT touch the slot — it only reads the 12-byte header via the
// server's own size query.
func (p *Proxy) sessionSnapshotN(info *reqInfo) int64 {
	if info.Session == "" {
		return 0
	}
	name := p.sessionName(info.Session)

	// Check the fingerprint BEFORE pulling the file back: a snapshot can be
	// gigabytes, and copying one only to discard it wastes that I/O every
	// single request.
	key, savedMsgs := p.sessionMeta(info.Session)
	if key == "" || key != info.Key {
		// Only complain when there really was a snapshot to discard: a brand new
		// session legitimately has no sidecar yet.
		had := false
		if _, err := os.Stat(p.cachePath(name)); err == nil {
			had = true
			_ = os.Remove(p.cachePath(name))
		}
		if _, err := os.Stat(p.cachePath(p.sessionKeyName(info.Session))); err == nil {
			_ = os.Remove(p.cachePath(p.sessionKeyName(info.Session)))
		}
		if p.cfg.Persist != "" && p.cfg.Persist != p.cfg.CacheDir {
			dst := filepath.Join(p.cfg.Persist, name)
			if _, err := os.Stat(dst); err == nil {
				had = true
				_ = os.Remove(dst)
			}
		}
		if had {
			reason := "system mismatch"
			if key == "" {
				reason = "no sidecar"
			}
			log.Printf("INFO dropped snapshot session=%s (%s)", info.Session, reason)
		}
		return 0
	}

	// A snapshot taken from a longer conversation cannot be a prefix of a
	// shorter request, so restoring it would only force a full recompute — and
	// because it probes deeper than the seed it would win the comparison and
	// shadow the seed, which is still perfectly valid. Skip it here, before
	// pulling the file back. The snapshot itself is kept: the client may return
	// with the longer prompt.
	if savedMsgs > 0 && info.NMsgs < savedMsgs {
		if p.cfg.Verbose {
			log.Printf("INFO snapshot skipped session=%s (saved with %d msgs, request has %d)",
				info.Session, savedMsgs, info.NMsgs)
		}
		return 0
	}

	if !p.ensureLocal(name) {
		return 0
	}
	path := p.cachePath(name)
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if p.cfg.MaxAge > 0 && time.Since(st.ModTime()) > p.cfg.MaxAge {
		_ = os.Remove(path)
		return 0
	}
	// The snapshot is only valid for the system+tools it was captured under.
	// An auxiliary request (title generation, ...) issued under the same
	// session id carries a DIFFERENT system message; without this check its
	// snapshot would be restored over the real one and force a full recompute.
	// The file embeds its token count; read it without restoring.
	return p.slotProbe(name)
}

// restoreBySession puts this session's snapshot into the slot.
func (p *Proxy) restoreBySession(info *reqInfo) bool {
	if info.Session == "" {
		return false
	}
	name := p.sessionName(info.Session)
	n, ok := p.slotRestore(name)
	if !ok {
		// Degrade B: broken snapshot, drop it.
		log.Printf("WARN session restore failed, dropping file=%s", name)
		_ = os.Remove(p.cachePath(name))
		return false
	}
	p.smu.Lock()
	p.expect[info.Session] = n
	p.smu.Unlock()
	return true
}

// seedProbe reports how many tokens the seed file holds (0 if absent).
func (p *Proxy) seedProbe(info *reqInfo) int64 {
	name := p.seedName(info.Key)
	if !p.ensureLocal(name) {
		return 0
	}
	return p.slotProbe(name)
}

// noteReuse records whether the restored snapshot actually got reused.
//
// A snapshot that restores but is not reused is harmful: the restore already
// overwrote the slot, so the server lost the in-process checkpoints it would
// have used. Two consecutive such events retire the snapshot.
func (p *Proxy) noteReuse(session string, cacheN int64) {
	if session == "" {
		return
	}
	p.smu.Lock()
	exp, had := p.expect[session]
	delete(p.expect, session)
	if !had {
		p.smu.Unlock()
		return
	}
	// Reuse means the server took essentially the whole snapshot.
	reused := cacheN >= exp-8 && cacheN > 0
	if reused {
		p.badHits[session] = 0
		p.smu.Unlock()
		return
	}
	p.badHits[session]++
	n := p.badHits[session]
	p.smu.Unlock()

	if n >= 2 {
		name := p.sessionName(session)
		if err := os.Remove(p.cachePath(name)); err == nil {
			log.Printf("WARN retired unusable snapshot session=%s (restored %d, reused %d) — server checkpoints preserved instead",
				session, exp, cacheN)
		}
		p.smu.Lock()
		delete(p.badHits, session)
		p.smu.Unlock()
	}
}

func (p *Proxy) sessionSave(info *reqInfo) error {
	if info.Session == "" {
		return nil
	}
	name := p.sessionName(info.Session)
	if !p.slotSave(name) {
		return fmt.Errorf("save failed")
	}
	// Record the owning system+tools, plus the message count reached, so the
	// retirement rule can compare against policy B's thresholds later.
	if err := os.WriteFile(p.cachePath(p.sessionKeyName(info.Session)),
		[]byte(fmt.Sprintf("%s\n%d", info.Key, info.NMsgs)), 0o644); err != nil {
		log.Printf("WARN could not write key sidecar session=%s err=%v", info.Session, err)
	}
	if tok, ok := p.slotTokensLive(); ok {
		p.markSaved(info.Session, tok)
	}
	return nil
}

// ---------------------------------------------------------------- slot API

// slotProbe reports how many tokens a state file holds, WITHOUT restoring it.
// The proxy shares the filesystem with the server, so we just read the header.
//
// A truncated file (e.g. a save that ran out of space) still carries the full
// token count in its header, so the size is checked against the measured size
// law ~= 149.63 MiB + 34.0 KiB per token. Anything far short is reported as 0,
// which makes the caller treat it as absent instead of restoring garbage.
func (p *Proxy) slotProbe(name string) int64 {
	n := p.readStateTokens(name)
	if n <= 0 {
		return 0
	}
	st, err := os.Stat(p.cachePath(name))
	if err != nil {
		return 0
	}
	const fixedBytes = int64(149630000) // ~149.63 MiB
	const perToken = int64(34 * 1024)   // 34.0 KiB/token
	want := fixedBytes + n*perToken
	if st.Size() < want*8/10 { // allow 20% slack for version differences
		log.Printf("WARN truncated state file name=%s tokens=%d size=%d expected~%d — ignoring",
			name, n, st.Size(), want)
		return 0
	}
	return n
}

// readStateTokens parses the 12-byte header of a llama state file:
// u32 magic, u32 version, u32 n_token_count (little endian).
func (p *Proxy) readStateTokens(name string) int64 {
	f, err := os.Open(p.cachePath(name))
	if err != nil {
		return 0
	}
	defer f.Close()
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0
	}
	return int64(uint32(hdr[8]) | uint32(hdr[9])<<8 | uint32(hdr[10])<<16 | uint32(hdr[11])<<24)
}

func (p *Proxy) slotRestore(name string) (int64, bool) {
	var out struct {
		NRestored int64 `json:"n_restored"`
		Timings   struct {
			RestoreMS float64 `json:"restore_ms"`
		} `json:"timings"`
	}
	err := p.postJSON("/slots/0?action=restore", map[string]any{"filename": name}, &out)
	if err != nil {
		if p.cfg.Verbose {
			log.Printf("restore err name=%s err=%v", name, err)
		}
		return 0, false
	}
	if out.NRestored <= 0 {
		return 0, false
	}
	if p.cfg.Verbose {
		log.Printf("INFO restore name=%s n_restored=%d ms=%.1f", name, out.NRestored, out.Timings.RestoreMS)
	}
	return out.NRestored, true
}

func (p *Proxy) slotSave(name string) bool {
	var out struct {
		NSaved int64 `json:"n_saved"`
	}
	if err := p.postJSON("/slots/0?action=save", map[string]any{"filename": name}, &out); err != nil {
		if p.cfg.Verbose {
			log.Printf("save err name=%s err=%v", name, err)
		}
		return false
	}
	return out.NSaved > 0
}

// postJSON is a tiny helper for the control endpoints. Timings are generous:
// a save of a 6 GB state is slow.
func (p *Proxy) postJSON(path string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, p.cfg.Addr()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	cl := &http.Client{Timeout: 10 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ---------------------------------------------------------------- prune

type fileInfo struct {
	name string
	size int64
	mod  time.Time
}

// prune enforces the size and count caps. It runs between requests (the caller
// holds the lock) so it never races with a restore/save.
// prune frees working copies with no reservation for upcoming writes.
func (p *Proxy) prune() { p.pruneReserve(0) }

// expectedSnapshotBytes estimates the size of the snapshot we are about to
// write, from the tokens currently resident in the slot (~149.63 MiB + 34 KiB
// per token).
func (p *Proxy) expectedSnapshotBytes() int64 {
	tok, ok := p.slotTokensLive()
	if !ok || tok <= 0 {
		return 0
	}
	return int64(149630000) + tok*(34*1024)
}

// pruneReserve enforces the tmpfs budget while leaving room for a snapshot of
// the given size.
//
// Pruning only on the pre-write total is not enough: the budget would be met,
// then the new snapshot would push the total back over it, and nothing would
// run afterwards to correct that. Since the seeds share this same tmpfs, they
// count against the budget too.
func (p *Proxy) pruneReserve(need int64) {
	files, err := p.list()
	if err != nil {
		return
	}
	var seeds, sess []fileInfo
	var total int64
	for _, f := range files {
		switch {
		case strings.HasPrefix(f.name, "seed-"):
			seeds = append(seeds, f)
			// Seeds live on the SAME tmpfs, so they must be counted against the
			// budget. Ignoring them let the working copy fill the filesystem:
			// seeds 863 MB + sessions 4.86 GB = 5.72 GB of 6 GB, leaving too
			// little room for the next 622 MB snapshot to be written at all.
			total += f.size
		case strings.HasPrefix(f.name, "sess-"):
			sess = append(sess, f)
			total += f.size
		case strings.HasPrefix(f.name, "key-"):
			// sidecar for a session snapshot; accounted with it
		case strings.HasSuffix(f.name, ".tmp"):
			// leftover from an interrupted copy: the real file is intact
			_ = os.Remove(p.cachePath(f.name))
		}
	}
	// oldest first
	byAge := func(s []fileInfo) {
		sort.Slice(s, func(i, j int) bool { return s[i].mod.Before(s[j].mod) })
	}
	byAge(seeds)
	byAge(sess)

	// drop expired sessions
	now := time.Now()
	for _, f := range sess {
		if p.cfg.MaxAge > 0 && now.Sub(f.mod) > p.cfg.MaxAge {
			if p.remove(f) {
				total -= f.size
			}
		}
	}
	// Enforce the byte cap by moving the oldest working copies to disk rather
	// than deleting them: the durable dir holds far more than the tmpfs budget.
	for _, f := range sess {
		if total+need <= p.cfg.MaxBytes {
			break
		}
		p.retireOrDemote(f.name)
		total -= f.size
	}
	// If even the durable dir is over budget, delete the oldest there.
	p.enforceDurable()
	// enforce the seed count
	for len(seeds) > p.cfg.MaxSeeds {
		if !p.remove(seeds[0]) {
			break
		}
		seeds = seeds[1:]
	}
}

func (p *Proxy) list() ([]fileInfo, error) {
	ents, err := os.ReadDir(p.cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	out := make([]fileInfo, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		// never prune another session's in-flight temp files
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileInfo{name: e.Name(), size: fi.Size(), mod: fi.ModTime()})
	}
	return out, nil
}

func (p *Proxy) remove(f fileInfo) bool {
	if err := os.Remove(filepath.Join(p.cfg.CacheDir, f.name)); err != nil {
		return false
	}
	if p.cfg.Verbose {
		log.Printf("INFO pruned %s (%d bytes)", f.name, f.size)
	}
	return true
}

// ---------------------------------------------------------------- recorder

// rec is one structured observability row.
type rec struct {
	Session string `json:"session"`
	Seed    string `json:"seed"`
	Sess    string `json:"sess"`
	CacheN  int64  `json:"cache_n"`
	PromptN int64  `json:"prompt_n"`
	DurMS   int64  `json:"dur_ms"`
	TS      string `json:"ts"`
}

// recorder keeps counters and appends rows to the log file.
type recorder struct {
	mu       sync.Mutex
	rows     int64
	hitSess  int64
	hitSeed  int64
	missSeed int64
	resident int64
	auxSkip  int64
	lockTO   int64
	upErr    int64
	cacheN   int64
	promptN  int64
	durMS    int64
	f        *os.File
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) add(e rec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows++
	switch e.Sess {
	case "hit":
		r.hitSess++
	case "resident":
		r.resident++
	}
	switch e.Seed {
	case "hit":
		r.hitSeed++
	case "miss":
		r.missSeed++
	}
	r.cacheN += e.CacheN
	r.promptN += e.PromptN
	r.durMS += e.DurMS
	if e.TS == "" {
		e.TS = time.Now().Format(time.RFC3339)
	}
	b, _ := json.Marshal(e)
	log.Printf("STATS rows=%d sess_hit=%d seed_miss=%d %s", r.rows, r.hitSess, r.missSeed, b)
}

// count records a request that never reached the cache phases.
func (r *recorder) count(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch kind {
	case "aux":
		r.auxSkip++
	case "lock_timeout":
		r.lockTO++
	case "upstream_error":
		r.upErr++
	}
}

// stats is a point-in-time copy of the counters.
type stats struct {
	rows, hitSess, hitSeed, missSeed, resident, auxSkip, lockTO, upErr int64
	cacheN, promptN, durMS                                             int64
}

func (r *recorder) snapshot() stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return stats{
		rows: r.rows, hitSess: r.hitSess, hitSeed: r.hitSeed, missSeed: r.missSeed,
		resident: r.resident, auxSkip: r.auxSkip, lockTO: r.lockTO, upErr: r.upErr,
		cacheN: r.cacheN, promptN: r.promptN, durMS: r.durMS,
	}
}

// ---------------------------------------------------------------- slot state

// slotHolds reports whether this session's state is already resident in the
// slot, in which case llama-server's own prefix matching handles the
// continuation and a restore would only cost time.
//
// This is the single most valuable check in the proxy: for a continuous
// conversation (which is the normal case, including tool-call round trips) the
// slot is already warm, and a restore both wastes ~0.4 s and discards the
// server's in-process checkpoints.
func (p *Proxy) slotHolds(info *reqInfo) bool {
	if info.Session == "" {
		return false
	}
	p.smu.Lock()
	owner, key, n := p.slotOwner, p.slotKey, p.slotTokens
	p.smu.Unlock()
	if owner != info.Session || n <= 0 {
		return false
	}
	// The session id alone is not enough to call the slot resident: if the
	// resident state was claimed under a different system prompt or tool set,
	// it does not hold THIS request's prefix. Forwarding it as "resident"
	// would then cost a full prefill and skip the seed that avoids it.
	// (Auxiliary requests are already filtered out by -min-system-bytes; this
	// guards the same shape when it slips past that filter, e.g. after the
	// system prompt or tools changed mid-session.)
	if key != info.Key {
		return false
	}
	// Verify against the live slot; the server is the source of truth.
	if live, ok := p.slotTokensLive(); ok && live <= 0 {
		p.forgetSlot()
		return false
	}
	return true
}

// slotTokensLive reads the resident token count from the server.
func (p *Proxy) slotTokensLive() (int64, bool) {
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(p.cfg.Addr() + "/slots")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var slots []struct {
		ID            int   `json:"id"`
		IsProcessing  bool  `json:"is_processing"`
		NPromptTokens int64 `json:"n_prompt_tokens"`
	}
	if err := json.Unmarshal(data, &slots); err != nil {
		return 0, false
	}
	for _, s := range slots {
		if s.ID == 0 {
			if s.IsProcessing {
				return 0, false
			}
			return s.NPromptTokens, true
		}
	}
	return 0, false
}

// claimSlot records that this session now occupies the slot.
func (p *Proxy) claimSlot(session, key string) {
	n, ok := p.slotTokensLive()
	if !ok {
		p.forgetSlot()
		return
	}
	p.smu.Lock()
	p.slotOwner, p.slotKey, p.slotTokens = session, key, n
	p.smu.Unlock()
}

// forgetSlot marks the slot as untracked (e.g. after a request we did not
// account for, such as an auxiliary call).
func (p *Proxy) forgetSlot() {
	p.smu.Lock()
	p.slotOwner, p.slotKey, p.slotTokens = "", "", 0
	p.smu.Unlock()
}

// ---------------------------------------------------------------- save policy

// shouldSave implements strategy B: save a session snapshot only when its
// CONTENT grew enough since the last save. Growth is measured in tokens and
// messages rather than wall-clock time, so idle periods cost nothing and bursts
// are still captured.
//
// Combined with strategy A (save on slot handover) this means a continuous
// conversation writes to disk roughly once per SaveEveryTokens tokens instead of
// once per request.
func (p *Proxy) shouldSave(info *reqInfo) bool {
	if info.Session == "" {
		return false
	}
	tok, ok := p.slotTokensLive()
	if !ok || tok <= 0 {
		return false
	}

	p.smu.Lock()
	lastTok := p.savedTok[info.Session]
	lastMsg := p.savedMsg[info.Session]
	p.smu.Unlock()

	if p.cfg.SaveEveryTokens > 0 && tok-lastTok >= p.cfg.SaveEveryTokens {
		return true
	}
	if p.cfg.SaveEveryMsgs > 0 && info.NMsgs-lastMsg >= p.cfg.SaveEveryMsgs {
		return true
	}
	return false
}

// slotOwnerOf reports which session currently occupies the slot.
func (p *Proxy) slotOwnerOf() (string, int64) {
	p.smu.Lock()
	defer p.smu.Unlock()
	return p.slotOwner, p.slotTokens
}

// sessionSaveName saves the snapshot for a session we are not currently
// handling (the slot handover case). It captures whatever is in the slot right
// now, which at handover time is exactly that session's state.
func (p *Proxy) sessionSaveName(session string) error {
	key, ok := p.cachedKey(session)
	if !ok {
		// We do not know its system+tools any more; without the sidecar a later
		// restore would be untrusted anyway, so skip.
		return fmt.Errorf("no key sidecar for %s", session)
	}
	if !p.slotSave(p.sessionName(session)) {
		return fmt.Errorf("slot save failed")
	}
	p.smu.Lock()
	n := p.msgCount[session]
	p.smu.Unlock()
	_ = os.WriteFile(p.cachePath(p.sessionKeyName(session)),
		[]byte(fmt.Sprintf("%s\n%d", key, n)), 0o644)
	if tok, ok := p.slotTokensLive(); ok {
		p.markSaved(session, tok)
	}
	return nil
}

// cachedKey returns the remembered system+tools fingerprint for a session.
func (p *Proxy) cachedKey(session string) (string, bool) {
	k, _ := p.sessionMeta(session)
	if k == "" {
		return "", false
	}
	return k, true
}

// markSaved records how much of a session is on disk.
func (p *Proxy) markSaved(session string, tok int64) {
	p.smu.Lock()
	p.savedTok[session] = tok
	if v, ok := p.msgCount[session]; ok {
		p.savedMsg[session] = v
	}
	p.smu.Unlock()
}

// noteMsgs remembers the message count of the latest request for a session.
func (p *Proxy) noteMsgs(session string, n int) {
	p.smu.Lock()
	p.msgCount[session] = n
	p.smu.Unlock()
}

// enforceDurable trims the durable directory to a (larger) budget, oldest
// first. This is where snapshots finally get deleted, so the on-disk cap can
// safely exceed the tmpfs cap.
func (p *Proxy) enforceDurable() {
	if p.cfg.Persist == "" || p.cfg.Persist == p.cfg.CacheDir {
		return
	}
	ents, err := os.ReadDir(p.cfg.Persist)
	if err != nil {
		return
	}
	type fi struct {
		name string
		size int64
		mod  time.Time
	}
	var sess []fi
	var total int64
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") ||
			strings.HasSuffix(e.Name(), ".tmp") ||
			strings.HasPrefix(e.Name(), "key-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		sess = append(sess, fi{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(sess, func(i, j int) bool { return sess[i].mod.Before(sess[j].mod) })
	limit := p.cfg.DurableBytes
	if limit <= 0 {
		return
	}
	for _, f := range sess {
		if total <= limit {
			break
		}
		if err := os.Remove(filepath.Join(p.cfg.Persist, f.name)); err == nil {
			total -= f.size
			if p.cfg.Verbose {
				log.Printf("INFO durable pruned %s (%d bytes)", f.name, f.size)
			}
		}
	}
}

// ---------------------------------------------------------------- retirement

// tokensOf reads a state file's token count (0 if unreadable).
func (p *Proxy) tokensOf(name string) int64 {
	if _, err := os.Stat(p.cachePath(name)); err != nil {
		return 0
	}
	return p.readStateTokens(name)
}

// seedTokensFor returns how many tokens the seed for a given system+tools
// fingerprint holds (0 when there is no such seed).
func (p *Proxy) seedTokensFor(key string) int64 {
	if key == "" {
		return 0
	}
	return p.tokensOf("seed-" + key + ".bin")
}

// sessionMeta reads a snapshot's sidecar: the system+tools fingerprint it
// belongs to, and the message count it was captured at. Older sidecars hold
// only the fingerprint, in which case the message count comes back as 0.
func (p *Proxy) sessionMeta(session string) (key string, msgs int) {
	b, err := os.ReadFile(p.cachePath("key-" + session + ".txt"))
	if err != nil {
		return "", 0
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	key = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		msgs, _ = strconv.Atoi(strings.TrimSpace(lines[1]))
	}
	return key, msgs
}

// seedCoversIt reports whether the seed alone is already as good as this
// session's snapshot.
//
// The bar is deliberately the SAME one policy B uses to decide that a save is
// worth making: a snapshot that never accumulated SaveEveryTokens new tokens
// nor SaveEveryMsgs new messages is not worth storing at all. Since the seed
// always survives, archiving such a snapshot would cost hundreds of MB of disk
// and a copy on every exit while saving the next restore only a few tokens.
//
// Returns false when the question cannot be answered (no seed, no snapshot, or
// no sidecar), in which case the caller keeps the snapshot.
func (p *Proxy) seedCoversIt(session string) bool {
	name := p.sessionName(session)
	sTok := p.tokensOf(name)
	if sTok <= 0 {
		return false // nothing to reason about
	}
	key, msgs := p.sessionMeta(session)
	seedTok := p.seedTokensFor(key)
	if seedTok <= 0 {
		return false // no seed to fall back on -> must keep
	}
	gainTok := sTok - seedTok
	if p.cfg.SaveEveryTokens > 0 && gainTok >= p.cfg.SaveEveryTokens {
		return false // deep enough to be worth archiving
	}
	if p.cfg.SaveEveryMsgs > 0 && msgs >= p.cfg.SaveEveryMsgs {
		return false // enough conversation turns to be worth archiving
	}
	return true
}

// retireOrDemote decides what happens to a snapshot that no longer fits in the
// tmpfs budget.
//
//   - If the seed gives nearly the same depth, the snapshot is worthless: a
//     restore would save only a handful of tokens, while the file costs
//     hundreds of MB of disk and an entry in the exit flush. Delete it.
//   - Otherwise move it to the durable directory so a later session switch can
//     still pull it back.
func (p *Proxy) retireOrDemote(name string) {
	session, ok := strings.CutPrefix(name, "sess-")
	if !ok {
		p.demote(name)
		return
	}
	session = strings.TrimSuffix(session, ".bin")

	if p.seedCoversIt(session) {
		// The snapshot does not clear the same bar we use to decide a save is
		// worth making, so the seed already covers this conversation just as
		// well. Drop the working copy, the durable copy and the sidecar: an
		// archive would cost hundreds of MB and a copy on every exit while
		// saving the next restore only a few tokens.
		_ = os.Remove(p.cachePath(name))
		if p.cfg.Persist != "" && p.cfg.Persist != p.cfg.CacheDir {
			_ = os.Remove(filepath.Join(p.cfg.Persist, name))
		}
		_ = os.Remove(p.cachePath("key-" + session + ".txt"))
		log.Printf("INFO dropped snapshot session=%s — seed already covers it (gain below the save threshold)", session)
		return
	}
	p.demote(name)
}

// stampKey records this session's system+tools fingerprint and message count
// without saving any state. Done as soon as the slot is ours so that a later
// handover always has what it needs, even for a session that never reaches a
// save threshold.
func (p *Proxy) stampKey(info *reqInfo) {
	if info.Session == "" || info.Key == "" {
		return
	}
	p.smu.Lock()
	n := p.msgCount[info.Session]
	p.smu.Unlock()
	_ = os.WriteFile(p.cachePath(p.sessionKeyName(info.Session)),
		[]byte(fmt.Sprintf("%s\n%d", info.Key, n)), 0o644)
}
