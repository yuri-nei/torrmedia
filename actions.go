package main

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

func (s *server) startOperation(w http.ResponseWriter, kind, itemID string, fileID *string,
	body func(*operation) (map[string]any, []string, error)) {

	op := s.ops().start(kind, itemID, fileID)
	snapshot, _ := s.ops().get(op.ID)

	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		result, warnings, err := body(op)
		switch {
		case err != nil:
			s.ops().fail(op.ID, err, warnings)
			return
		}
		s.ops().succeed(op.ID, warnings, result)
	}()

	w.Header().Set("Location", "/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, snapshot)
}

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
		if err := s.qb.waitCheckDone(hash, budget(), s.pollInterval); err != nil {
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
	outcome := s.awaitDelete(target)
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

func (s *server) runDeleteWatched(op *operation, item *catalogItem, files []catalogFile) (map[string]any, []string, error) {
	warnings := []string{}
	if len(files) == 0 {
		return nil, warnings, errors.New("no watched files to delete")
	}
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
	for i, outcome := range s.awaitDeletes(targets) {
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

func (s *server) runFileEnable(op *operation, hash string, index int) (map[string]any, []string, error) {
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
		return nil, warnings, errors.New("torrent is being checked; enable was not applied")
	}
	files, err := s.qb.files(hash)
	if err != nil {
		return nil, warnings, err
	}
	file, ok := qbFileByID(files, index)
	if !ok {
		return nil, warnings, fmt.Errorf("file %d disappeared from the torrent", index)
	}
	abs, err := s.torrentFilePath(t, file)
	if err != nil {
		return nil, warnings, err
	}
	required, err := requiredFileBytes(abs, file.Size)
	if err != nil {
		return nil, warnings, err
	}
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
	startTimeout := 30 * time.Second
	if s.operationTimeout < startTimeout {
		startTimeout = s.operationTimeout
	}
	after, err := s.qb.waitStarted(hash, startTimeout, s.pollInterval)
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
		return nil, warnings, fmt.Errorf("file %d disappeared from the torrent", index)
	}
	if file.Priority == 0 {
		return nil, warnings, errors.New("file is excluded; enable it first")
	}
	if file.Priority != priority {
		s.ops().phase(op.ID, "prioritizing")
		if err := s.qb.setFilePrio(hash, index, priority); err != nil {
			return nil, warnings, err
		}
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
	log.Printf("add: ok (category=%q source=magnet)", category)
	s.poke()
	return map[string]any{"added": true}, []string{}, nil
}

func checkAddLink(link string) error {
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
