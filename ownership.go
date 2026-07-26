package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var errTorrentNotFound = errors.New("torrent not found")

type fileOwner struct {
	hash      string
	fileID    int
	priority  int
	progress  float64
	ambiguous bool
}

func (o fileOwner) known() bool { return o.hash != "" && !o.ambiguous }

type ownershipIndex struct {
	byInode map[string]fileOwner
	byPath  map[string]fileOwner
}

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

func (idx *ownershipIndex) put(key string, m map[string]fileOwner, owner fileOwner) {
	if key == "" {
		return
	}
	if existing, ok := m[key]; ok {
		if existing.hash != owner.hash || existing.fileID != owner.fileID {
			existing.ambiguous = true
			m[key] = existing
		}
		return
	}
	m[key] = owner
}

func (idx *ownershipIndex) lookup(abs string, info os.FileInfo) (fileOwner, bool) {
	if idx == nil {
		return fileOwner{}, false
	}
	if key := inodeKey(info); key != "" {
		if owner, ok := idx.byInode[key]; ok {
			return owner, true
		}
	}
	if owner, ok := idx.byPath[filepath.Clean(abs)]; ok {
		return owner, true
	}
	return fileOwner{}, false
}

func (q *qbit) ownership() (*ownershipIndex, error) {
	var torrents []qbTorrent
	if err := q.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		return nil, err
	}
	idx := &ownershipIndex{
		byInode: make(map[string]fileOwner),
		byPath:  make(map[string]fileOwner),
	}
	for _, t := range torrents {
		files, err := q.files(t.Hash)
		if err != nil {
			return nil, err
		}
		for i := range files {
			f := &files[i]
			id := i
			if f.Index != nil {
				id = *f.Index
			}
			if t.SavePath == "" || f.Name == "" || filepath.IsAbs(f.Name) {
				continue
			}
			abs := filepath.Clean(filepath.Join(t.SavePath, filepath.FromSlash(f.Name)))
			owner := fileOwner{hash: t.Hash, fileID: id, priority: f.Priority, progress: f.Progress}
			idx.put(abs, idx.byPath, owner)
			if info, err := os.Stat(abs); err == nil {
				idx.put(inodeKey(info), idx.byInode, owner)
			}
		}
	}
	return idx, nil
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

type healthStatus struct {
	OK      bool              `json:"ok"`
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks"`
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
