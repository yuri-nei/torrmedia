package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var mediaMime = map[string]string{
	".mkv":  "video/x-matroska",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".avi":  "video/x-msvideo",
	".ts":   "video/mp2t",
	".m2ts": "video/mp2t",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".wmv":  "video/x-ms-wmv",
	".mp3":  "audio/mpeg",
	".flac": "audio/flac",
	".aac":  "audio/aac",
	".mka":  "audio/x-matroska",
	".srt":  "application/x-subrip",
	".vtt":  "text/vtt",
	".ass":  "text/x-ssa",
}

type fileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Watch string `json:"watch,omitempty"`
	Pos   int64  `json:"pos,omitempty"`

	TorrentHash string `json:"torrent_hash"`
	QbitFileID  int    `json:"qbit_file_id"`
}

type item struct {
	Title       string      `json:"title"`
	Path        string      `json:"path"`
	TorrentHash string      `json:"torrent_hash"`
	Files       []fileEntry `json:"files"`
}

type server struct {
	root   string
	osRoot *os.Root
	token  string
	qb     *qbit

	locks *lockTable

	streams *streamRegistry

	deleteMu       sync.Mutex
	deleteBatches  map[string]*deleteBatch
	deleteDebounce time.Duration

	operationTimeout time.Duration
	pollInterval     time.Duration
	enableReserve    int64
	availableBytes   func(string) (int64, error)

	viewed   *viewedStore
	playback playbackConfig

	draining atomic.Bool
	inflight sync.WaitGroup
	probe    qbitProbe
	initOnce sync.Once
}

func (s *server) ensureRuntime() {
	s.initOnce.Do(func() {
		if s.locks == nil {
			s.locks = newLockTable()
		}
		if s.streams == nil {
			s.streams = newStreamRegistry()
		}
		if s.osRoot == nil && s.root != "" {
			if r, err := os.OpenRoot(s.root); err == nil {
				s.osRoot = r
			} else {
				log.Printf("open root %q: %v", s.root, err)
			}
		}
	})
}

func (s *server) lockTorrent(hash string) func() {
	s.ensureRuntime()
	return s.locks.lock(hash)
}

func (s *server) streamReg() *streamRegistry {
	s.ensureRuntime()
	return s.streams
}

func (s *server) fsRoot() *os.Root {
	s.ensureRuntime()
	return s.osRoot
}

const version = "2026-07-27.1"

var errBadPath = errors.New("bad path")

func (s *server) resolve(rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) {
		return "", errBadPath
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errBadPath
	}
	real, err := filepath.EvalSymlinks(filepath.Join(s.root, clean))
	if err != nil {
		return "", err
	}
	if real != s.root && !strings.HasPrefix(real, s.root+"/") {
		return "", errBadPath
	}
	return real, nil
}

func hidden(name string) bool { return strings.HasPrefix(name, ".") }

func complete(info os.FileInfo) bool {
	if info.Size() == 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return st.Blocks*512 >= info.Size()
}

func (s *server) collectFiles(dir string) ([]fileEntry, error) {
	files := []fileEntry{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %q: %w", p, err)
		}
		if p != dir && hidden(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", p, err)
		}
		if !complete(info) {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return fmt.Errorf("rel %q: %w", p, err)
		}
		files = append(files, newFileEntry(d.Name(), rel, info))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func newFileEntry(name, rel string, info os.FileInfo) fileEntry {
	return fileEntry{
		Name: name, Path: rel, Size: info.Size(), Mtime: info.ModTime().Unix(),
		QbitFileID: -1,
	}
}

func (s *server) fileItem(abs string, info os.FileInfo) (item, bool) {
	if !complete(info) {
		return item{}, false
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return item{}, false
	}
	fe := newFileEntry(info.Name(), rel, info)
	return item{Title: info.Name(), Path: rel, Files: []fileEntry{fe}}, true
}

func (s *server) dirItem(abs string) (item, bool, error) {
	fl, err := s.collectFiles(abs)
	if err != nil {
		return item{}, false, err
	}
	if len(fl) == 0 {
		return item{}, false, nil
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return item{}, false, err
	}
	return item{Title: filepath.Base(abs), Path: rel, Files: fl}, true, nil
}

func (s *server) buildItems() ([]item, error) {
	items := []item{}
	addEntry := func(parent string, e fs.DirEntry) error {
		abs := filepath.Join(parent, e.Name())
		if e.IsDir() {
			it, ok, err := s.dirItem(abs)
			if err != nil {
				return err
			}
			if ok {
				items = append(items, it)
			}
			return nil
		}
		if e.Type().IsRegular() {
			info, err := e.Info()
			if err != nil {
				return fmt.Errorf("stat %q: %w", abs, err)
			}
			if it, ok := s.fileItem(abs, info); ok {
				items = append(items, it)
			}
		}
		return nil
	}
	top, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read root: %w", err)
	}
	for _, e := range top {
		if hidden(e.Name()) {
			continue
		}
		if e.IsDir() {
			cat := filepath.Join(s.root, e.Name())
			sub, err := os.ReadDir(cat)
			if err != nil {
				return nil, fmt.Errorf("read %q: %w", cat, err)
			}
			for _, c := range sub {
				if hidden(c.Name()) {
					continue
				}
				if err := addEntry(cat, c); err != nil {
					return nil, err
				}
			}
		} else if err := addEntry(s.root, e); err != nil {
			return nil, err
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Title < items[j].Title })
	return items, nil
}

func (s *server) enrichOwnership(items []item) ([]item, bool) {
	if s.qb == nil {
		return items, true
	}
	idx, err := s.qb.ownership()
	if err != nil {
		log.Printf("list: ownership unavailable: %v", err)
		return items, false
	}
	for i := range items {
		hashes := map[string]struct{}{}
		kept := items[i].Files[:0]
		for j := range items[i].Files {
			fe := items[i].Files[j]
			abs := filepath.Join(s.root, fe.Path)
			info, err := os.Stat(abs)
			if err != nil {
				continue
			}
			owner, found := idx.lookup(abs, info)
			if found && owner.known() {
				if owner.progress < 1 {
					continue
				}
				fe.TorrentHash = owner.hash
				fe.QbitFileID = owner.fileID
				hashes[owner.hash] = struct{}{}
			}
			kept = append(kept, fe)
		}
		items[i].Files = kept
		if len(hashes) == 1 {
			for h := range hashes {
				items[i].TorrentHash = h
			}
		}
	}
	kept := items[:0]
	for _, it := range items {
		if len(it.Files) > 0 {
			kept = append(kept, it)
		}
	}
	return kept, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]any{"ok": false, "error": fmt.Sprintf(format, a...)})
}

func pathErrCode(err error) int {
	if errors.Is(err, errBadPath) {
		return http.StatusBadRequest
	}
	if errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	var items []item
	if rel == "" {
		built, err := s.buildItems()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list: %v", err)
			return
		}
		items = built
	} else {
		abs, err := s.resolve(rel)
		if err != nil {
			writeErr(w, pathErrCode(err), "list %q: %v", rel, err)
			return
		}
		info, err := os.Stat(abs)
		if err != nil {
			writeErr(w, pathErrCode(err), "list %q: %v", rel, err)
			return
		}
		items = []item{}
		if info.IsDir() {
			it, ok, err := s.dirItem(abs)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "list %q: %v", rel, err)
				return
			}
			if ok {
				items = append(items, it)
			}
		} else if it, ok := s.fileItem(abs, info); ok {
			items = append(items, it)
		}
	}
	items, qbitAvailable := s.enrichOwnership(items)
	s.enrichViewed(items)
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "qbit_available": qbitAvailable,
	})
}

func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs, err := s.resolve(rel)
	if err != nil {
		writeErr(w, pathErrCode(err), "file %q: %v", rel, err)
		return
	}
	f, err := s.openInRoot(rel)
	if err != nil {
		writeErr(w, pathErrCode(err), "file %q: %v", rel, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !complete(info) {
		writeErr(w, http.StatusNotFound, "file %q: not a complete regular file", rel)
		return
	}
	defer s.streamReg().acquire(inodeKey(info))()
	ext := strings.ToLower(filepath.Ext(abs))
	ct := mediaMime[ext]
	if ct == "" {
		ct = mime.TypeByExtension(ext)
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano()))
	if r.Method != http.MethodGet || s.viewed == nil || !trackableRange(r.Header.Get("Range")) {
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
		return
	}
	tracker := newPlaybackTracker(s, rel, info.Size(), rangeStart(r.Header.Get("Range")))
	defer tracker.finish()
	cw := &countingWriter{ResponseWriter: w, total: &tracker.total}
	http.ServeContent(cw, r, info.Name(), info.ModTime(), f)
}

func (s *server) openInRoot(rel string) (*os.File, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}
	root := s.fsRoot()
	if root == nil {
		return nil, fmt.Errorf("media root %q is not available", s.root)
	}
	return root.Open(clean)
}

func (s *server) pruneEmptyDirs(dir string) {
	for strings.HasPrefix(dir, s.root+"/") && filepath.Dir(dir) != s.root {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

type trimResult struct {
	logicalSize int64
	retained    int64
	freed       int64
}

type deleteTarget struct {
	rel     string
	abs     string
	info    os.FileInfo
	match   qbMatch
	waiters []chan *deleteBatchOutcome
}

type deleteBatch struct {
	generation uint64
	timer      *time.Timer
	targets    map[string]*deleteTarget
}

type deleteBatchOutcome struct {
	status   int
	err      string
	response map[string]any

	perTarget map[string]*deleteBatchOutcome
}

func (o *deleteBatchOutcome) forTarget(rel string) *deleteBatchOutcome {
	if o != nil && o.perTarget != nil {
		if own, ok := o.perTarget[rel]; ok {
			return own
		}
	}
	return o
}

type preparedDelete struct {
	target   *deleteTarget
	file     *os.File
	info     os.FileInfo
	priority int
	size     int64
}

func allocatedBytes(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

func readAtFull(f *os.File, p []byte, off int64) error {
	n, err := f.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(p) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

var errTooSmallToTrim = errors.New("file fits in two pieces; boundary data is shared with neighbours")

func trimSparseFile(f *os.File, expected os.FileInfo, pieceSize int64) (trimResult, error) {
	if pieceSize <= 0 || pieceSize > 64<<20 {
		return trimResult{}, fmt.Errorf("unsafe piece size %d", pieceSize)
	}
	current, err := f.Stat()
	if err != nil {
		return trimResult{}, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(expected, current) {
		return trimResult{}, errors.New("file changed while delete was prepared")
	}
	size := current.Size()
	before := allocatedBytes(current)

	if size <= 2*pieceSize {
		return trimResult{}, errTooSmallToTrim
	}

	prefix := make([]byte, int(pieceSize))
	suffix := make([]byte, int(pieceSize))
	suffixOff := size - pieceSize
	if err := readAtFull(f, prefix, 0); err != nil {
		return trimResult{}, fmt.Errorf("read prefix: %w", err)
	}
	if err := readAtFull(f, suffix, suffixOff); err != nil {
		return trimResult{}, fmt.Errorf("read suffix: %w", err)
	}

	if err := f.Truncate(0); err != nil {
		return trimResult{}, fmt.Errorf("truncate to zero: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		return trimResult{}, fmt.Errorf("restore logical size: %w", err)
	}
	if len(prefix) > 0 {
		if _, err := f.WriteAt(prefix, 0); err != nil {
			return trimResult{}, fmt.Errorf("restore prefix: %w", err)
		}
		if _, err := f.WriteAt(suffix, suffixOff); err != nil {
			return trimResult{}, fmt.Errorf("restore suffix: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		return trimResult{}, fmt.Errorf("sync: %w", err)
	}
	afterInfo, err := f.Stat()
	if err != nil {
		return trimResult{}, err
	}
	after := allocatedBytes(afterInfo)
	freed := before - after
	if freed < 0 {
		freed = 0
	}
	return trimResult{logicalSize: size, retained: int64(len(prefix) + len(suffix)), freed: freed}, nil
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimSpace(r.URL.Query().Get("hash"))
	idRaw := strings.TrimSpace(r.URL.Query().Get("id"))
	if hash != "" || idRaw != "" {
		s.handleTorrentFileDelete(w, r, hash, idRaw)
		return
	}

	rel := r.URL.Query().Get("path")
	abs, err := s.resolve(rel)
	if err != nil {
		writeErr(w, pathErrCode(err), "delete %q: %v", rel, err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "delete %q: not a regular file", rel)
		return
	}

	var m *qbMatch
	if s.qb != nil {
		if s.qb.pass == "CHANGE_ME" {
			writeErr(w, http.StatusBadGateway, "QBIT_PASS is not configured in /etc/torrmedia/config.env")
			return
		}
		m, err = s.qb.findFile(info)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "qbittorrent check failed, nothing deleted: %v", err)
			return
		}
	}

	if m == nil {
		if s.streamReg().active(inodeKey(info)) {
			writeErr(w, http.StatusConflict, "file %q is being streamed right now", rel)
			return
		}
		if err := os.Remove(abs); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete %q: %v", rel, err)
			return
		}
		s.pruneEmptyDirs(filepath.Dir(abs))
		log.Printf("delete: %s (not owned by a live torrent)", rel)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": rel, "torrent": nil, "warnings": []string{}})
		return
	}

	canonicalRel, err := filepath.Rel(s.root, abs)
	if err != nil {
		writeErr(w, http.StatusForbidden, "delete %q: outside torrmedia root", rel)
		return
	}
	s.queueTorrentDelete(w, r, &deleteTarget{
		rel: canonicalRel, abs: abs, info: info, match: *m,
	})
}

func (s *server) handleTorrentFileDelete(
	w http.ResponseWriter, r *http.Request, hash, idRaw string,
) {
	if s.qb == nil {
		writeErr(w, http.StatusNotImplemented, "qbittorrent is not configured")
		return
	}
	if s.qb.pass == "CHANGE_ME" {
		writeErr(w, http.StatusBadGateway, "QBIT_PASS is not configured in /etc/torrmedia/config.env")
		return
	}
	id, err := strconv.Atoi(idRaw)
	if hash == "" || err != nil || id < 0 {
		writeErr(w, http.StatusBadRequest, "valid hash and non-negative file id are required")
		return
	}
	torrent, err := s.qb.torrent(hash)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errTorrentNotFound) {
			status = http.StatusNotFound
		}
		writeErr(w, status, "torrent %q: %v", hash, err)
		return
	}
	files, err := s.qb.files(hash)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent files failed, nothing queued: %v", err)
		return
	}
	qbFile, ok := qbFileByID(files, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no file id %d in torrent %q", id, hash)
		return
	}
	abs, err := s.torrentFilePath(torrent, qbFile)
	if err != nil {
		writeErr(w, http.StatusForbidden, "delete refused: %v", err)
		return
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		writeErr(w, http.StatusForbidden, "delete refused: torrent file is outside torrmedia root")
		return
	}

	var info os.FileInfo
	info, err = os.Stat(abs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeErr(w, http.StatusInternalServerError, "stat %q, nothing queued: %v", rel, err)
		return
	}
	if err == nil && !info.Mode().IsRegular() {
		writeErr(w, http.StatusConflict, "delete %q: torrent path is not a regular file", rel)
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		info = nil
	}

	s.queueTorrentDelete(w, r, &deleteTarget{
		rel:  rel,
		abs:  abs,
		info: info,
		match: qbMatch{
			hash: hash, fileID: id, abs: abs, priority: qbFile.Priority,
		},
	})
}

func (s *server) queueTorrentDelete(w http.ResponseWriter, r *http.Request, target *deleteTarget) {
	result := make(chan *deleteBatchOutcome, 1)
	target.waiters = []chan *deleteBatchOutcome{result}
	s.enqueueTorrentDelete(target)
	select {
	case outcome := <-result:
		own := outcome.forTarget(target.rel)
		if own.err != "" {
			writeErr(w, own.status, "%s", own.err)
			return
		}
		resp := make(map[string]any, len(own.response)+2)
		for key, value := range own.response {
			resp[key] = value
		}
		resp["ok"] = true
		resp["deleted"] = target.rel
		writeJSON(w, own.status, resp)
	case <-r.Context().Done():
	}
}

func (s *server) cancelQueuedDelete(hash string, fileID int) bool {
	s.deleteMu.Lock()
	batch := s.deleteBatches[hash]
	if batch == nil {
		s.deleteMu.Unlock()
		return false
	}
	var cancelled *deleteTarget
	for rel, target := range batch.targets {
		if target.match.fileID == fileID {
			cancelled = target
			delete(batch.targets, rel)
			break
		}
	}
	if cancelled != nil && len(batch.targets) == 0 {
		if batch.timer != nil {
			batch.timer.Stop()
		}
		delete(s.deleteBatches, hash)
	}
	s.deleteMu.Unlock()

	if cancelled == nil {
		return false
	}
	outcome := deleteBatchError(http.StatusConflict,
		"queued delete for %q was cancelled by an enable request", cancelled.rel)
	for _, waiter := range cancelled.waiters {
		waiter <- outcome
	}
	log.Printf("delete: queued %s cancelled by enable (torrent=%s file=%d)", cancelled.rel, hash, fileID)
	return true
}

func (s *server) pendingBatch(hash string) bool {
	s.deleteMu.Lock()
	defer s.deleteMu.Unlock()
	return s.deleteBatches[hash] != nil
}

func (s *server) cancelAllQueuedDeletes() {
	s.deleteMu.Lock()
	batches := s.deleteBatches
	s.deleteBatches = nil
	s.deleteMu.Unlock()

	for _, batch := range batches {
		if batch.timer != nil {
			batch.timer.Stop()
		}
		outcome := deleteBatchError(http.StatusServiceUnavailable,
			"torrmedia is shutting down; nothing was deleted, retry after restart")
		for _, target := range batch.targets {
			for _, waiter := range target.waiters {
				waiter <- outcome
			}
		}
	}
}

func (s *server) enqueueTorrentDelete(target *deleteTarget) {
	debounce := s.deleteDebounce
	if debounce <= 0 {
		debounce = 30 * time.Second
	}

	s.deleteMu.Lock()
	if s.deleteBatches == nil {
		s.deleteBatches = make(map[string]*deleteBatch)
	}
	batch := s.deleteBatches[target.match.hash]
	if batch == nil {
		batch = &deleteBatch{targets: make(map[string]*deleteTarget)}
		s.deleteBatches[target.match.hash] = batch
	}
	if existing := batch.targets[target.rel]; existing != nil {
		existing.waiters = append(existing.waiters, target.waiters...)
	} else {
		batch.targets[target.rel] = target
	}

	// Generation makes an already-fired stale timer harmless if a new DELETE
	// arrived while its callback was waiting for deleteMu.
	batch.generation++
	generation := batch.generation
	if batch.timer != nil {
		batch.timer.Stop()
	}
	batch.timer = time.AfterFunc(debounce, func() {
		s.flushDeleteBatch(target.match.hash, batch, generation)
	})
	queued := len(batch.targets)
	s.deleteMu.Unlock()

	log.Printf("delete: queued %s (torrent=%s batch=%d debounce=%s)",
		target.rel, target.match.hash, queued, debounce)
}

func (s *server) flushDeleteBatch(hash string, batch *deleteBatch, generation uint64) {
	s.deleteMu.Lock()
	if s.deleteBatches[hash] != batch || batch.generation != generation {
		s.deleteMu.Unlock()
		return
	}
	delete(s.deleteBatches, hash)
	targets := make([]*deleteTarget, 0, len(batch.targets))
	for _, target := range batch.targets {
		targets = append(targets, target)
	}
	s.deleteMu.Unlock()

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].match.fileID == targets[j].match.fileID {
			return targets[i].rel < targets[j].rel
		}
		return targets[i].match.fileID < targets[j].match.fileID
	})
	s.inflight.Add(1)
	defer s.inflight.Done()
	outcome := s.deleteTorrentBatch(hash, targets)
	for _, target := range targets {
		own := outcome.forTarget(target.rel)
		for _, waiter := range target.waiters {
			waiter <- own
		}
	}
}

func deleteBatchError(status int, format string, args ...any) *deleteBatchOutcome {
	return &deleteBatchOutcome{
		status: status,
		err:    fmt.Sprintf(format, args...),
	}
}

func closePreparedDeletes(prepared []*preparedDelete) {
	for _, item := range prepared {
		if item.file != nil {
			_ = item.file.Close()
		}
	}
}

func (s *server) rollbackDeletePriorities(hash string, changed []*preparedDelete) []string {
	var failures []string
	for i := len(changed) - 1; i >= 0; i-- {
		if err := s.qb.setFilePrio(hash, changed[i].target.match.fileID, changed[i].priority); err != nil {
			failures = append(failures, fmt.Sprintf("file %d: %v", changed[i].target.match.fileID, err))
		}
	}
	if len(failures) > 0 {
		log.Printf("delete batch: rollback of priorities for %s failed: %s", hash, strings.Join(failures, "; "))
	}
	return failures
}

func rollbackNote(failures []string) string {
	if len(failures) == 0 {
		return "priorities rolled back"
	}
	return "ROLLBACK INCOMPLETE (" + strings.Join(failures, "; ") + ")"
}

func (s *server) deleteTorrentBatch(hash string, targets []*deleteTarget) *deleteBatchOutcome {
	if len(targets) == 0 {
		return deleteBatchError(http.StatusBadRequest, "empty delete batch")
	}

	unlock := s.lockTorrent(hash)
	defer unlock()

	deadline := time.Now().Add(s.operationTimeout)
	budget := func() (time.Duration, error) {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, fmt.Errorf("operation deadline of %s exceeded", s.operationTimeout)
		}
		return left, nil
	}

	torrent, err := s.qb.torrent(hash)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errTorrentNotFound) {
			status = http.StatusNotFound
		}
		return deleteBatchError(status, "qbittorrent refresh failed, nothing deleted: %v", err)
	}
	if qbitCheckingState(torrent.State) {
		return deleteBatchError(http.StatusConflict,
			"torrent is checking, nothing deleted")
	}
	props, err := s.qb.properties(hash)
	if err != nil {
		return deleteBatchError(http.StatusBadGateway,
			"qbittorrent properties failed, nothing deleted: %v", err)
	}
	files, err := s.qb.files(hash)
	if err != nil {
		return deleteBatchError(http.StatusBadGateway,
			"qbittorrent files failed, nothing deleted: %v", err)
	}

	prepared := make([]*preparedDelete, 0, len(targets))
	rejected := make(map[string]*deleteBatchOutcome)
	seenIDs := make(map[int]string, len(targets))
	reject := func(target *deleteTarget, status int, format string, args ...any) {
		rejected[target.rel] = deleteBatchError(status, format, args...)
	}
	for _, target := range targets {
		if target.match.hash != hash {
			reject(target, http.StatusConflict, "file %q belongs to another torrent", target.rel)
			continue
		}
		if previous, exists := seenIDs[target.match.fileID]; exists {
			reject(target, http.StatusConflict,
				"torrent file id %d is referenced by both %q and %q",
				target.match.fileID, previous, target.rel)
			continue
		}
		seenIDs[target.match.fileID] = target.rel

		qbFile, ok := qbFileByID(files, target.match.fileID)
		if !ok {
			reject(target, http.StatusConflict, "torrent file id %d disappeared", target.match.fileID)
			continue
		}
		currentAbs, err := s.torrentFilePath(torrent, qbFile)
		if err != nil || filepath.Clean(currentAbs) != filepath.Clean(target.match.abs) {
			reject(target, http.StatusConflict,
				"torrent path for file id %d changed", target.match.fileID)
			continue
		}
		currentRel, err := filepath.Rel(s.root, currentAbs)
		if err != nil {
			reject(target, http.StatusForbidden, "torrent file %q is outside torrmedia root", target.rel)
			continue
		}
		currentInfo, err := os.Stat(currentAbs)
		if errors.Is(err, fs.ErrNotExist) {
			prepared = append(prepared, &preparedDelete{
				target: target, priority: qbFile.Priority, size: qbFile.Size,
			})
			continue
		}
		if err != nil || !currentInfo.Mode().IsRegular() {
			reject(target, http.StatusConflict, "torrent path for %q is not a regular file", target.rel)
			continue
		}
		if target.info != nil && !os.SameFile(target.info, currentInfo) {
			reject(target, http.StatusConflict, "torrent inode for %q changed", target.rel)
			continue
		}
		root := s.fsRoot()
		if root == nil {
			reject(target, http.StatusInternalServerError, "media root %q is not available", s.root)
			continue
		}
		f, err := root.OpenFile(filepath.ToSlash(currentRel), os.O_RDWR, 0)
		if err != nil {
			reject(target, http.StatusInternalServerError, "open %q: %v", target.rel, err)
			continue
		}
		pinnedInfo, err := f.Stat()
		if err != nil || !pinnedInfo.Mode().IsRegular() ||
			!os.SameFile(currentInfo, pinnedInfo) {
			_ = f.Close()
			reject(target, http.StatusConflict, "file %q changed while delete was prepared", target.rel)
			continue
		}
		if s.streamReg().active(inodeKey(pinnedInfo)) {
			_ = f.Close()
			reject(target, http.StatusConflict, "file %q is being streamed right now", target.rel)
			continue
		}
		prepared = append(prepared, &preparedDelete{
			target: target, file: f, info: pinnedInfo,
			priority: qbFile.Priority, size: qbFile.Size,
		})
	}
	defer closePreparedDeletes(prepared)

	if len(prepared) == 0 {
		out := deleteBatchError(http.StatusConflict, "no valid files in delete batch, nothing deleted")
		out.perTarget = rejected
		return out
	}

	warnings := []string{}

	wasRunning := !qbitStoppedState(torrent.State)
	if wasRunning {
		if err := s.qb.switchTorrent(hash, "stop"); err != nil {
			out := deleteBatchError(http.StatusBadGateway,
				"qbittorrent stop failed, nothing deleted: %v", err)
			out.perTarget = rejected
			return out
		}
		left, err := budget()
		if err == nil {
			err = s.qb.waitStopped(hash, left, s.pollInterval)
		}
		if err != nil {
			_ = s.qb.switchTorrent(hash, "start")
			out := deleteBatchError(http.StatusGatewayTimeout,
				"qbittorrent did not stop, nothing deleted: %v", err)
			out.perTarget = rejected
			return out
		}
	}

	changed := make([]*preparedDelete, 0, len(prepared))
	for _, item := range prepared {
		if item.priority == 0 {
			continue
		}
		changed = append(changed, item)
		abort := func(status int, format string, args ...any) *deleteBatchOutcome {
			note := rollbackNote(s.rollbackDeletePriorities(hash, changed))
			if wasRunning {
				if err := s.qb.switchTorrent(hash, "start"); err != nil {
					note += fmt.Sprintf("; restart failed: %v", err)
				}
			}
			out := deleteBatchError(status, format+"; %s", append(args, note)...)
			out.perTarget = rejected
			return out
		}
		if err := s.qb.setFilePrio(hash, item.target.match.fileID, 0); err != nil {
			return abort(http.StatusBadGateway, "qbittorrent filePrio failed, nothing trimmed: %v", err)
		}
		left, err := budget()
		if err == nil {
			err = s.qb.waitFilePriority(hash, item.target.match.fileID, 0, left, s.pollInterval)
		}
		if err != nil {
			return abort(http.StatusGatewayTimeout, "qbittorrent did not accept priority=0, nothing trimmed: %v", err)
		}
	}

	type deletedFile struct {
		path    string
		trimmed trimResult
		absent  bool
		kept    bool
	}
	deleted := make([]deletedFile, 0, len(prepared))
	for _, item := range prepared {
		if item.file == nil {
			deleted = append(deleted, deletedFile{
				path: item.target.rel,
				trimmed: trimResult{
					logicalSize: item.size,
				},
				absent: true,
			})
			continue
		}
		trimmed, err := trimSparseFile(item.file, item.info, props.PieceSize)
		if errors.Is(err, errTooSmallToTrim) {
			warnings = append(warnings, fmt.Sprintf(
				"%s: excluded from the torrent, but data kept on disk (%v)", item.target.rel, err))
			deleted = append(deleted, deletedFile{
				path:    item.target.rel,
				trimmed: trimResult{logicalSize: item.size},
				kept:    true,
			})
			continue
		}
		if err != nil {
			note := "torrent left stopped"
			if rcErr := s.qb.recheck(hash); rcErr != nil {
				note = fmt.Sprintf("recheck also failed: %v", rcErr)
			} else if wasRunning {
				if startErr := s.qb.switchTorrent(hash, "start"); startErr != nil {
					note = fmt.Sprintf("restart failed: %v", startErr)
				} else {
					note = "torrent restarted to repair neighbouring pieces"
				}
			}
			out := deleteBatchError(http.StatusInternalServerError,
				"batch trim failed at %q with selected priorities=0; %s: %v",
				item.target.rel, note, err)
			out.perTarget = rejected
			return out
		}
		deleted = append(deleted, deletedFile{path: item.target.rel, trimmed: trimmed})
	}
	if err := s.qb.recheck(hash); err != nil {
		out := deleteBatchError(http.StatusBadGateway,
			"files trimmed, but qbittorrent recheck failed; torrent left stopped: %v", err)
		out.perTarget = rejected
		return out
	}
	left, err := budget()
	if err == nil {
		err = s.qb.waitRecheck(hash, prepared[0].target.match.fileID, left, s.pollInterval)
	}
	if err != nil {
		out := deleteBatchError(http.StatusGatewayTimeout,
			"files trimmed, but qbittorrent recheck did not finish; torrent left stopped: %v", err)
		out.perTarget = rejected
		return out
	}

	fileResponses := make([]map[string]any, 0, len(deleted))
	var totalFreed, totalRetained int64
	for _, item := range deleted {
		totalFreed += item.trimmed.freed
		totalRetained += item.trimmed.retained
		fileResponse := map[string]any{
			"path": item.path, "logical_size": item.trimmed.logicalSize,
			"retained_boundary_bytes": item.trimmed.retained,
			"freed_bytes_estimate":    item.trimmed.freed,
		}
		if item.absent {
			fileResponse["was_absent"] = true
		}
		if item.kept {
			fileResponse["data_kept"] = true
		}
		fileResponses = append(fileResponses, fileResponse)
	}
	resp := map[string]any{
		"torrent": hash, "batch_size": len(deleted), "batch_deleted": fileResponses,
		"storage_trimmed": true, "freed_bytes_estimate": totalFreed,
		"retained_boundary_bytes": totalRetained, "recheck": "complete",
	}
	if wasRunning {
		if err := s.qb.switchTorrent(hash, "start"); err != nil {
			warnings = append(warnings, fmt.Sprintf("torrent was not restarted: %v", err))
			log.Printf("delete batch: restart %s failed after successful trim: %v", hash, err)
		} else {
			resp["torrent_restarted"] = true
		}
	}
	resp["warnings"] = warnings
	log.Printf("delete batch: torrent=%s files=%d freed=%d retained=%d warnings=%d",
		hash, len(deleted), totalFreed, totalRetained, len(warnings))
	return &deleteBatchOutcome{status: http.StatusOK, response: resp, perTarget: rejected}
}

func availableFilesystemBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (s *server) torrentFilePath(t *qbTorrent, f *qbFile) (string, error) {
	if t.SavePath == "" || f.Name == "" || filepath.IsAbs(f.Name) {
		return "", errors.New("invalid torrent file path")
	}
	abs := filepath.Clean(filepath.Join(t.SavePath, filepath.FromSlash(f.Name)))
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("torrent file is outside torrmedia root")
	}
	return abs, nil
}

func requiredFileBytes(path string, logicalSize int64) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return logicalSize, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("torrent path is not a regular file")
	}
	required := logicalSize - allocatedBytes(info)
	if required < 0 {
		required = 0
	}
	return required, nil
}

func (s *server) handleEnable(w http.ResponseWriter, r *http.Request) {
	if s.qb == nil {
		writeErr(w, http.StatusNotImplemented, "qbittorrent is not configured")
		return
	}
	if s.qb.pass == "CHANGE_ME" {
		writeErr(w, http.StatusBadGateway, "QBIT_PASS is not configured in /etc/torrmedia/config.env")
		return
	}
	hash := strings.TrimSpace(r.URL.Query().Get("hash"))
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if hash == "" || id < 0 || err != nil {
		writeErr(w, http.StatusBadRequest, "valid hash and non-negative file id are required")
		return
	}

	s.inflight.Add(1)
	defer s.inflight.Done()
	unlock := s.lockTorrent(hash)
	defer unlock()

	if s.cancelQueuedDelete(hash, id) {
		log.Printf("enable: cancelled queued delete for %s file=%d", hash, id)
	}

	t, err := s.qb.torrent(hash)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errTorrentNotFound) {
			status = http.StatusNotFound
		}
		writeErr(w, status, "torrent: %v", err)
		return
	}
	if qbitCheckingState(t.State) {
		writeErr(w, http.StatusConflict, "torrent is checking; enable was not applied")
		return
	}
	files, err := s.qb.files(hash)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent files: %v", err)
		return
	}
	file, ok := qbFileByID(files, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no file id %d in torrent", id)
		return
	}
	abs, err := s.torrentFilePath(t, file)
	if err != nil {
		writeErr(w, http.StatusForbidden, "enable refused: %v", err)
		return
	}
	required, err := requiredFileBytes(abs, file.Size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "space check: %v", err)
		return
	}
	spaceProbe := t.DownloadPath
	if spaceProbe == "" {
		spaceProbe = t.SavePath
	}
	available, err := s.availableBytes(spaceProbe)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "free space check: %v", err)
		return
	}
	reserve := s.enableReserve
	if required == 0 {
		reserve = 0
	}
	if available < required+reserve {
		writeErr(w, http.StatusInsufficientStorage,
			"selected file needs %d bytes plus %d reserve; only %d available",
			required, reserve, available)
		return
	}

	rollback := func() string {
		if !(file.Priority == 0) {
			return "no rollback needed"
		}
		if err := s.qb.setFilePrio(hash, id, 0); err != nil {
			log.Printf("enable: rollback of priority for %s file=%d failed: %v", hash, id, err)
			return fmt.Sprintf("ROLLBACK INCOMPLETE: %v", err)
		}
		return "priority rolled back"
	}

	changed := file.Priority == 0
	if changed {
		if err := s.qb.setFilePrio(hash, id, 1); err != nil {
			writeErr(w, http.StatusBadGateway, "qbittorrent filePrio failed: %v", err)
			return
		}
		if err := s.qb.waitFilePriority(hash, id, 1, s.operationTimeout, s.pollInterval); err != nil {
			writeErr(w, http.StatusGatewayTimeout, "qbittorrent did not accept priority=1: %v; %s", err, rollback())
			return
		}
	}
	if err := s.qb.switchTorrent(hash, "start"); err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent start failed: %v; %s", err, rollback())
		return
	}
	startTimeout := 30 * time.Second
	if s.operationTimeout < startTimeout {
		startTimeout = s.operationTimeout
	}
	after, err := s.qb.waitStarted(hash, startTimeout, s.pollInterval)
	if err != nil {
		note := ""
		if stopErr := s.qb.switchTorrent(hash, "stop"); stopErr != nil {
			note = fmt.Sprintf("; stop also failed: %v", stopErr)
		}
		writeErr(w, http.StatusGatewayTimeout, "torrent did not start: %v; %s%s", err, rollback(), note)
		return
	}
	log.Printf("enable: %s file=%d required=%d available=%d state=%s", hash, id, required, available, after.State)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "warnings": []string{},
		"hash": hash, "file_id": id, "priority": 1, "started": true,
		"state": after.State, "amount_left": after.AmountLeft,
		"required_bytes": required, "available_bytes": available,
	})
}

func (s *server) torrentFor(w http.ResponseWriter, r *http.Request) (*qbTorrent, bool) {
	if s.qb == nil {
		writeErr(w, http.StatusNotImplemented, "qbittorrent is not configured")
		return nil, false
	}
	if hash := r.URL.Query().Get("hash"); hash != "" {
		var torrents []qbTorrent
		if err := s.qb.getJSON("/api/v2/torrents/info?hashes="+url.QueryEscape(hash), &torrents); err != nil {
			writeErr(w, http.StatusBadGateway, "qbittorrent: %v", err)
			return nil, false
		}
		if len(torrents) == 0 {
			writeErr(w, http.StatusNotFound, "no torrent with hash %q", hash)
			return nil, false
		}
		return &torrents[0], true
	}
	rel := r.URL.Query().Get("path")
	abs, err := s.resolve(rel)
	if err != nil {
		writeErr(w, pathErrCode(err), "%q: %v", rel, err)
		return nil, false
	}
	t, err := s.qb.findTorrentByPath(abs)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent: %v", err)
		return nil, false
	}
	if t == nil {
		writeErr(w, http.StatusNotFound, "no torrent owns %q", rel)
		return nil, false
	}
	return t, true
}

func (s *server) handleTorrents(w http.ResponseWriter, r *http.Request) {
	if s.qb == nil {
		writeErr(w, http.StatusNotImplemented, "qbittorrent is not configured")
		return
	}
	var torrents []qbTorrent
	if err := s.qb.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"torrents": torrents})
}

func (s *server) handleTorrent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.torrentFor(w, r)
	if !ok {
		return
	}
	files, err := s.qb.files(t.Hash)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent files: %v", err)
		return
	}
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		abs := filepath.Join(t.SavePath, f.Name)
		entry := map[string]any{
			"name": f.Name, "size": f.Size,
			"priority": f.Priority, "progress": f.Progress,
			"on_disk": false,
		}
		if info, err := os.Stat(abs); err == nil {
			entry["on_disk"] = true
			entry["complete_on_disk"] = complete(info)
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				entry["allocated"] = st.Blocks * 512
			}
		}
		out = append(out, entry)
	}
	var trackers []map[string]any
	s.qb.getJSON("/api/v2/torrents/trackers?hash="+url.QueryEscape(t.Hash), &trackers)
	urls := []string{}
	for _, tr := range trackers {
		if u, ok := tr["url"].(string); ok && strings.HasPrefix(u, "http") || strings.HasPrefix(fmt.Sprint(tr["url"]), "udp") {
			urls = append(urls, fmt.Sprint(tr["url"]))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hash": t.Hash, "name": t.Name, "state": t.State,
		"progress": t.Progress, "files": out, "trackers": urls,
	})
}

func (s *server) guardTorrentMutation(w http.ResponseWriter, hash string) (func(), bool) {
	if s.draining.Load() {
		writeErr(w, http.StatusServiceUnavailable, "torrmedia is shutting down")
		return nil, false
	}
	unlock := s.lockTorrent(hash)
	if s.pendingBatch(hash) {
		unlock()
		writeErr(w, http.StatusConflict,
			"a delete batch is queued for torrent %s; retry after it completes", hash)
		return nil, false
	}
	return unlock, true
}

func (s *server) handleStartStop(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := s.torrentFor(w, r)
		if !ok {
			return
		}
		unlock, ok := s.guardTorrentMutation(w, t.Hash)
		if !ok {
			return
		}
		defer unlock()
		legacy := map[string]string{"start": "resume", "stop": "pause"}[action]
		err := s.qb.postOK("/api/v2/torrents/"+action, url.Values{"hashes": {t.Hash}})
		if err != nil && legacy != "" {
			err = s.qb.postOK("/api/v2/torrents/"+legacy, url.Values{"hashes": {t.Hash}})
		}
		if err != nil {
			writeErr(w, http.StatusBadGateway, "qbittorrent %s: %v", action, err)
			return
		}
		log.Printf("%s %s (%s): ok", action, t.Hash, t.Name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": t.Hash, action: "ok"})
	}
}

func (s *server) handleRecheck(w http.ResponseWriter, r *http.Request) {
	t, ok := s.torrentFor(w, r)
	if !ok {
		return
	}
	unlock, ok := s.guardTorrentMutation(w, t.Hash)
	if !ok {
		return
	}
	defer unlock()
	if err := s.qb.recheck(t.Hash); err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent recheck: %v", err)
		return
	}
	log.Printf("recheck %s (%s): started via API", t.Hash, t.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": t.Hash, "recheck": "started"})
}

func (s *server) handleRemove(w http.ResponseWriter, r *http.Request) {
	t, ok := s.torrentFor(w, r)
	if !ok {
		return
	}
	unlock, ok := s.guardTorrentMutation(w, t.Hash)
	if !ok {
		return
	}
	defer unlock()
	if err := s.qb.deleteTorrent(t.Hash, false); err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent delete: %v", err)
		return
	}
	log.Printf("remove %s (%s): torrent dropped, files kept", t.Hash, t.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": t.Hash, "removed": true, "files_kept": true})
}

func (s *server) handlePrio(w http.ResponseWriter, r *http.Request) {
	t, ok := s.torrentFor(w, r)
	if !ok {
		return
	}
	prio, err := strconv.Atoi(r.URL.Query().Get("priority"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "priority: %v", err)
		return
	}
	ids := r.URL.Query().Get("ids")
	if ids == "" {
		writeErr(w, http.StatusBadRequest, "ids required (comma-separated file indexes)")
		return
	}
	unlock, ok := s.guardTorrentMutation(w, t.Hash)
	if !ok {
		return
	}
	defer unlock()
	if err := s.qb.postOK("/api/v2/torrents/filePrio", url.Values{
		"hash": {t.Hash}, "id": {strings.ReplaceAll(ids, ",", "|")}, "priority": {strconv.Itoa(prio)},
	}); err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent filePrio: %v", err)
		return
	}
	log.Printf("prio %s: ids=%s -> %d", t.Hash, ids, prio)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": t.Hash, "ids": ids, "priority": prio})
}

func (s *server) handleAdd(w http.ResponseWriter, r *http.Request) {
	if s.qb == nil {
		writeErr(w, http.StatusNotImplemented, "qbittorrent is not configured")
		return
	}
	q := r.URL.Query()
	fields := map[string]string{}
	if v := q.Get("savepath"); v != "" {
		clean := filepath.Clean(v)
		if !filepath.IsAbs(clean) || (clean != s.root && !strings.HasPrefix(clean, s.root+"/")) {
			writeErr(w, http.StatusBadRequest, "savepath must be inside %s", s.root)
			return
		}
		fields["savepath"] = clean
	}
	if v := q.Get("category"); v != "" {
		if strings.ContainsAny(v, "/\\\x00") {
			writeErr(w, http.StatusBadRequest, "category must not contain path separators")
			return
		}
		fields["category"] = v
	}
	if q.Get("stopped") == "true" {
		fields["stopped"] = "true" // qBit 5.x
		fields["paused"] = "true"
	}
	var err error
	if name := q.Get("file"); name != "" {
		if strings.ContainsAny(name, "/\\") {
			writeErr(w, http.StatusBadRequest, "file must be a bare name inside /etc/torrmedia")
			return
		}
		var data []byte
		if data, err = os.ReadFile(filepath.Join("/etc/torrmedia", name)); err != nil {
			writeErr(w, http.StatusBadRequest, "read %s: %v", name, err)
			return
		}
		err = s.qb.addTorrentFile(name, data, fields)
	} else if m := q.Get("magnet"); m != "" {
		fields["urls"] = m
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		err = s.qb.postOK("/api/v2/torrents/add", form)
	} else {
		writeErr(w, http.StatusBadRequest, "need ?file= or ?magnet=")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent add: %v", err)
		return
	}
	log.Printf("add: ok (category=%q savepath=%q source=%s)",
		fields["category"], fields["savepath"], addSource(q))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": true, "warnings": []string{}})
}

func addSource(q url.Values) string {
	if q.Get("file") != "" {
		return "torrent-file"
	}
	return "magnet"
}

type qbit struct {
	base       *url.URL
	user, pass string
	hc         *http.Client
}

type qbTorrent struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	AmountLeft   int64   `json:"amount_left"`
	NumSeeds     int     `json:"num_seeds"`
	NumComplete  int     `json:"num_complete"`
	Availability float64 `json:"availability"`
	DownloadPath string  `json:"download_path"`
}

type qbFile struct {
	Index    *int    `json:"index"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Priority int     `json:"priority"`
	Progress float64 `json:"progress"`
}

type qbProperties struct {
	PieceSize int64 `json:"piece_size"`
}

type qbMatch struct {
	hash     string
	fileID   int
	abs      string
	priority int
}

func newQbit(rawURL, user, pass string) (*qbit, error) {
	u, err := url.Parse(strings.TrimSuffix(rawURL, "/"))
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &qbit{base: u, user: user, pass: pass, hc: &http.Client{Timeout: 15 * time.Second, Jar: jar}}, nil
}

func (q *qbit) do(method, path string, form url.Values) (*http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, q.base.String()+path, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Referer", q.base.String())
	req.Header.Set("Origin", q.base.String())
	return q.hc.Do(req)
}

func (q *qbit) login() error {
	resp, err := q.do("POST", "/api/v2/auth/login", url.Values{"username": {q.user}, "password": {q.pass}})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("login rejected: HTTP %d — check QBIT_USER/QBIT_PASS", resp.StatusCode)
	}
	return nil
}

func (q *qbit) doAuth(method, path string, form url.Values) (*http.Response, error) {
	resp, err := q.do(method, path, form)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err := q.login(); err != nil {
			return nil, err
		}
		return q.do(method, path, form)
	}
	return resp, nil
}

func (q *qbit) getJSON(path string, v any) error {
	resp, err := q.doAuth("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (q *qbit) postOK(path string, form url.Values) error {
	resp, err := q.doAuth("POST", path, form)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: HTTP %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if text := strings.TrimSpace(string(body)); strings.HasPrefix(text, "Fails") {
		return fmt.Errorf("POST %s: qbittorrent rejected the request (%s)", path, text)
	}
	return nil
}

func (q *qbit) getText(path string, out *string) error {
	resp, err := q.doAuth("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	*out = strings.TrimSpace(string(body))
	return nil
}

func (q *qbit) findFile(target os.FileInfo) (*qbMatch, error) {
	var torrents []qbTorrent
	if err := q.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		return nil, err
	}
	var match *qbMatch
	for _, t := range torrents {
		var files []qbFile
		if err := q.getJSON("/api/v2/torrents/files?hash="+url.QueryEscape(t.Hash), &files); err != nil {
			return nil, err
		}
		for i, f := range files {
			abs := filepath.Join(t.SavePath, f.Name)
			fi, err := os.Stat(abs)
			if err != nil {
				continue
			}
			if os.SameFile(target, fi) {
				id := i
				if f.Index != nil {
					id = *f.Index
				}
				if match != nil && (match.hash != t.Hash || match.fileID != id) {
					return nil, errors.New("file inode belongs to multiple torrent entries; refusing ambiguous delete")
				}
				match = &qbMatch{hash: t.Hash, fileID: id, abs: abs, priority: f.Priority}
			}
		}
	}
	return match, nil
}

func (q *qbit) findTorrentByPath(abs string) (*qbTorrent, error) {
	var torrents []qbTorrent
	if err := q.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		return nil, err
	}
	for _, t := range torrents {
		for _, base := range []string{t.ContentPath, filepath.Join(t.SavePath, t.Name)} {
			if base == "" {
				continue
			}
			base = strings.TrimSuffix(base, "/")
			if abs == base || strings.HasPrefix(abs, base+"/") {
				return &t, nil
			}
		}
	}
	return nil, nil
}

func (q *qbit) files(hash string) ([]qbFile, error) {
	var files []qbFile
	err := q.getJSON("/api/v2/torrents/files?hash="+url.QueryEscape(hash), &files)
	return files, err
}

func (q *qbit) properties(hash string) (*qbProperties, error) {
	var p qbProperties
	err := q.getJSON("/api/v2/torrents/properties?hash="+url.QueryEscape(hash), &p)
	return &p, err
}

func (q *qbit) torrent(hash string) (*qbTorrent, error) {
	var torrents []qbTorrent
	err := q.getJSON("/api/v2/torrents/info?hashes="+url.QueryEscape(hash), &torrents)
	if err != nil {
		return nil, err
	}
	if len(torrents) == 0 {
		return nil, fmt.Errorf("%w: %q", errTorrentNotFound, hash)
	}
	if len(torrents) != 1 {
		return nil, fmt.Errorf("expected one torrent %q, got %d", hash, len(torrents))
	}
	return &torrents[0], nil
}

func qbitStoppedState(state string) bool {
	return strings.HasPrefix(strings.ToLower(state), "stopped") ||
		strings.HasPrefix(strings.ToLower(state), "paused")
}

func qbitCheckingState(state string) bool {
	return strings.HasPrefix(strings.ToLower(state), "checking")
}

func qbFileByID(files []qbFile, id int) (*qbFile, bool) {
	for i := range files {
		actualID := i
		if files[i].Index != nil {
			actualID = *files[i].Index
		}
		if actualID == id {
			return &files[i], true
		}
	}
	return nil, false
}

func (q *qbit) switchTorrent(hash, action string) error {
	legacy := map[string]string{"start": "resume", "stop": "pause"}[action]
	err := q.postOK("/api/v2/torrents/"+action, url.Values{"hashes": {hash}})
	if err != nil && legacy != "" {
		err = q.postOK("/api/v2/torrents/"+legacy, url.Values{"hashes": {hash}})
	}
	return err
}

func pollUntil(timeout, interval time.Duration, check func() (bool, error)) error {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("operation timed out")
		}
		time.Sleep(interval)
	}
}

func (q *qbit) waitStopped(hash string, timeout, interval time.Duration) error {
	return pollUntil(timeout, interval, func() (bool, error) {
		t, err := q.torrent(hash)
		if err != nil {
			return false, err
		}
		return qbitStoppedState(t.State), nil
	})
}

func (q *qbit) waitStarted(hash string, timeout, interval time.Duration) (*qbTorrent, error) {
	var started *qbTorrent
	err := pollUntil(timeout, interval, func() (bool, error) {
		t, err := q.torrent(hash)
		if err != nil {
			return false, err
		}
		if !qbitStoppedState(t.State) {
			started = t
			return true, nil
		}
		return false, nil
	})
	return started, err
}

func (q *qbit) waitFilePriority(hash string, id, priority int, timeout, interval time.Duration) error {
	return pollUntil(timeout, interval, func() (bool, error) {
		files, err := q.files(hash)
		if err != nil {
			return false, err
		}
		f, ok := qbFileByID(files, id)
		if !ok {
			return false, fmt.Errorf("file id %d disappeared", id)
		}
		return f.Priority == priority, nil
	})
}

func (q *qbit) waitRecheck(hash string, id int, timeout, interval time.Duration) error {
	settled := 0
	const requiredSettled = 2
	return pollUntil(timeout, interval, func() (bool, error) {
		t, err := q.torrent(hash)
		if err != nil {
			return false, err
		}
		if qbitCheckingState(t.State) {
			settled = 0
			return false, nil
		}
		if !qbitStoppedState(t.State) {
			return false, fmt.Errorf("torrent unexpectedly left stopped state after recheck: %s", t.State)
		}
		files, err := q.files(hash)
		if err != nil {
			return false, err
		}
		if _, ok := qbFileByID(files, id); !ok {
			return false, fmt.Errorf("file id %d disappeared after recheck", id)
		}
		settled++
		return settled >= requiredSettled, nil
	})
}

func (q *qbit) addTorrentFile(name string, data []byte, fields map[string]string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	part, err := mw.CreateFormFile("torrents", name)
	if err != nil {
		return err
	}
	if _, err = part.Write(data); err != nil {
		return err
	}
	if err = mw.Close(); err != nil {
		return err
	}
	post := func() (*http.Response, error) {
		req, err := http.NewRequest("POST", q.base.String()+"/api/v2/torrents/add", bytes.NewReader(buf.Bytes()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Referer", q.base.String())
		req.Header.Set("Origin", q.base.String())
		return q.hc.Do(req)
	}
	resp, err := post()
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err := q.login(); err != nil {
			return err
		}
		if resp, err = post(); err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("add: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (q *qbit) recheck(hash string) error {
	return q.postOK("/api/v2/torrents/recheck", url.Values{"hashes": {hash}})
}

func (q *qbit) setFilePrio(hash string, id, prio int) error {
	return q.postOK("/api/v2/torrents/filePrio", url.Values{
		"hash": {hash}, "id": {strconv.Itoa(id)}, "priority": {strconv.Itoa(prio)},
	})
}

func (q *qbit) deleteTorrent(hash string, deleteFiles bool) error {
	return q.postOK("/api/v2/torrents/delete", url.Values{
		"hashes": {hash}, "deleteFiles": {strconv.FormatBool(deleteFiles)},
	})
}

var secretQueryKeys = map[string]bool{"magnet": true, "urls": true, "file": true, "savepath": true}

func redactQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if secretQueryKeys[k] {
			parts = append(parts, k+"=[redacted]")
			continue
		}
		parts = append(parts, k+"="+q.Get(k))
	}
	return "?" + strings.Join(parts, "&")
}

func (s *server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.URL.Path != "/health" {
			expected := "Bearer " + s.token
			got := r.Header.Get("Authorization")
			if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
				writeErr(w, http.StatusUnauthorized, "missing or invalid token")
				return
			}
		}
		if r.Method != http.MethodGet {
			if s.draining.Load() && r.URL.Path != "/viewed" {
				writeErr(w, http.StatusServiceUnavailable, "torrmedia is shutting down")
				return
			}
			log.Printf("%s %s %s%s", r.RemoteAddr, r.Method, r.URL.Path, redactQuery(r.URL.Query()))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	status := healthStatus{OK: true, Version: version, Checks: map[string]string{}}
	switch root := s.fsRoot(); {
	case root == nil:
		status.OK = false
		status.Checks["root"] = "media root is not available"
	default:
		if _, err := root.Stat("."); err != nil {
			status.OK = false
			status.Checks["root"] = err.Error()
		} else {
			status.Checks["root"] = "ok"
		}
	}
	if s.viewed == nil {
		status.OK = false
		status.Checks["viewed"] = "not configured"
	} else {
		status.Checks["viewed"] = "ok"
	}
	if s.qb != nil {
		if err := s.probe.check(s.qb); err != nil {
			status.OK = false
			status.Checks["qbittorrent"] = err.Error()
		} else {
			status.Checks["qbittorrent"] = "ok"
		}
	}
	code := http.StatusOK
	if !status.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func onlyMethod(method string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeErr(w, http.StatusMethodNotAllowed, "method %s is not allowed", r.Method)
			return
		}
		handler(w, r)
	}
}

// routes deliberately uses path-only ServeMux patterns. Method-qualified
// patterns ("GET /health") depend on the Go module compatibility mode and can
// silently become literal paths in legacy builds.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", onlyMethod(http.MethodGet, s.handleHealth))
	mux.HandleFunc("/list", onlyMethod(http.MethodGet, s.handleList))
	handleFile := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.handleFile(w, r)
		case http.MethodDelete:
			s.handleDelete(w, r)
		default:
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead+", "+http.MethodDelete)
			writeErr(w, http.StatusMethodNotAllowed, "method %s is not allowed", r.Method)
		}
	}
	mux.HandleFunc("/file", handleFile)
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
			writeErr(w, http.StatusMethodNotAllowed, "method %s is not allowed", r.Method)
			return
		}
		s.handleFile(w, r)
	})
	mux.HandleFunc("/file/enable", onlyMethod(http.MethodPost, s.handleEnable))
	mux.HandleFunc("/viewed", s.handleViewed)
	// Service endpoints, outside docs/09:
	mux.HandleFunc("/torrent", onlyMethod(http.MethodGet, s.handleTorrent))
	mux.HandleFunc("/torrents", onlyMethod(http.MethodGet, s.handleTorrents))
	mux.HandleFunc("/recheck", onlyMethod(http.MethodPost, s.handleRecheck))
	mux.HandleFunc("/start", onlyMethod(http.MethodPost, s.handleStartStop("start")))
	mux.HandleFunc("/stop", onlyMethod(http.MethodPost, s.handleStartStop("stop")))
	mux.HandleFunc("/add", onlyMethod(http.MethodPost, s.handleAdd))
	mux.HandleFunc("/prio", onlyMethod(http.MethodPost, s.handlePrio))
	mux.HandleFunc("/remove", onlyMethod(http.MethodPost, s.handleRemove))
	return s.wrap(mux)
}

func main() {
	root, err := filepath.EvalSymlinks(envOr("TORRMEDIA_ROOT", "/var/media/library"))
	if err != nil {
		log.Fatalf("TORRMEDIA_ROOT: %v", err)
	}
	root = strings.TrimSuffix(root, "/")
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		log.Fatalf("TORRMEDIA_ROOT: %v", err)
	}
	defer osRoot.Close()
	s := &server{
		root:             root,
		osRoot:           osRoot,
		token:            os.Getenv("TORRMEDIA_TOKEN"),
		deleteDebounce:   30 * time.Second,
		operationTimeout: 5 * time.Minute,
		pollInterval:     250 * time.Millisecond,
		enableReserve:    512 << 20,
		availableBytes:   availableFilesystemBytes,
		playback:         defaultPlaybackConfig(),
		locks:            newLockTable(),
		streams:          newStreamRegistry(),
	}
	if err := parsePlaybackConfig(&s.playback); err != nil {
		log.Fatal(err)
	}
	s.viewed, err = newViewedStore(envOr("TORRMEDIA_VIEWED_FILE", "/var/lib/torrmedia/viewed.json"))
	if err != nil {
		log.Fatalf("TORRMEDIA_VIEWED_FILE: %v", err)
	}
	if s.token == "" {
		log.Printf("WARNING: TORRMEDIA_TOKEN is not set — the API is open to anyone who can reach %s. "+
			"Set TORRMEDIA_TOKEN or keep the port behind a firewall/VPN (see README).",
			envOr("TORRMEDIA_LISTEN", ":8091"))
	}
	if v := os.Getenv("TORRMEDIA_OPERATION_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("TORRMEDIA_OPERATION_TIMEOUT: %v", err)
		}
		if d <= 0 {
			log.Fatalf("TORRMEDIA_OPERATION_TIMEOUT must be positive")
		}
		s.operationTimeout = d
	}
	if v := os.Getenv("TORRMEDIA_DELETE_DEBOUNCE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("TORRMEDIA_DELETE_DEBOUNCE: %v", err)
		}
		if d <= 0 {
			log.Fatalf("TORRMEDIA_DELETE_DEBOUNCE must be positive")
		}
		s.deleteDebounce = d
	}
	if qu := os.Getenv("QBIT_URL"); qu != "" {
		if s.qb, err = newQbit(qu, os.Getenv("QBIT_USER"), os.Getenv("QBIT_PASS")); err != nil {
			log.Fatalf("QBIT_URL: %v", err)
		}
	}

	listen := envOr("TORRMEDIA_LISTEN", ":8091")
	srv := &http.Server{
		Addr:              listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("torrmedia %s: listen=%s root=%s qbit=%t", version, listen, s.root, s.qb != nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		stop()
		log.Printf("torrmedia: shutdown requested, draining")
		s.draining.Store(true)
		s.cancelAllQueuedDeletes()
		done := make(chan struct{})
		go func() { s.inflight.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(s.operationTimeout + 30*time.Second):
			log.Printf("torrmedia: in-flight operations did not finish in time")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("torrmedia: shutdown: %v", err)
		}
		log.Printf("torrmedia: stopped cleanly")
	}
}
