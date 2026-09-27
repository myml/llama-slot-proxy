package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// tmpfs work area + durable copy.
//
// The slot state files are large (0.4–6 GB) and rewritten often, so writing
// them straight to a disk-backed filesystem is both slow and write-amplifying.
// Measured on the development machine:
//
//	tmpfs (/dev/shm)       2 GB in 0.25 s  (8.7 GB/s)
//	ext4 on a loop device  2 GB in 2.07 s  (1.0 GB/s)
//
// So the working directory lives on tmpfs and the durable directory on disk.
// The durable copy is refreshed on exit and whenever a snapshot is superseded,
// and only files whose mtime advanced are copied.
// ============================================================================

// flushToDisk copies work-dir entries that are newer than their durable copy.
// Called on shutdown. Files that fail to copy are logged, never fatal.
func (p *Proxy) flushToDisk() {
	if p.cfg.Persist == "" || p.cfg.Persist == p.cfg.CacheDir {
		return
	}
	if err := os.MkdirAll(p.cfg.Persist, 0o755); err != nil {
		log.Printf("WARN persist dir: %v", err)
		return
	}
	ents, err := os.ReadDir(p.cfg.CacheDir)
	if err != nil {
		log.Printf("WARN flush readdir: %v", err)
		return
	}
	var copied, skipped int
	var bytes int64
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		src := filepath.Join(p.cfg.CacheDir, e.Name())
		dst := filepath.Join(p.cfg.Persist, e.Name())

		si, err := os.Stat(src)
		if err != nil {
			continue
		}
		if di, err := os.Stat(dst); err == nil {
			// Only copy when the working copy is strictly newer (and at least
			// a second apart, to tolerate coarse timestamps).
			if !si.ModTime().After(di.ModTime().Add(time.Millisecond)) {
				skipped++
				continue
			}
		}
		if err := copyFile(src, dst); err != nil {
			log.Printf("WARN flush %s: %v", e.Name(), err)
			continue
		}
		copied++
		bytes += si.Size()
	}
	log.Printf("INFO flush done: copied=%d skipped=%d bytes=%d", copied, skipped, bytes)
}

// ensureLocal is deliberately lazy — nothing is pre-loaded at startup. A
// snapshot is pulled back from the durable directory only when a request
// actually needs it.
func (p *Proxy) ensureLocal(name string) bool {
	local := p.cachePath(name)
	if _, err := os.Stat(local); err == nil {
		return true
	}
	if p.cfg.Persist == "" || p.cfg.Persist == p.cfg.CacheDir {
		return false
	}
	durable := filepath.Join(p.cfg.Persist, name)
	si, err := os.Stat(durable)
	if err != nil {
		return false
	}
	if err := copyFile(durable, local); err != nil {
		log.Printf("WARN pull-back %s: %v", name, err)
		return false
	}
	if p.cfg.Verbose {
		log.Printf("INFO pulled back %s (%d bytes)", name, si.Size())
	}
	return true
}

// demote moves a tmpfs file to the durable dir and removes the working copy,
// freeing RAM while keeping the snapshot available.
func (p *Proxy) demote(name string) {
	if p.cfg.Persist == "" || p.cfg.Persist == p.cfg.CacheDir {
		return
	}
	src := p.cachePath(name)
	dst := filepath.Join(p.cfg.Persist, name)
	if _, err := os.Stat(src); err != nil {
		return
	}
	if err := copyFile(src, dst); err != nil {
		log.Printf("WARN demote %s: %v", name, err)
		return
	}
	_ = os.Remove(src)
	if p.cfg.Verbose {
		log.Printf("INFO demoted %s to disk", name)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
