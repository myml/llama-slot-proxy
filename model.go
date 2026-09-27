package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// ---------------------------------------------------------------- model identity
//
// The cache key must identify the MODEL, not just the prompt.
//
// llama.cpp writes only the architecture name into a state file
// (`state_write_data`: `io.write_string(llm_arch_name(model.arch))`) and the
// read side compares nothing else — both sides carry the same TODO:
//
//	// TODO: add more model-specific info which should prevent loading the
//	// session file if not identical
//
// The file-format version IS checked (`LLAMA_STATE_SEQ_VERSION`), so a
// llama.cpp upgrade that changes the layout fails the restore cleanly. What is
// NOT checked is which model the state came from. So if the server is restarted
// on a different GGUF with the same architecture and the same dimensions —
// another quantisation, another fine-tune — the old state loads without any
// error and the KV cache of the previous model is used to answer. That is a
// silent wrong answer, not a crash.
//
// The fix is the same content-addressing already used for the system prompt:
// fold a fingerprint of the model into the key, so a model change renames every
// cache entry and the old ones simply become unreferenced garbage. Nothing has
// to be cleaned up by hand.

// modelID returns the current model fingerprint ("" while unknown).
func (p *Proxy) modelID() string {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	return p.modelIDStr
}

// modelDesc returns a human-readable description of the current model.
func (p *Proxy) modelDesc() string {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	return p.modelDescStr
}

func (p *Proxy) setModel(id, desc string) bool {
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	if p.modelIDStr == id {
		return false
	}
	p.modelIDStr, p.modelDescStr = id, desc
	return true
}

// modelWatch keeps the fingerprint current. It retries until the first success
// (the server may still be starting when the proxy comes up) and then polls, so
// that restarting llama-server on a different model is noticed without
// restarting the proxy.
func (p *Proxy) modelWatch() {
	const poll = 60 * time.Second
	for {
		id, desc, err := p.fetchModelID()
		switch {
		case err != nil:
			log.Printf("WARN model fingerprint unavailable: %v (cache keys omit it until it works)", err)
		case p.setModel(id, desc):
			log.Printf("INFO model fingerprint %s (%s)", id, desc)
		}
		time.Sleep(poll)
	}
}

// fetchModelID reads what makes this model this model. /props is the only
// interface that is stable across llama-server versions.
func (p *Proxy) fetchModelID() (string, string, error) {
	req, err := http.NewRequest(http.MethodGet, p.cfg.Addr()+"/props", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("/props: %s", resp.Status)
	}

	var props struct {
		ModelPath  string `json:"model_path"`
		ModelFtype string `json:"model_ftype"`
		BuildInfo  string `json:"build_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return "", "", err
	}
	if props.ModelPath == "" {
		return "", "", fmt.Errorf("/props: empty model_path")
	}

	// The path alone is not enough: re-quantising or re-downloading a model
	// keeps the name and changes the weights. Size and mtime catch that. When
	// the file is not visible from here (the proxy does not have to share the
	// model directory, only the cache directory) fall back to the path.
	h := sha256.New()
	h.Write([]byte(props.ModelPath))
	h.Write([]byte{0})
	h.Write([]byte(props.ModelFtype))
	h.Write([]byte{0})
	desc := props.ModelPath
	if props.ModelFtype != "" {
		desc += " [" + props.ModelFtype + "]"
	}
	if fi, err := os.Stat(props.ModelPath); err == nil {
		fmt.Fprintf(h, "%d:%d", fi.Size(), fi.ModTime().UnixNano())
		desc += fmt.Sprintf(" %d bytes", fi.Size())
	} else {
		h.Write([]byte("nostat"))
	}
	if props.BuildInfo != "" {
		desc += " (" + props.BuildInfo + ")"
	}

	return hex.EncodeToString(h.Sum(nil))[:16], desc, nil
}
