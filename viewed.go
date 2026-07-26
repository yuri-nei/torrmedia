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
		return nil, fmt.Errorf("decode: %w", err)
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

func (v *viewedStore) list(path, item string) []viewedEntry {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]viewedEntry, 0, len(v.items))
	for _, entry := range v.items {
		if path != "" && entry.Path != path {
			continue
		}
		if item != "" && entry.Item != item {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (v *viewedStore) update(fn func(map[string]viewedEntry)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	fn(v.items)
	return v.saveLocked()
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
	return os.Rename(tmpName, v.path)
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

func (s *server) enrichViewed(items []item) {
	if s.viewed == nil {
		return
	}
	viewed := s.viewed.snapshot()
	for i := range items {
		for j := range items[i].Files {
			entry, ok := viewed[items[i].Files[j].Path]
			if !ok || entry.State == "" {
				continue
			}
			items[i].Files[j].Watch = entry.State
			items[i].Files[j].Pos = entry.Pos
		}
	}
}

func (s *server) viewedSize(rel string) int64 {
	abs, err := s.resolve(rel)
	if err != nil {
		return 0
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

func (s *server) handleViewed(w http.ResponseWriter, r *http.Request) {
	if s.viewed == nil {
		writeErr(w, http.StatusInternalServerError, "viewed store is not configured")
		return
	}
	pathRaw := r.URL.Query().Get("path")
	itemRaw := r.URL.Query().Get("item")
	var path, item string
	var err error
	if pathRaw != "" {
		if path, err = cleanRelative(pathRaw); err != nil {
			writeErr(w, http.StatusBadRequest, "path %q: %v", pathRaw, err)
			return
		}
	}
	if itemRaw != "" {
		if item, err = cleanRelative(itemRaw); err != nil {
			writeErr(w, http.StatusBadRequest, "item %q: %v", itemRaw, err)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, viewedFile{Items: s.viewed.list(path, item)})
	case http.MethodPost:
		if path == "" || item != "" {
			writeErr(w, http.StatusBadRequest, "exactly one path is required")
			return
		}
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "watched"
		}
		if state != "started" && state != "watched" {
			writeErr(w, http.StatusBadRequest, "unknown state %q", state)
			return
		}
		now := time.Now().Unix()
		size := s.viewedSize(path)
		err := s.viewed.update(func(items map[string]viewedEntry) {
			entry := items[path]
			entry.Path = path
			entry.Item = itemForPath(path)
			if size > 0 {
				entry.Size = size
			}
			entry.State = state
			entry.Manual = true
			entry.Updated = now
			items[path] = entry
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "save viewed: %v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if (path == "") == (item == "") {
			writeErr(w, http.StatusBadRequest, "exactly one path or item is required")
			return
		}
		now := time.Now().Unix()
		err := s.viewed.update(func(items map[string]viewedEntry) {
			if path != "" {
				entry := items[path]
				entry.Path = path
				entry.Item = itemForPath(path)
				if entry.Size == 0 {
					entry.Size = s.viewedSize(path)
				}
				entry.State = ""
				entry.Manual = true
				entry.Served = 0
				entry.Pos = 0
				entry.Updated = now
				items[path] = entry
				return
			}
			for entryPath, entry := range items {
				if entry.Item == item {
					delete(items, entryPath)
				}
			}
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "save viewed: %v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost+", "+http.MethodDelete)
		writeErr(w, http.StatusMethodNotAllowed, "method %s is not allowed", r.Method)
	}
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

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.total.Add(int64(n))
	return n, err
}

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

func parsePlaybackConfig(cfg *playbackConfig) error {
	for key, target := range map[string]*float64{
		"TORRMEDIA_WATCHED_FRACTION":      &cfg.watchedFraction,
		"TORRMEDIA_END_FRACTION":          &cfg.endFraction,
		"TORRMEDIA_STARTED_FRACTION":      &cfg.startedFraction,
		"TORRMEDIA_MANUAL_RESET_FRACTION": &cfg.manualResetFraction,
	} {
		if value := os.Getenv(key); value != "" {
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || parsed <= 0 || parsed > 1 {
				return fmt.Errorf("%s must be a number in (0, 1]", key)
			}
			*target = parsed
		}
	}
	for key, target := range map[string]*int64{
		"TORRMEDIA_STARTED_BYTES":  &cfg.startedBytes,
		"TORRMEDIA_MIN_PLAY_CHUNK": &cfg.minPlayChunk,
	} {
		if value := os.Getenv(key); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("%s must be a positive integer", key)
			}
			*target = parsed
		}
	}
	if value := os.Getenv("TORRMEDIA_VIEWED_FLUSH_INTERVAL"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("TORRMEDIA_VIEWED_FLUSH_INTERVAL must be a positive duration")
		}
		cfg.flushInterval = parsed
	}
	return nil
}
