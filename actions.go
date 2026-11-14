package main

// actions.go holds the operation bodies: what actually happens after the 202.
//
// Each function gets the operation so it can mark phases, and returns a
// result, warnings and an error. A warning means "it worked, but": for
// example, the data was freed but the torrent could not be restarted.
// Swallowing that is wrong, and turning it into an error would be a lie.

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// startOperation replies to the client immediately and runs the work in the
// background. doomed holds the ids of the files the operation deletes: play is
// not offered for them until it finishes. The operation does not block
// playback of the item's other files.
func (s *server) startOperation(w http.ResponseWriter, kind, itemID string, fileID *string,
	doomed []string, body func(*operation) (map[string]any, []string, error)) {

	op := s.ops().start(kind, itemID, fileID, doomed...)
	// Take the snapshot before starting the goroutine, which then owns the
	// operation itself.
	snapshot, _ := s.ops().get(op.ID)

	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		result, warnings, err := body(op)
		for i := range warnings {
			warnings[i] = s.withoutRoot(warnings[i])
		}
		if err != nil {
			s.ops().fail(op.ID, errors.New(s.withoutRoot(err.Error())), warnings)
			return
		}
		s.ops().succeed(op.ID, warnings, result)
	}()

	w.Header().Set("Location", "/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, snapshot)
}

// withoutRoot drops the media root from a message that reaches the client:
// operating system errors carry absolute paths, and disk paths are not part
// of the API.
func (s *server) withoutRoot(text string) string {
	if s.root == "" {
		return text
	}
	text = strings.ReplaceAll(text, s.root+string(filepath.Separator), "")
	return strings.ReplaceAll(text, s.root, "")
}

// guardTorrent takes the per-torrent lock and refuses if a file delete is
// already scheduled for the item. Without this a maintenance command could
// wedge in between freeing the data and the recheck, and a /remove in that
// window made the loss unrecoverable: the files are zeroed and there is no
// torrent left to re-download them.
func (s *server) guardTorrent(hash string) (func(), error) {
	if s.draining.Load() {
		return nil, errors.New("torrmedia is shutting down")
	}
	unlock := s.lockTorrent(hash)
	if s.pendingBatch(hash) {
		unlock()
		return nil, errors.New("a delete batch is queued for this item")
	}
	return unlock, nil
}

func (s *server) runItemCommand(op *operation, hash, action string) (map[string]any, []string, error) {
	unlock, err := s.guardTorrent(hash)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()

	warnings := []string{}
	deadline := time.Now().Add(s.operationTimeout)
	budget := func() time.Duration { return time.Until(deadline) }

	switch action {
	case actionStart:
		s.ops().phase(op.ID, "starting")
		if err := s.qb.switchTorrent(hash, "start"); err != nil {
			return nil, warnings, err
		}
		if _, err := s.qb.waitStarted(hash, budget(), s.pollInterval); err != nil {
			return nil, warnings, err
		}
	case actionStop:
		s.ops().phase(op.ID, "stopping")
		// The force start flag survives a stop, and the next start would again
		// ignore the queue and the seeding limits.
		if err := s.qb.setForceStart(hash, false); err != nil {
			warnings = append(warnings, fmt.Sprintf("force start was not cleared: %v", err))
		}
		if err := s.qb.switchTorrent(hash, "stop"); err != nil {
			return nil, warnings, err
		}
		if err := s.qb.waitStopped(hash, budget(), s.pollInterval); err != nil {
			return nil, warnings, err
		}
	case actionRecheck:
		s.ops().phase(op.ID, "rechecking")
		if err := s.qb.recheck(hash); err != nil {
			return nil, warnings, err
		}
		// The timeout counts from the last progress of the check, not from the
		// overall budget: a large torrent takes longer than that to check.
		if err := s.qb.waitCheckDone(hash, s.operationTimeout, s.pollInterval); err != nil {
			return nil, warnings, err
		}
	default:
		return nil, warnings, fmt.Errorf("unknown item action %q", action)
	}
	s.poke()
	return map[string]any{"action": action}, warnings, nil
}

func (s *server) runItemRemove(op *operation, item *catalogItem, deleteFiles bool) (map[string]any, []string, error) {
	warnings := []string{}
	if item.torrentBacked() {
		unlock, err := s.guardTorrent(item.hash)
		if err != nil {
			return nil, warnings, err
		}
		defer unlock()
		s.ops().phase(op.ID, "removing")
		if err := s.qb.deleteTorrent(item.hash, deleteFiles); err != nil {
			return nil, warnings, err
		}
		s.poke()
		return map[string]any{"files_deleted": deleteFiles}, warnings, nil
	}

	// An item with no torrent: nothing to delete but the files themselves.
	if !deleteFiles {
		return nil, warnings, errors.New("this item has no torrent behind it")
	}
	s.ops().phase(op.ID, "deleting")
	removed := 0
	for i := range item.files {
		file := &item.files[i]
		if file.shelfRel == "" {
			continue
		}
		if err := s.removeShelfFile(file); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", file.name, err))
			continue
		}
		removed++
	}
	s.poke()
	// Not a single file went away: that is a failure, not a success with
	// warnings, or the client would report the item as removed while it is
	// still there.
	if removed == 0 && len(warnings) > 0 {
		return nil, warnings, fmt.Errorf("no file was deleted: %s", warnings[0])
	}
	return map[string]any{"files_deleted": removed}, warnings, nil
}

func (s *server) runFileDelete(op *operation, item *catalogItem, file *catalogFile) (map[string]any, []string, error) {
	if !item.torrentBacked() {
		s.ops().phase(op.ID, "deleting")
		if err := s.removeShelfFile(file); err != nil {
			return nil, nil, err
		}
		s.poke()
		return map[string]any{"deleted": file.name}, []string{}, nil
	}

	target, err := s.deleteTargetFor(item, file)
	if err != nil {
		return nil, nil, err
	}
	s.ops().phase(op.ID, "waiting")
	outcome := s.awaitDelete(op.ID, target)
	if outcome.err != "" {
		return nil, nil, errors.New(outcome.err)
	}
	warnings, _ := outcome.response["warnings"].([]string)
	if warnings == nil {
		warnings = []string{}
	}
	s.poke()
	return outcome.response, warnings, nil
}

// deleteTargetFor describes a file for a batched delete. A file missing from
// disk is a valid target: its priority still has to be dropped, otherwise
// qBittorrent would download it right away.
func (s *server) deleteTargetFor(item *catalogItem, file *catalogFile) (*deleteTarget, error) {
	abs, ok := s.fileAbs(file)
	if !ok {
		return nil, errors.New("file lies outside the media root")
	}
	info, err := os.Stat(abs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		info = nil
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return nil, errors.New("file lies outside the media root")
	}
	return &deleteTarget{
		rel: rel, info: info,
		match: qbMatch{hash: item.hash, fileID: file.qbitIndex, abs: abs},
	}, nil
}

// runDeleteWatched deletes watched episodes. They all go into one delete
// batch: however many episodes there are, the torrent is stopped, rechecked
// and started exactly once. Partly watched ones are left alone; they are not
// in the list.
//
// The batch itself rejects a file that is playing right now, and that is no
// reason to fail the rest: it ends up as a warning, not an error.
func (s *server) runDeleteWatched(op *operation, item *catalogItem, files []catalogFile) (map[string]any, []string, error) {
	warnings := []string{}
	if len(files) == 0 {
		return nil, warnings, errors.New("no watched files to delete")
	}
	// The batch has one response for all of its files, so its warnings arrive
	// as many times as there are targets of ours in it.
	seen := map[string]bool{}
	warn := func(format string, args ...any) {
		text := fmt.Sprintf(format, args...)
		if !seen[text] {
			seen[text] = true
			warnings = append(warnings, text)
		}
	}

	if !item.torrentBacked() {
		s.ops().phase(op.ID, "deleting")
		deleted := 0
		for i := range files {
			if err := s.removeShelfFile(&files[i]); err != nil {
				warn("%s: %v", files[i].name, err)
				continue
			}
			deleted++
		}
		if deleted == 0 {
			return nil, warnings, errors.New("nothing was deleted")
		}
		s.poke()
		return map[string]any{"deleted": deleted, "requested": len(files)}, warnings, nil
	}

	targets := make([]*deleteTarget, 0, len(files))
	for i := range files {
		target, err := s.deleteTargetFor(item, &files[i])
		if err != nil {
			warn("%s: %v", files[i].name, err)
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, warnings, errors.New("no watched file could be addressed")
	}

	s.ops().phase(op.ID, "waiting")
	deleted := 0
	for i, outcome := range s.awaitDeletes(op.ID, targets) {
		if outcome.err != "" {
			warn("%s: %s", filepath.Base(targets[i].rel), outcome.err)
			continue
		}
		deleted++
		texts, _ := outcome.response["warnings"].([]string)
		for _, text := range texts {
			warn("%s", text)
		}
	}
	if deleted == 0 {
		return nil, warnings, errors.New("no watched file was deleted")
	}
	s.poke()
	return map[string]any{"deleted": deleted, "requested": len(files)}, warnings, nil
}

// reserveDeleteFile claims the existing inode before the operation is created.
// So a stream that got through permitFile before the DELETE can no longer
// start reading in the window between the 202 and the actual trim.
func (s *server) reserveDeleteFile(file *catalogFile) (release func(), free bool, err error) {
	abs, ok := s.fileAbs(file)
	if !ok {
		return func() {}, true, nil
	}
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	release, free = s.streamReg().reserve(inodeKey(info))
	return release, free, nil
}

// removeShelfFile deletes a catalog file that has no torrent behind it.
func (s *server) removeShelfFile(file *catalogFile) error {
	abs, ok := s.fileAbs(file)
	if !ok {
		return errors.New("file lies outside the media root")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	unreserve, free := s.streamReg().reserve(inodeKey(info))
	if !free {
		return errors.New("file is being streamed right now")
	}
	defer unreserve()
	if err := os.Remove(abs); err != nil {
		return err
	}
	s.pruneEmptyDirs(filepath.Dir(abs))
	return nil
}

// runFileEnable returns an excluded file to downloading.
// lockAfterDeletes waits for the item's scheduled and running deletes, then
// takes its lock. Wedging in between a batch's stop, trim and recheck is not
// allowed (see guardTorrent), but there is no reason to refuse either: an
// enable queued while neighbouring episodes are being deleted simply runs
// after the batch, with the space the batch freed and with the torrent start
// that the batch itself does not do if the torrent was stopped.
func (s *server) lockAfterDeletes(op *operation, hash string) (func(), error) {
	debounce := s.deleteDebounce
	if debounce <= 0 {
		debounce = defaultDeleteDebounce
	}
	// A new delete extends the batch's debounce window, so the deadline leaves
	// room for several windows plus the batch itself.
	deadline := time.Now().Add(3*debounce + s.operationTimeout)
	wait := s.pollInterval
	if wait <= 0 {
		wait = 250 * time.Millisecond
	}
	if s.pendingBatch(hash) || s.operations.busyBeyond(op.Item, opFileEnable) {
		s.ops().phase(op.ID, "waiting")
	}
	for {
		if s.draining.Load() {
			return nil, errors.New("torrmedia is shutting down; file was not enabled")
		}
		if !s.pendingBatch(hash) {
			// A running batch holds this lock; wait here until it finishes.
			unlock := s.lockTorrent(hash)
			if !s.pendingBatch(hash) && !s.draining.Load() {
				return unlock, nil
			}
			// A new delete was queued while we waited for the lock (or the
			// service is shutting down); wait for it again.
			unlock()
		}
		if time.Now().After(deadline) {
			return nil, errors.New("deletion on this item did not finish in time; file was not enabled")
		}
		time.Sleep(wait)
	}
}

// startTimeout is how long to wait for qBittorrent to confirm a torrent start.
func (s *server) startTimeout() time.Duration {
	return min(30*time.Second, s.operationTimeout)
}

func (s *server) runFileEnable(op *operation, hash string, index int) (map[string]any, []string, error) {
	unlock, err := s.lockAfterDeletes(op, hash)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	warnings := []string{}

	t, err := s.qb.torrent(hash)
	if err != nil {
		return nil, warnings, err
	}
	if qbitCheckingState(t.State) {
		return nil, warnings, errors.New("torrent is being checked; enable was not applied")
	}
	files, err := s.qb.files(hash)
	if err != nil {
		return nil, warnings, err
	}
	file, ok := qbFileByID(files, index)
	if !ok {
		return nil, warnings, errFileGone
	}
	abs, err := s.torrentFilePath(t, file)
	if err != nil {
		return nil, warnings, err
	}
	required, err := requiredFileBytes(abs, file.Size)
	if err != nil {
		return nil, warnings, err
	}
	// The file downloads into download_path if that is set, and it may be on
	// another partition.
	spaceProbe := t.DownloadPath
	if spaceProbe == "" {
		spaceProbe = t.SavePath
	}
	available, err := s.availableBytes(spaceProbe)
	if err != nil {
		return nil, warnings, err
	}
	reserve := s.enableReserve
	if required == 0 {
		reserve = 0
	}
	if available < required+reserve {
		return nil, warnings, fmt.Errorf(
			"selected file needs %d bytes plus %d reserve; only %d available",
			required, reserve, available)
	}

	rollback := func() {
		if file.Priority != 0 {
			return
		}
		if err := s.qb.setFilePrio(hash, index, 0); err != nil {
			warnings = append(warnings, fmt.Sprintf("ROLLBACK INCOMPLETE: %v", err))
		}
	}
	if file.Priority == 0 {
		s.ops().phase(op.ID, "enabling")
		if err := s.qb.setFilePrio(hash, index, 1); err != nil {
			return nil, warnings, err
		}
		if err := s.qb.waitFilePriority(hash, index, 1, s.operationTimeout, s.pollInterval); err != nil {
			rollback()
			return nil, warnings, err
		}
	}
	s.ops().phase(op.ID, "starting")
	if err := s.qb.switchTorrent(hash, "start"); err != nil {
		rollback()
		return nil, warnings, err
	}
	after, err := s.qb.waitStarted(hash, s.startTimeout(), s.pollInterval)
	if err != nil {
		if stopErr := s.qb.switchTorrent(hash, "stop"); stopErr != nil {
			warnings = append(warnings, fmt.Sprintf("stop also failed: %v", stopErr))
		}
		rollback()
		return nil, warnings, err
	}
	s.poke()
	return map[string]any{
		"required_bytes": required, "available_bytes": available,
		"amount_left": after.AmountLeft,
	}, warnings, nil
}

// runFilePriority changes the priority of a file that is already downloading
// or queued. Excluding and re-including a file are not handled here: delete
// and enable exist for that.
func (s *server) runFilePriority(op *operation, hash string, index, priority int) (map[string]any, []string, error) {
	unlock, err := s.guardTorrent(hash)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	warnings := []string{}

	t, err := s.qb.torrent(hash)
	if err != nil {
		return nil, warnings, err
	}
	if qbitCheckingState(t.State) {
		return nil, warnings, errors.New("torrent is being checked; priority was not applied")
	}
	files, err := s.qb.files(hash)
	if err != nil {
		return nil, warnings, err
	}
	file, ok := qbFileByID(files, index)
	if !ok {
		return nil, warnings, errFileGone
	}
	// The file was excluded between rendering and the click: setting a priority
	// would put it back into downloading, bypassing the space check enable does.
	if file.Priority == 0 {
		return nil, warnings, errors.New("file is excluded; enable it first")
	}
	if file.Priority != priority {
		s.ops().phase(op.ID, "prioritizing")
		if err := s.qb.setFilePrio(hash, index, priority); err != nil {
			return nil, warnings, err
		}
		// filePrio returns 200 even when it applied nothing, so trust only
		// the value read back.
		if err := s.qb.waitFilePriority(hash, index, priority, s.operationTimeout, s.pollInterval); err != nil {
			return nil, warnings, err
		}
	}
	s.poke()
	return map[string]any{"priority": priorityName(priority)}, warnings, nil
}

func (s *server) runAddMagnet(op *operation, magnet, category string) (map[string]any, []string, error) {
	s.ops().phase(op.ID, "adding")
	form := map[string]string{"urls": magnet}
	if category != "" {
		form["category"] = category
	}
	if err := s.qb.addForm(form); err != nil {
		return nil, nil, err
	}
	// Neither the magnet nor its trackers go to the log: private trackers put a
	// passkey in the URL, and the log is kept longer than it needs to be.
	log.Printf("add: ok (category=%q source=magnet)", category)
	s.poke()
	return map[string]any{"added": true}, []string{}, nil
}

func checkAddLink(link string) error {
	// qBittorrent's "urls" field is a list separated by line breaks: a magnet
	// with a newline inside would smuggle a second, arbitrary http(s) URL past
	// the prefix check below. A real magnet never contains control characters.
	if strings.ContainsFunc(link, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("magnet link must not contain line breaks or control characters")
	}
	lower := strings.ToLower(strings.TrimSpace(link))
	if strings.HasPrefix(lower, "magnet:?") {
		return nil
	}
	return errors.New("only magnet: links are accepted")
}

func checkCategory(category string) error {
	if strings.ContainsAny(category, "/\\\x00") {
		return errors.New("category must not contain path separators")
	}
	return nil
}

// streamedFileOf returns the name of the item's first file that the player is
// reading right now. Data under a playing file must not be deleted.
func (s *server) streamedFileOf(item *catalogItem) (string, error) {
	for i := range item.files {
		file := &item.files[i]
		abs, ok := s.fileAbs(file)
		if !ok {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if s.streamReg().active(inodeKey(info)) {
			return file.name, nil
		}
	}
	return "", nil
}
