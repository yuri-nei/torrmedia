package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type viewedEntry struct {
	Path    string `json:"path"`
	Item    string `json:"item"`
	Size    int64  `json:"size"`
	Served  int64  `json:"served"`
	Pos     int64  `json:"pos"`
	State   string `json:"state"`
	Manual  bool   `json:"manual"`
	Updated int64  `json:"updated"`
}

// viewedFile is the STORAGE format. The response envelope is built
// separately: there is no reason to put the ok field, which only the
// protocol needs, into the file on disk.
type viewedFile struct {
	Items []viewedEntry `json:"items"`
}

type viewedStore struct {
	mu    sync.Mutex
	path  string
	items map[string]viewedEntry
}

func newViewedStore(path string) (*viewedStore, error) {
	v := &viewedStore{path: path, items: make(map[string]viewedEntry)}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	var stored viewedFile
	if err := json.Unmarshal(data, &stored); err != nil {
		// Fail-open. Broken JSON used to lead to log.Fatalf, and with
		// Restart=on-failure that is an endless crash loop: the whole media
		// library is unavailable because of the viewed-marks file. Move the
		// corrupt file aside and start with an empty state.
		corrupt := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, corrupt); renameErr != nil {
			log.Printf("viewed: %q is corrupt (%v) and could not be moved aside (%v); "+
				"starting with empty state, the file will be overwritten", path, err, renameErr)
		} else {
			log.Printf("viewed: %q is corrupt (%v); moved to %q, starting with empty state",
				path, err, corrupt)
		}
		return v, nil
	}
	for _, entry := range stored.Items {
		if entry.Path != "" {
			v.items[entry.Path] = entry
		}
	}
	return v, nil
}

func (v *viewedStore) snapshot() map[string]viewedEntry {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]viewedEntry, len(v.items))
	for path, entry := range v.items {
		out[path] = entry
	}
	return out
}

// update applies a change and rolls it back if saving fails. Without the
// rollback, memory and disk diverged: /list already showed the new status,
// while the client got a 500 and assumed nothing had changed.
func (v *viewedStore) update(fn func(map[string]viewedEntry)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	backup := make(map[string]viewedEntry, len(v.items))
	for path, entry := range v.items {
		backup[path] = entry
	}
	fn(v.items)
	if err := v.saveLocked(); err != nil {
		v.items = backup
		return err
	}
	return nil
}

func (v *viewedStore) saveLocked() error {
	dir := filepath.Dir(v.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	entries := make([]viewedEntry, 0, len(v.items))
	for _, entry := range v.items {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	tmp, err := os.CreateTemp(dir, ".viewed-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(viewedFile{Items: entries}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, v.path); err != nil {
		return err
	}
	// fsync the directory: without it the rename may not survive a power
	// loss, leaving on disk exactly the truncated stub that used to send the
	// service into a crash loop.
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}

type playbackConfig struct {
	watchedFraction     float64
	endFraction         float64
	startedBytes        int64
	startedFraction     float64
	minPlayChunk        int64
	manualResetFraction float64
	flushInterval       time.Duration
}

func defaultPlaybackConfig() playbackConfig {
	return playbackConfig{
		watchedFraction:     0.70,
		endFraction:         0.90,
		startedBytes:        16 << 20,
		startedFraction:     0.003,
		minPlayChunk:        8 << 20,
		manualResetFraction: 0.05,
		flushInterval:       30 * time.Second,
	}
}

var errBadPath = errors.New("bad path")

func cleanRelative(rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) {
		return "", errBadPath
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errBadPath
	}
	return clean, nil
}

func itemForPath(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return rel
	}
	return filepath.FromSlash(strings.Join(parts[:2], "/"))
}

func (s *server) viewedSize(rel string) int64 {
	info, err := os.Stat(filepath.Join(s.root, rel))
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

// markViewed, clearViewedFile and clearViewedItem are the only mutations that
// do not touch qBittorrent: they change only local state, do not take
// minutes and never complete partially. That is why they have no operation
// and respond immediately.
func (s *server) markViewed(shelfRel, state string) error {
	rel := filepath.FromSlash(shelfRel)
	size := s.viewedSize(rel)
	now := time.Now().Unix()
	return s.viewed.update(func(items map[string]viewedEntry) {
		entry := items[rel]
		entry.Path = rel
		entry.Item = itemForPath(rel)
		if size > 0 {
			entry.Size = size
		}
		entry.State = state
		entry.Manual = true
		entry.Updated = now
		items[rel] = entry
	})
}

func (s *server) clearViewedFile(shelfRel string) error {
	rel := filepath.FromSlash(shelfRel)
	now := time.Now().Unix()
	return s.viewed.update(func(items map[string]viewedEntry) {
		entry := items[rel]
		entry.Path = rel
		entry.Item = itemForPath(rel)
		if entry.Size == 0 {
			entry.Size = s.viewedSize(rel)
		}
		entry.State = ""
		entry.Manual = true
		entry.Served = 0
		entry.Pos = 0
		entry.Updated = now
		items[rel] = entry
	})
}

func (s *server) clearViewedItem(shelfRel string) error {
	prefix := filepath.FromSlash(shelfRel)
	return s.viewed.update(func(items map[string]viewedEntry) {
		for path := range items {
			if path == prefix || strings.HasPrefix(path, prefix+string(filepath.Separator)) {
				delete(items, path)
			}
		}
	})
}

type playbackTracker struct {
	server  *server
	rel     string
	item    string
	size    int64
	start   int64
	total   atomic.Int64
	flushed int64
	stop    chan struct{}
	done    chan struct{}
}

func newPlaybackTracker(s *server, rel string, size, start int64) *playbackTracker {
	t := &playbackTracker{
		server: s,
		rel:    rel,
		item:   itemForPath(rel),
		size:   size,
		start:  start,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go t.run()
	return t
}

func (t *playbackTracker) run() {
	defer close(t.done)
	ticker := time.NewTicker(t.server.playback.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.flush()
		case <-t.stop:
			return
		}
	}
}

func (t *playbackTracker) finish() {
	close(t.stop)
	<-t.done
	t.flush()
}

func (t *playbackTracker) flush() {
	total := t.total.Load()
	delta := total - t.flushed
	if delta <= 0 {
		return
	}
	t.flushed = total
	if err := t.server.trackPlayback(t.rel, t.item, t.size, t.start, delta, total); err != nil {
		log.Printf("track playback %q: %v", t.rel, err)
	}
}

type countingWriter struct {
	http.ResponseWriter
	total *atomic.Int64
}

// Unwrap is needed by http.ResponseController and the Flusher/Hijacker
// checks: without it the wrapper broke them precisely for tracked streams,
// i.e. in production.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.total.Add(int64(n))
	return n, err
}

// ReadFrom is never called explicitly and so looks dead, but it is
// load-bearing. http.ServeContent copies via io.Copy, which prefers
// io.ReaderFrom. Without this method io.Copy would take ReadFrom from the
// EMBEDDED ResponseWriter, the bytes would bypass Write, and the viewing
// counter would always show zero.
func (w *countingWriter) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

// trackableRange: progress can be counted only for a whole-file request or a
// single range with an explicit start. A suffix range (bytes=-N) is what a
// player sends to fetch trailing metadata, and a multipart response mixes in
// framing bytes.
func trackableRange(header string) bool {
	if header == "" {
		return true
	}
	if !strings.HasPrefix(header, "bytes=") {
		return false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(spec), "-")
}

func rangeStart(header string) int64 {
	if !strings.HasPrefix(header, "bytes=") {
		return 0
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(header, "bytes="), ",", 2)[0])
	if strings.HasPrefix(first, "-") {
		return 0
	}
	start, err := strconv.ParseInt(strings.SplitN(first, "-", 2)[0], 10, 64)
	if err != nil || start < 0 {
		return 0
	}
	return start
}

func (s *server) trackPlayback(rel, item string, size, start, delta, requestTotal int64) error {
	if s.viewed == nil || delta <= 0 {
		return nil
	}
	cfg := s.playback
	return s.viewed.update(func(items map[string]viewedEntry) {
		entry, existed := items[rel]
		entry.Path = rel
		entry.Item = item
		entry.Size = size
		entry.Served += delta
		entry.Updated = time.Now().Unix()
		if requestTotal >= cfg.minPlayChunk {
			entry.Pos = max(entry.Pos, start+requestTotal)
		}
		if entry.Manual && float64(entry.Served) >= cfg.manualResetFraction*float64(size) {
			entry.Manual = false
		}
		if !entry.Manual {
			switch {
			case float64(entry.Served) >= cfg.watchedFraction*float64(size),
				float64(entry.Pos) >= cfg.endFraction*float64(size):
				entry.State = "watched"
			case float64(entry.Served) >= math.Max(float64(cfg.startedBytes), cfg.startedFraction*float64(size)):
				if entry.State != "watched" {
					entry.State = "started"
				}
			}
		}
		if existed || entry.State != "" {
			items[rel] = entry
		}
	})
}
