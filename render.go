package main

// render.go turns the internal catalog into what is sent to the client.
//
// This is the only place where actions are computed. They must not be computed
// in two places: if those diverge, the client shows a button the server then
// refuses to execute.

import "path/filepath"

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *server) renderList(cat *catalog) apiItemList {
	items := make([]apiItem, 0, len(cat.items))
	for _, item := range cat.items {
		items = append(items, s.renderItem(cat, item))
	}
	return apiItemList{Items: items, Degraded: nilIfEmpty(cat.degraded)}
}

func (s *server) renderItem(cat *catalog, item *catalogItem) apiItem {
	ready, watched := 0, 0
	for _, f := range item.files {
		if f.status == fileReady {
			ready++
		}
		if f.watch == watchWatched {
			watched++
		}
	}
	status := item.status
	if !cat.qbitOK && item.torrentBacked() {
		status = statusUnknown
	}
	return apiItem{
		ID: item.id, Title: item.title, Status: status,
		Progress: item.progress, Size: item.size, Downloaded: item.downloaded,
		FilesTotal: len(item.files), FilesReady: ready, FilesWatched: watched,
		Category: nilIfEmpty(item.category), Magnet: nilIfEmpty(item.magnet),
		AddedAt:  item.addedAt,
		Transfer: item.transfer,
		Actions:  s.itemActionList(cat, item),
	}
}

func (s *server) renderDetail(cat *catalog, item *catalogItem) apiItemDetail {
	files := make([]apiFile, 0, len(item.files))
	for i := range item.files {
		f := &item.files[i]
		// The catalog is shared by all requests and lives in the cache, so it
		// must not be written from here. A file outside the root cannot be
		// served, so we won't report it as "ready", but we only change the
		// local copy of the status.
		status := f.status
		if f.shelfRel == "" && status == fileReady {
			status = filePartial
		}
		files = append(files, apiFile{
			ID: f.id, Name: f.name, Path: f.rel, Size: f.size,
			Progress: f.progress, Status: status, Priority: nilIfEmpty(f.priority),
			Watch: nilIfEmpty(f.watch), Position: f.position,
			Actions: s.fileActionList(cat, item, f),
		})
	}
	return apiItemDetail{apiItem: s.renderItem(cat, item), Files: files}
}

// fileAbs returns the absolute path of a catalog file; ok=false if the file
// is outside the root.
func (s *server) fileAbs(f *catalogFile) (string, bool) {
	if f.shelfRel == "" {
		return "", false
	}
	return filepath.Join(s.root, filepath.FromSlash(f.shelfRel)), true
}
