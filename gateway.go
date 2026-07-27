package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *server) itemPath(t *qbTorrent) string {
	base := t.ContentPath
	if base == "" {
		base = filepath.Join(t.SavePath, t.Name)
	}
	if base == "" {
		return ""
	}
	rel, err := filepath.Rel(s.root, filepath.Clean(base))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (s *server) torrentView(t *qbTorrent) map[string]any {
	return map[string]any{
		"hash": t.Hash, "name": t.Name, "state": t.State,
		"progress": t.Progress, "category": t.Category,
		"save_path": t.SavePath, "content_path": t.ContentPath,
		"size": t.Size, "amount_left": t.AmountLeft,
		"force_start": t.ForceStart, "item_path": s.itemPath(t),
	}
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
	out := make([]map[string]any, 0, len(torrents))
	for i := range torrents {
		out = append(out, s.torrentView(&torrents[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "qbit_available": true, "torrents": out,
	})
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
	for i := range files {
		f := &files[i]
		id := i
		if f.Index != nil {
			id = *f.Index
		}
		rel := ""
		if abs, err := s.torrentFilePath(t, f); err == nil {
			if r, err := filepath.Rel(s.root, abs); err == nil {
				rel = filepath.ToSlash(r)
			}
		}
		ready := f.Progress >= 1
		if ready && rel != "" {
			info, statErr := os.Stat(filepath.Join(s.root, filepath.FromSlash(rel)))
			ready = statErr == nil && complete(info)
		}
		out = append(out, map[string]any{
			"id": id, "name": f.Name, "path": rel, "size": f.Size,
			"priority": f.Priority, "progress": f.Progress, "ready": ready,
		})
	}
	view := s.torrentView(t)
	view["ok"] = true
	view["files"] = out
	writeJSON(w, http.StatusOK, view)
}

func (s *server) handleCategories(w http.ResponseWriter, r *http.Request) {
	if s.qb == nil {
		writeErr(w, http.StatusNotImplemented, "qbittorrent is not configured")
		return
	}
	var raw map[string]qbCategory
	if err := s.qb.getJSON("/api/v2/torrents/categories", &raw); err != nil {
		writeErr(w, http.StatusBadGateway, "qbittorrent categories: %v", err)
		return
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sortStrings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{"name": name, "save_path": raw[name].SavePath})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "categories": out})
}

func (q *qbit) setForceStart(hash string, value bool) error {
	return q.postOK("/api/v2/torrents/setForceStart", url.Values{
		"hashes": {hash}, "value": {strconv.FormatBool(value)},
	})
}

func (s *server) streamedFileOf(t *qbTorrent) (string, error) {
	files, err := s.qb.files(t.Hash)
	if err != nil {
		return "", err
	}
	for i := range files {
		abs, err := s.torrentFilePath(t, &files[i])
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if s.streamReg().active(inodeKey(info)) {
			return files[i].Name, nil
		}
	}
	return "", nil
}

func parseDeleteFiles(r *http.Request) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("delete_files"))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("delete_files must be true or false")
	}
	return value, nil
}

func (s *server) checkAddLink(link string) error {
	trimmed := strings.TrimSpace(link)
	lower := strings.ToLower(trimmed)
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
