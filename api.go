package main

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /categories", s.handleCategories)
	mux.HandleFunc("GET /storage", s.handleStorage)
	mux.HandleFunc("GET /operations/{op}", s.handleOperation)

	mux.HandleFunc("POST /drafts", s.handleDraftCreate)
	mux.HandleFunc("GET /drafts/{draft}", s.handleDraft)
	mux.HandleFunc("GET /drafts/{draft}/events", s.handleDraftEvents)
	mux.HandleFunc("POST /drafts/{draft}/confirm", s.handleDraftConfirm)
	mux.HandleFunc("DELETE /drafts/{draft}", s.handleDraftDelete)

	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		s.serveEvents(w, r, topicList)
	})
	mux.HandleFunc("GET /items", s.handleItems)
	mux.HandleFunc("POST /items", s.handleItemCreate)
	mux.HandleFunc("GET /items/{item}", s.handleItem)
	mux.HandleFunc("DELETE /items/{item}", s.handleItemDelete)
	mux.HandleFunc("GET /items/{item}/events", s.handleItemEvents)
	mux.HandleFunc("POST /items/{item}/start", s.itemCommand(actionStart))
	mux.HandleFunc("POST /items/{item}/stop", s.itemCommand(actionStop))
	mux.HandleFunc("POST /items/{item}/recheck", s.itemCommand(actionRecheck))
	mux.HandleFunc("DELETE /items/{item}/viewed", s.handleItemViewedClear)

	mux.HandleFunc("DELETE /items/{item}/files", s.handleWatchedDelete)
	mux.HandleFunc("DELETE /items/{item}/files/{file}", s.handleFileDelete)
	mux.HandleFunc("POST /items/{item}/files/{file}/enable", s.handleFileEnable)
	mux.HandleFunc("PUT /items/{item}/files/{file}/priority", s.handleFilePriority)
	mux.HandleFunc("PUT /items/{item}/files/{file}/viewed", s.handleFileViewedSet)
	mux.HandleFunc("DELETE /items/{item}/files/{file}/viewed", s.handleFileViewedClear)
	mux.HandleFunc("GET /items/{item}/files/{file}/stream/{name...}", s.handleStream)
	mux.HandleFunc("HEAD /items/{item}/files/{file}/stream/{name...}", s.handleStream)

	return s.wrap(mux)
}

func (s *server) resolveItem(w http.ResponseWriter, r *http.Request) (*catalog, *catalogItem, bool) {
	id := r.PathValue("item")
	if _, err := parseID(id); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "malformed item id")
		return nil, nil, false
	}
	cat, item := s.lookupItem(id)
	if item == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such item")
		return nil, nil, false
	}
	return cat, item, true
}

func (s *server) resolveFile(w http.ResponseWriter, r *http.Request) (*catalog, *catalogItem, *catalogFile, bool) {
	cat, item, ok := s.resolveItem(w, r)
	if !ok {
		return nil, nil, nil, false
	}
	file := item.file(r.PathValue("file"))
	if file == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such file in this item")
		return nil, nil, nil, false
	}
	return cat, item, file, true
}

func writeDenial(w http.ResponseWriter, d *denial) {
	writeErr(w, d.status, d.code, "%s", d.reason)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	status := healthStatus{
		Version: version, Contract: contractVersion, Checks: map[string]string{},
	}
	healthy := true
	switch root := s.fsRoot(); {
	case root == nil:
		healthy = false
		status.Checks["root"] = "media root is not available"
	default:
		if _, err := root.Stat("."); err != nil {
			healthy = false
			status.Checks["root"] = err.Error()
		} else {
			status.Checks["root"] = "ok"
		}
	}
	if s.viewed == nil {
		healthy = false
		status.Checks["viewed"] = "not configured"
	} else {
		status.Checks["viewed"] = "ok"
	}
	if s.qb != nil {
		if err := s.probe.check(s.qb); err != nil {
			healthy = false
			status.Checks["qbittorrent"] = err.Error()
		} else {
			status.Checks["qbittorrent"] = "ok"
		}
	}
	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}

func (s *server) handleCategories(w http.ResponseWriter, _ *http.Request) {
	if s.qb == nil {
		writeJSON(w, http.StatusOK, map[string]any{"categories": []any{}})
		return
	}
	var raw map[string]qbCategory
	if err := s.qb.getJSON("/api/v2/torrents/categories", &raw); err != nil {
		writeErr(w, http.StatusBadGateway, codeQbitUnavailable, "qbittorrent categories: %v", err)
		return
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sortStrings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{"name": name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": out})
}

func (s *server) handleItems(w http.ResponseWriter, _ *http.Request) {
	cat := s.buildCatalog()
	s.catalog.Store(cat)
	writeJSON(w, http.StatusOK, s.renderList(cat))
}

func (s *server) handleItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("item")
	if _, err := parseID(id); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "malformed item id")
		return
	}
	cat := s.buildCatalog()
	s.catalog.Store(cat)
	item := cat.item(id)
	if item == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such item")
		return
	}
	writeJSON(w, http.StatusOK, s.renderDetail(cat, item))
}

func (s *server) handleItemEvents(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.resolveItem(w, r); !ok {
		return
	}
	s.serveEvents(w, r, r.PathValue("item"))
}

func (s *server) handleOperation(w http.ResponseWriter, r *http.Request) {
	op, ok := s.ops().get(r.PathValue("op"))
	if !ok {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such operation")
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *server) itemCommand(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cat, item, ok := s.resolveItem(w, r)
		if !ok {
			return
		}
		if d := s.permitItem(cat, item, action); d != nil {
			writeDenial(w, d)
			return
		}
		hash, kind := item.hash, "item."+action
		s.startOperation(w, kind, item.id, nil, nil, func(op *operation) (map[string]any, []string, error) {
			return s.runItemCommand(op, hash, action)
		})
	}
}

func (s *server) handleItemDelete(w http.ResponseWriter, r *http.Request) {
	cat, item, ok := s.resolveItem(w, r)
	if !ok {
		return
	}
	deleteFiles, err := parseBoolQuery(r, "delete_files")
	if err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	action := actionRemove
	if deleteFiles {
		action = actionRemoveWithData
	}
	if d := s.permitItem(cat, item, action); d != nil {
		writeDenial(w, d)
		return
	}
	if deleteFiles {
		if name, err := s.streamedFileOf(item); err != nil {
			writeErr(w, http.StatusInternalServerError, codeInternal, "%v", err)
			return
		} else if name != "" {
			writeErr(w, http.StatusConflict, codeStreaming,
				"file %q is being streamed right now; nothing removed", name)
			return
		}
	}
	snapshot := *item
	var doomed []string
	if deleteFiles {
		for i := range item.files {
			doomed = append(doomed, item.files[i].id)
		}
	}
	s.startOperation(w, "item."+action, item.id, nil, doomed, func(op *operation) (map[string]any, []string, error) {
		return s.runItemRemove(op, &snapshot, deleteFiles)
	})
}

func (s *server) handleItemCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Magnet   string `json:"magnet"`
		Category string `json:"category"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	if s.qb == nil {
		writeErr(w, http.StatusBadGateway, codeQbitUnavailable, "qbittorrent is not configured")
		return
	}
	if err := checkAddLink(body.Magnet); err != nil {
		writeErr(w, http.StatusForbidden, codeRefused, "%v", err)
		return
	}
	if err := checkCategory(body.Category); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	magnet, category := body.Magnet, body.Category
	s.startOperation(w, "item.add", "", nil, nil, func(op *operation) (map[string]any, []string, error) {
		return s.runAddMagnet(op, magnet, category)
	})
}

func (s *server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	cat, item, file, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	if d := s.permitFile(cat, item, file, actionDelete); d != nil {
		writeDenial(w, d)
		return
	}
	itemCopy, fileCopy := *item, *file
	unreserve, free, err := s.reserveDeleteFile(&fileCopy)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, codeInternal, "reserve file for delete: %v", err)
		return
	}
	if !free {
		writeErr(w, http.StatusConflict, codeStreaming,
			"file is being streamed right now; nothing deleted")
		return
	}
	s.startOperation(w, "file.delete", item.id, &fileCopy.id, []string{fileCopy.id},
		func(op *operation) (map[string]any, []string, error) {
			defer unreserve()
			return s.runFileDelete(op, &itemCopy, &fileCopy)
		})
}

func (s *server) handleWatchedDelete(w http.ResponseWriter, r *http.Request) {
	cat, item, ok := s.resolveItem(w, r)
	if !ok {
		return
	}
	watched, err := parseBoolQuery(r, "watched")
	if err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	if !watched {
		writeErr(w, http.StatusBadRequest, codeBadRequest,
			"watched=true is required; deleting every file of an item is not offered")
		return
	}
	if d := s.permitItem(cat, item, actionDeleteWatched); d != nil {
		writeDenial(w, d)
		return
	}
	itemCopy := *item
	files := make([]catalogFile, 0, len(item.files))
	doomed := make([]string, 0, len(item.files))
	for _, file := range s.watchedToDelete(cat, item) {
		files = append(files, *file)
		doomed = append(doomed, file.id)
	}
	s.startOperation(w, "item."+actionDeleteWatched, item.id, nil, doomed,
		func(op *operation) (map[string]any, []string, error) {
			return s.runDeleteWatched(op, &itemCopy, files)
		})
}

func (s *server) handleFileEnable(w http.ResponseWriter, r *http.Request) {
	cat, item, file, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	if d := s.permitFile(cat, item, file, actionEnable); d != nil {
		writeDenial(w, d)
		return
	}
	hash, index, id := item.hash, file.qbitIndex, file.id
	s.startOperation(w, "file.enable", item.id, &id, nil,
		func(op *operation) (map[string]any, []string, error) {
			return s.runFileEnable(op, hash, index)
		})
}

func (s *server) handleFilePriority(w http.ResponseWriter, r *http.Request) {
	cat, item, file, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	var body struct {
		Priority string `json:"priority"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	priority, known := priorityValue(body.Priority)
	if !known {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "unknown priority %q", body.Priority)
		return
	}
	if d := s.permitFile(cat, item, file, actionSetPriority); d != nil {
		writeDenial(w, d)
		return
	}
	hash, index, id := item.hash, file.qbitIndex, file.id
	s.startOperation(w, "file.priority", item.id, &id, nil,
		func(op *operation) (map[string]any, []string, error) {
			return s.runFilePriority(op, hash, index, priority)
		})
}

func (s *server) handleFileViewedSet(w http.ResponseWriter, r *http.Request) {
	cat, item, file, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	if body.State == "" {
		body.State = watchWatched
	}
	if body.State != watchWatched && body.State != watchStarted {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "unknown state %q", body.State)
		return
	}
	if d := s.permitFile(cat, item, file, actionSetViewed); d != nil {
		writeDenial(w, d)
		return
	}
	if err := s.markViewed(file.shelfRel, body.State); err != nil {
		writeErr(w, http.StatusInternalServerError, codeInternal, "save viewed: %v", err)
		return
	}
	s.poke()
	writeNoContent(w)
}

func (s *server) handleFileViewedClear(w http.ResponseWriter, r *http.Request) {
	cat, item, file, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	if d := s.permitFile(cat, item, file, actionClearViewed); d != nil {
		writeDenial(w, d)
		return
	}
	if err := s.clearViewedFile(file.shelfRel); err != nil {
		writeErr(w, http.StatusInternalServerError, codeInternal, "save viewed: %v", err)
		return
	}
	s.poke()
	writeNoContent(w)
}

func (s *server) handleItemViewedClear(w http.ResponseWriter, r *http.Request) {
	cat, item, ok := s.resolveItem(w, r)
	if !ok {
		return
	}
	if d := s.permitItem(cat, item, actionClearViewed); d != nil {
		writeDenial(w, d)
		return
	}
	if err := s.clearViewedItem(item.shelfRel); err != nil {
		writeErr(w, http.StatusInternalServerError, codeInternal, "save viewed: %v", err)
		return
	}
	s.poke()
	writeNoContent(w)
}

func parseBoolQuery(r *http.Request, key string) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errBadQuery(key)
	}
	return value, nil
}

type queryError string

func (e queryError) Error() string { return string(e) + " must be true or false" }

func errBadQuery(key string) error { return queryError(key) }
