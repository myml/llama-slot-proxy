package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// handleStats answers GET /stats with everything needed to tell whether the
// proxy is doing its job — without grepping the log.
func (p *Proxy) handleStats(w http.ResponseWriter, r *http.Request) {
	s := p.rec.snapshot()

	localBytes, localFiles := dirUsage(p.cfg.CacheDir)
	var durableBytes, durableFiles int64
	if p.cfg.Persist != "" {
		durableBytes, durableFiles = dirUsage(p.cfg.Persist)
	}

	owner, tok := p.slotOwnerOf()

	resp := map[string]any{
		"uptime_s": int64(time.Since(p.started).Seconds()),
		"requests": map[string]any{
			"cached":          s.rows,
			"resident":        s.resident,
			"session_hit":     s.hitSess,
			"seed_hit":        s.hitSeed,
			"seed_miss":       s.missSeed,
			"aux_skipped":     s.auxSkip,
			"lock_timeouts":   s.lockTO,
			"upstream_errors": s.upErr,
		},
		"tokens": map[string]any{
			"reused":      s.cacheN,
			"recomputed":  s.promptN,
			"reuse_ratio": ratio(s.cacheN, s.cacheN+s.promptN),
		},
		"cache": map[string]any{
			"dir":            p.cfg.CacheDir,
			"bytes":          localBytes,
			"files":          localFiles,
			"limit_bytes":    p.cfg.MaxBytes,
			"durable_dir":    p.cfg.Persist,
			"durable_bytes":  durableBytes,
			"durable_files":  durableFiles,
			"durable_limit":  p.cfg.DurableBytes,
			"total_duration": s.durMS,
		},
		"slot": map[string]any{
			"owner":  owner,
			"tokens": tok,
		},
		"model": map[string]any{
			"fingerprint": p.modelID(),
			"description": p.modelDesc(),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func ratio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// dirUsage sums the size and count of the regular files directly in dir.
func dirUsage(dir string) (int64, int64) {
	if dir == "" {
		return 0, 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var bytes, files int64
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		bytes += fi.Size()
		files++
	}
	return bytes, files
}
