package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

var errTorrentNotFound = errors.New("torrent not found")

func inodeKey(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
}

type streamRegistry struct {
	mu sync.Mutex
	n  map[string]int
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{n: make(map[string]int)}
}

func (r *streamRegistry) acquire(key string) func() {
	if key == "" {
		return func() {}
	}
	r.mu.Lock()
	r.n[key]++
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.n[key] <= 1 {
			delete(r.n, key)
		} else {
			r.n[key]--
		}
		r.mu.Unlock()
	}
}

func (r *streamRegistry) active(key string) bool {
	if key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[key] > 0
}

type lockTable struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func newLockTable() *lockTable { return &lockTable{m: make(map[string]*lockEntry)} }

func (t *lockTable) lock(key string) func() {
	t.mu.Lock()
	entry := t.m[key]
	if entry == nil {
		entry = &lockEntry{}
		t.m[key] = entry
	}
	entry.refs++
	t.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		t.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(t.m, key)
		}
		t.mu.Unlock()
	}
}

// --- health ---

const contractVersion = 1

type healthStatus struct {
	Version  string            `json:"version"`
	Contract int               `json:"contract"`
	Checks   map[string]string `json:"checks"`
}

type qbitProbe struct {
	mu   sync.Mutex
	at   time.Time
	err  error
	ttl  time.Duration
	done bool
}

func (p *qbitProbe) check(q *qbit) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ttl := p.ttl
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	if p.done && time.Since(p.at) < ttl {
		return p.err
	}
	var version string
	err := q.getText("/api/v2/app/version", &version)
	p.at = time.Now()
	p.err = err
	p.done = true
	return err
}
