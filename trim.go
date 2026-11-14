package main

// trim.go frees the space taken by a single file of a torrent.
//
// qBittorrent has no API to "delete one file of a torrent" (issue #9565). The
// workaround: drop the file's priority, zero out its contents while keeping
// one boundary piece at each end (those are shared with neighbouring files),
// and recheck the torrent once. The client knows nothing about this mechanism.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type trimResult struct {
	logicalSize int64
	retained    int64
	freed       int64
}

type deleteTarget struct {
	rel     string
	info    os.FileInfo
	match   qbMatch
	waiters []chan *deleteBatchOutcome
	// ops are the operations waiting on this file; the batch reports its phase
	// to them. Otherwise the client's header would show the whole delete as
	// "waiting", and a recheck of a large torrent takes minutes, which is easy
	// to mistake for a hang.
	ops []string
}

type deleteBatch struct {
	generation uint64
	timer      *time.Timer
	targets    map[string]*deleteTarget
}

type deleteBatchOutcome struct {
	err      string
	response map[string]any

	// perTarget: an invalid file no longer fails the whole batch. Each waiter
	// gets the result of ITS OWN file, and the rest are carried through.
	perTarget map[string]*deleteBatchOutcome

	// ownWarnings are warnings about one file of the batch. They go only to
	// that file's operations: the others used to get, say, a neighbour's
	// "data kept on disk" as if it were about their own file.
	ownWarnings map[string][]string
}

func (o *deleteBatchOutcome) forTarget(rel string) *deleteBatchOutcome {
	if o == nil {
		return nil
	}
	if own, ok := o.perTarget[rel]; ok {
		return own
	}
	extra := o.ownWarnings[rel]
	if len(extra) == 0 || o.response == nil {
		return o
	}
	copied := *o
	copied.response = make(map[string]any, len(o.response))
	for k, v := range o.response {
		copied.response[k] = v
	}
	general, _ := o.response["warnings"].([]string)
	copied.response["warnings"] = append(append([]string{}, general...), extra...)
	return &copied
}

type preparedDelete struct {
	target    *deleteTarget
	file      *os.File
	info      os.FileInfo
	unreserve func() // releases the inode claim; nil if the file does not exist yet
	priority  int
	size      int64
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

// errTooSmallToTrim means the file fits entirely in two pieces, so all of its
// bytes belong to pieces shared with neighbouring files. Zeroing it would
// corrupt data the user did not ask to delete, with nothing to gain. Such a
// file is only excluded from the wanted set.
var errTooSmallToTrim = errors.New("file fits in two pieces; boundary data is shared with neighbours")

// maxTrimPieceSize bounds the two edge pieces a trim holds in memory.
const maxTrimPieceSize = 64 << 20

func checkPieceSize(pieceSize int64) error {
	if pieceSize <= 0 || pieceSize > maxTrimPieceSize {
		return fmt.Errorf("unsafe piece size %d", pieceSize)
	}
	return nil
}

// trimSparseFile frees the contents of the selected file while keeping its
// inode, logical size and edges. One full piece at each edge is enough not to
// lose the pieces that cross the boundaries with neighbouring files.
func trimSparseFile(f *os.File, expected os.FileInfo, pieceSize int64) (trimResult, error) {
	if err := checkPieceSize(pieceSize); err != nil {
		return trimResult{}, err
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

	// Truncate acts on the inode, so it safely frees the data for both the
	// library path and the torrent hardlink at once, without removing
	// neighbouring files/directories.
	if err := f.Truncate(0); err != nil {
		return trimResult{}, fmt.Errorf("truncate to zero: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		return trimResult{}, fmt.Errorf("restore logical size: %w", err)
	}
	if _, err := f.WriteAt(prefix, 0); err != nil {
		return trimResult{}, fmt.Errorf("restore prefix: %w", err)
	}
	if _, err := f.WriteAt(suffix, suffixOff); err != nil {
		return trimResult{}, fmt.Errorf("restore suffix: %w", err)
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

// awaitDeletes queues the files into a delete batch and waits for their
// outcomes, one per target. It is called from an operation body, so blocking
// here is fine: the client has already got 202. All targets are enqueued at
// once, before the first wait; otherwise the debounce window would collect
// not a batch but a queue of stops and rechecks.
func (s *server) awaitDeletes(opID string, targets []*deleteTarget) []*deleteBatchOutcome {
	results := make([]chan *deleteBatchOutcome, len(targets))
	for i, target := range targets {
		results[i] = make(chan *deleteBatchOutcome, 1)
		target.waiters = []chan *deleteBatchOutcome{results[i]}
		target.ops = []string{opID}
		s.enqueueTorrentDelete(target)
	}
	out := make([]*deleteBatchOutcome, len(targets))
	for i, result := range results {
		out[i] = (<-result).forTarget(targets[i].rel)
	}
	return out
}

func (s *server) awaitDelete(opID string, target *deleteTarget) *deleteBatchOutcome {
	return s.awaitDeletes(opID, []*deleteTarget{target})[0]
}

// pendingBatch reports whether the torrent has a delete that is scheduled but
// not started yet. Maintenance mutating endpoints are rejected meanwhile;
// otherwise they would wedge in between stop, priority, trim and recheck.
func (s *server) pendingBatch(hash string) bool {
	s.deleteMu.Lock()
	defer s.deleteMu.Unlock()
	return s.deleteBatches[hash] != nil
}

// cancelAllQueuedDeletes cancels all scheduled delete batches when the service
// shuts down. None of them has changed anything yet, so cancelling is safe,
// and the client gets an honest refusal instead of a hung request.
func (s *server) cancelAllQueuedDeletes() {
	s.deleteMu.Lock()
	batches := s.deleteBatches
	s.deleteBatches = nil
	s.deleteMu.Unlock()

	for _, batch := range batches {
		if batch.timer != nil {
			batch.timer.Stop()
		}
		outcome := deleteBatchError("torrmedia is shutting down; nothing was deleted, retry after restart")
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
		debounce = defaultDeleteDebounce
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
		existing.ops = append(existing.ops, target.ops...)
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

func deleteBatchError(format string, args ...any) *deleteBatchOutcome {
	return &deleteBatchOutcome{err: fmt.Sprintf(format, args...)}
}

func closePreparedDeletes(prepared []*preparedDelete) {
	for _, item := range prepared {
		if item.file != nil {
			_ = item.file.Close()
		}
		if item.unreserve != nil {
			item.unreserve()
		}
	}
}

// rollbackDeletePriorities restores the priorities and REPORTS failures:
// errors used to be discarded with `_ =` while the client was told
// "priority rolled back".
func (s *server) rollbackDeletePriorities(hash string, changed []*preparedDelete) []string {
	var failures []string
	for i := len(changed) - 1; i >= 0; i-- {
		if err := s.qb.setFilePrio(hash, changed[i].target.match.fileID, changed[i].priority); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", filepath.Base(changed[i].target.rel), err))
		}
	}
	if len(failures) > 0 {
		log.Printf("delete batch: rollback of priorities for %s failed: %s", hash, strings.Join(failures, "; "))
	}
	return failures
}

// rollbackNote turns the rollback result into an honest error-message suffix.
func rollbackNote(failures []string) string {
	if len(failures) == 0 {
		return "priorities rolled back"
	}
	return "ROLLBACK INCOMPLETE (" + strings.Join(failures, "; ") + ")"
}

func (s *server) deleteTorrentBatch(hash string, targets []*deleteTarget) *deleteBatchOutcome {
	if len(targets) == 0 {
		return deleteBatchError("empty delete batch")
	}

	unlock := s.lockTorrent(hash)
	defer unlock()

	// One absolute deadline for the whole operation. Each wait inside (stop,
	// priority per file, recheck) used to have its own timeout, so the total time
	// was not bounded at all and the client gave up first.
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
		return deleteBatchError("qbittorrent refresh failed, nothing deleted: %v", err)
	}
	if qbitCheckingState(torrent.State) {
		return deleteBatchError("torrent is checking, nothing deleted")
	}
	props, err := s.qb.properties(hash)
	if err != nil {
		return deleteBatchError("qbittorrent properties failed, nothing deleted: %v", err)
	}
	// The trim itself would refuse such a piece size, but only after the
	// priorities were dropped: the files would end up excluded with their data
	// still on disk, and an excluded file is never offered for delete again.
	if err := checkPieceSize(props.PieceSize); err != nil {
		return deleteBatchError("cannot free space in this torrent (%v), nothing deleted", err)
	}
	files, err := s.qb.files(hash)
	if err != nil {
		return deleteBatchError("qbittorrent files failed, nothing deleted: %v", err)
	}

	// Validate every qBit ID/path and pin every inode that currently exists
	// before the first qBittorrent mutation. A missing file is a valid target:
	// priority=0 must be applicable before qBit creates or downloads it.
	//
	// A rejected file no longer cancels the whole batch: it gets its own
	// refusal, and the rest are carried through. Otherwise a single foreign file
	// in the selection silently lost the user's valid request.
	prepared := make([]*preparedDelete, 0, len(targets))
	rejected := make(map[string]*deleteBatchOutcome)
	seenIDs := make(map[int]string, len(targets))
	reject := func(target *deleteTarget, format string, args ...any) {
		rejected[target.rel] = deleteBatchError(format, args...)
	}
	for _, target := range targets {
		if target.match.hash != hash {
			reject(target, "%s belongs to another torrent", filepath.Base(target.rel))
			continue
		}
		if previous, exists := seenIDs[target.match.fileID]; exists {
			reject(target, "%s and %s are the same file of the torrent",
				filepath.Base(previous), filepath.Base(target.rel))
			continue
		}
		seenIDs[target.match.fileID] = target.rel

		qbFile, ok := qbFileByID(files, target.match.fileID)
		if !ok {
			reject(target, "%s is no longer part of the torrent", filepath.Base(target.rel))
			continue
		}
		currentAbs, err := s.torrentFilePath(torrent, qbFile)
		if err != nil || filepath.Clean(currentAbs) != filepath.Clean(target.match.abs) {
			reject(target, "%s moved inside the torrent", filepath.Base(target.rel))
			continue
		}
		currentRel, err := filepath.Rel(s.root, currentAbs)
		if err != nil {
			reject(target, "torrent file %q is outside torrmedia root", target.rel)
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
			reject(target, "torrent path for %q is not a regular file", target.rel)
			continue
		}
		if target.info != nil && !os.SameFile(target.info, currentInfo) {
			reject(target, "torrent inode for %q changed", target.rel)
			continue
		}
		// Open THROUGH os.Root: a symlink pointing outside (including one planted
		// in an intermediate directory during the debounce window) cannot escape
		// the root.
		root := s.fsRoot()
		if root == nil {
			reject(target, "media root %q is not available", s.root)
			continue
		}
		f, err := root.OpenFile(filepath.ToSlash(currentRel), os.O_RDWR, 0)
		if err != nil {
			reject(target, "open %q: %v", target.rel, err)
			continue
		}
		pinnedInfo, err := f.Stat()
		if err != nil || !pinnedInfo.Mode().IsRegular() ||
			!os.SameFile(currentInfo, pinnedInfo) {
			_ = f.Close()
			reject(target, "file %q changed while delete was prepared", target.rel)
			continue
		}
		// A file the player is reading right now must not be zeroed. The claim
		// is held until the operation ends: seconds pass between this check and
		// the zeroing, and without it the player could start reading meanwhile.
		unreserve, free := s.streamReg().reserve(inodeKey(pinnedInfo))
		if !free {
			_ = f.Close()
			reject(target, "file %q is being streamed right now", target.rel)
			continue
		}
		prepared = append(prepared, &preparedDelete{
			target: target, file: f, info: pinnedInfo, unreserve: unreserve,
			priority: qbFile.Priority, size: qbFile.Size,
		})
	}
	defer closePreparedDeletes(prepared)

	if len(prepared) == 0 {
		out := deleteBatchError("no valid files in delete batch, nothing deleted")
		out.perTarget = rejected
		return out
	}

	warnings := []string{}
	ownWarnings := map[string][]string{}
	// Every operation whose files the batch actually handles sees the phase.
	phase := func(name string) {
		seen := make(map[string]bool)
		for _, item := range prepared {
			for _, id := range item.target.ops {
				if !seen[id] {
					seen[id] = true
					s.ops().phase(id, name)
				}
			}
		}
	}

	wasRunning := !qbitStoppedState(torrent.State)
	if wasRunning {
		phase("stopping")
		if err := s.qb.switchTorrent(hash, "stop"); err != nil {
			out := deleteBatchError("qbittorrent stop failed, nothing deleted: %v", err)
			out.perTarget = rejected
			return out
		}
		left, err := budget()
		if err == nil {
			err = s.qb.waitStopped(hash, left, s.pollInterval)
		}
		if err != nil {
			_ = s.qb.switchTorrent(hash, "start")
			out := deleteBatchError("qbittorrent did not stop, nothing deleted: %v", err)
			out.perTarget = rejected
			return out
		}
	}

	phase("trimming")
	changed := make([]*preparedDelete, 0, len(prepared))
	for _, item := range prepared {
		if item.priority == 0 {
			continue
		}
		changed = append(changed, item)
		abort := func(format string, args ...any) *deleteBatchOutcome {
			note := rollbackNote(s.rollbackDeletePriorities(hash, changed))
			if wasRunning {
				if err := s.qb.switchTorrent(hash, "start"); err != nil {
					note += fmt.Sprintf("; restart failed: %v", err)
				}
			}
			out := deleteBatchError(format+"; %s", append(args, note)...)
			out.perTarget = rejected
			return out
		}
		if err := s.qb.setFilePrio(hash, item.target.match.fileID, 0); err != nil {
			return abort("qbittorrent filePrio failed, nothing trimmed: %v", err)
		}
		left, err := budget()
		if err == nil {
			err = s.qb.waitFilePriority(hash, item.target.match.fileID, 0, left, s.pollInterval)
		}
		if err != nil {
			return abort("qbittorrent did not accept priority=0, nothing trimmed: %v", err)
		}
	}

	type deletedFile struct {
		path    string
		trimmed trimResult
		absent  bool
		kept    bool // priority dropped, but data kept (file is smaller than two pieces)
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
			// Nothing to free, and zeroing would corrupt pieces of neighbouring
			// files. The file is excluded from the torrent and its data stays on
			// disk; say so honestly.
			ownWarnings[item.target.rel] = append(ownWarnings[item.target.rel], fmt.Sprintf(
				"%s: excluded from the torrent, but data kept on disk (%v)", filepath.Base(item.target.rel), err))
			deleted = append(deleted, deletedFile{
				path:    item.target.rel,
				trimmed: trimResult{logicalSize: item.size},
				kept:    true,
			})
			continue
		}
		if err != nil {
			// Part of the inode may already be sparse, and the zeroed boundary
			// pieces belong to NEIGHBOURING files. Priorities must not be
			// restored (that would re-download what was deleted), but recheck
			// and restart are mandatory; otherwise the neighbours stay corrupt
			// and nothing re-downloads them.
			// Start only after the recheck: in the middle of one, qBittorrent
			// swallows the start and stops the torrent again once the check
			// finishes.
			note := "torrent left stopped"
			phase("rechecking")
			if rcErr := s.qb.recheck(hash); rcErr != nil {
				note = fmt.Sprintf("recheck also failed: %v", rcErr)
			} else if wasRunning {
				startErr := s.qb.waitCheckDone(hash, s.operationTimeout, s.pollInterval)
				if startErr == nil {
					phase("restarting")
					startErr = s.qb.switchTorrent(hash, "start")
				}
				if startErr == nil {
					_, startErr = s.qb.waitStarted(hash, s.startTimeout(), s.pollInterval)
				}
				if startErr != nil {
					note = fmt.Sprintf("restart failed: %v", startErr)
				} else {
					note = "torrent restarted to repair neighbouring pieces"
				}
			}
			out := deleteBatchError("batch trim failed at %q with selected priorities=0; %s: %v",
				item.target.rel, note, err)
			out.perTarget = rejected
			return out
		}
		deleted = append(deleted, deletedFile{path: item.target.rel, trimmed: trimmed})
	}
	phase("rechecking")
	if err := s.qb.recheck(hash); err != nil {
		out := deleteBatchError("files trimmed, but qbittorrent recheck failed; torrent left stopped: %v", err)
		out.perTarget = rejected
		return out
	}
	// Not from the shared budget: a large torrent takes longer than that to
	// check, and abandoning the wait would leave it stopped. The timeout counts
	// from the last progress of the check.
	if err := s.qb.waitRecheck(hash, prepared[0].target.match.fileID, s.operationTimeout, s.pollInterval); err != nil {
		out := deleteBatchError("files trimmed, but qbittorrent recheck did not finish; torrent left stopped: %v", err)
		out.perTarget = rejected
		return out
	}

	fileResponses := make([]map[string]any, 0, len(deleted))
	var totalFreed, totalRetained int64
	for _, item := range deleted {
		totalFreed += item.trimmed.freed
		totalRetained += item.trimmed.retained
		fileResponse := map[string]any{
			"name": filepath.Base(item.path), "logical_size": item.trimmed.logicalSize,
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
		"batch_size": len(deleted), "batch_deleted": fileResponses,
		"storage_trimmed": true, "freed_bytes_estimate": totalFreed,
		"retained_boundary_bytes": totalRetained, "recheck": "complete",
	}
	if wasRunning {
		// Verify the start: it used to be sent in the middle of the recheck,
		// qBittorrent silently swallowed it, and the torrent stayed stopped despite
		// a "successful" batch.
		phase("restarting")
		err := s.qb.switchTorrent(hash, "start")
		if err == nil {
			_, err = s.qb.waitStarted(hash, s.startTimeout(), s.pollInterval)
		}
		if err != nil {
			// This used to go into a restart_error field the client never read,
			// so the operation looked unconditionally successful.
			warnings = append(warnings, fmt.Sprintf("torrent was not restarted: %v", err))
			log.Printf("delete batch: restart %s failed after successful trim: %v", hash, err)
		} else {
			resp["torrent_restarted"] = true
		}
	}
	resp["warnings"] = warnings
	log.Printf("delete batch: torrent=%s files=%d freed=%d retained=%d warnings=%d",
		hash, len(deleted), totalFreed, totalRetained, len(warnings))
	return &deleteBatchOutcome{response: resp, perTarget: rejected, ownWarnings: ownWarnings}
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
