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
		if err != nil {
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

	abs, ok := s.fileAbs(file)
	if !ok {
		return nil, nil, errors.New("file lies outside the media root")
	}
	info, err := os.Stat(abs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		info = nil
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return nil, nil, errors.New("file lies outside the media root")
	}

	s.ops().phase(op.ID, "waiting")
	outcome := s.awaitDelete(&deleteTarget{
		rel: rel, abs: abs, info: info,
		match: qbMatch{hash: item.hash, fileID: file.qbitIndex, abs: abs},
	})
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

func (s *server) removeShelfFile(file *catalogFile) error {
	abs, ok := s.fileAbs(file)
	if !ok {
		return errors.New("file lies outside the media root")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if s.streamReg().active(inodeKey(info)) {
		return errors.New("file is being streamed right now")
	}
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

	if s.cancelQueuedDelete(hash, index) {
		warnings = append(warnings, "a queued delete for this file was cancelled")
	}

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

func (s *server) runAddTorrentFile(op *operation, name, category string) (map[string]any, []string, error) {
	s.ops().phase(op.ID, "adding")
	data, err := os.ReadFile(filepath.Join(s.torrentDir, name))
	if err != nil {
		return nil, nil, err
	}
	fields := map[string]string{}
	if category != "" {
		fields["category"] = category
	}
	if err := s.qb.addTorrentFile(name, data, fields); err != nil {
		return nil, nil, err
	}
	log.Printf("add: ok (category=%q source=torrent-file)", category)
	s.poke()
	return map[string]any{"added": true}, []string{}, nil
}

func (s *server) checkAddLink(link string) error {
	lower := strings.ToLower(strings.TrimSpace(link))
	switch {
	case strings.HasPrefix(lower, "magnet:?"):
		return nil
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		if s.allowURLAdd {
			return nil
		}
		return errors.New("http(s) links are refused; set TORRMEDIA_ALLOW_URL_ADD=true to fetch them server-side")
	default:
		return errors.New("only magnet: links are accepted")
	}
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
