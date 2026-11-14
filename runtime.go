package main

// runtime.go holds the registry of active streams, the table of per-torrent
// locks, the qBittorrent liveness check and the contract number.

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// errTorrentNotFound separates "no such torrent" from "qBittorrent did not
// answer". Without this distinction any network error turned into a 404.
var errTorrentNotFound = errors.New("torrent not found")

// inodeKey returns a stable file key that survives hardlinks and renames.
// An empty string means "key unavailable".
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

// --- registry of active streams ---

// streamRegistry prevents zeroing an inode the player is reading right now.
// The key is the inode, not the path: the file may have a hardlink from the
// torrent directory.
//
// A single "nobody is reading it now" check is not enough: seconds pass
// between it and the zeroing (stopping the torrent, changing priorities), and
// the player has time to start reading. Hence the claim: a delete CLAIMS the
// inode, and from then on no new stream opens on it, while a stream already
// running prevents the claim.
type streamRegistry struct {
	mu       sync.Mutex
	n        map[string]int
	reserved map[string]int
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{n: map[string]int{}, reserved: map[string]int{}}
}

// acquire opens a read. ok=false means the inode is claimed for deletion.
func (r *streamRegistry) acquire(key string) (release func(), ok bool) {
	if key == "" {
		return func() {}, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reserved[key] > 0 {
		return nil, false
	}
	r.n[key]++
	return func() {
		r.mu.Lock()
		if r.n[key] <= 1 {
			delete(r.n, key)
		} else {
			r.n[key]--
		}
		r.mu.Unlock()
	}, true
}

// reserve claims the inode for deletion. ok=false: it is being read right now.
func (r *streamRegistry) reserve(key string) (release func(), ok bool) {
	if key == "" {
		return func() {}, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n[key] > 0 {
		return nil, false
	}
	r.reserved[key]++
	return func() {
		r.mu.Lock()
		if r.reserved[key] <= 1 {
			delete(r.reserved, key)
		} else {
			r.reserved[key]--
		}
		r.mu.Unlock()
	}, true
}

func (r *streamRegistry) active(key string) bool {
	if key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[key] > 0
}

// --- table of per-torrent locks ---

// lockTable keeps an entry only while it has holders or waiters; the sync.Map
// used before piled up a mutex for every infohash ever seen.
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

// contractVersion is a handshake between the client and the server (see
// API.md). The point is that a forgotten reinstall of the client APK fails at
// once and loudly instead of half-working. The number itself versions
// nothing: all development before the release is the first version, and the
// number is pinned at 1. Do NOT bump it on contract changes: the project has
// no backward compatibility, the client and server are rolled out together,
// and a bumped number would break an already installed APK for no reason.
const contractVersion = 1

type healthStatus struct {
	Version  string            `json:"version"`
	Contract int               `json:"contract"`
	Checks   map[string]string `json:"checks"`
}

// qbitProbe caches the result of a live qBittorrent check: monitoring polls
// /health, and logging in on every request would hit qBittorrent's rate limit.
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
