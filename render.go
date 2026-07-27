package main

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
	ready := 0
	for _, f := range item.files {
		if f.status == fileReady {
			ready++
		}
	}
	status := item.status
	if !cat.qbitOK && item.torrentBacked() {
		status = statusUnknown
	}
	return apiItem{
		ID: item.id, Title: item.title, Status: status,
		Progress: item.progress, Size: item.size, Downloaded: item.downloaded,
		FilesTotal: len(item.files), FilesReady: ready,
		Category: nilIfEmpty(item.category), AddedAt: item.addedAt,
		Transfer: item.transfer,
		Actions:  s.itemActionList(cat, item),
	}
}

func (s *server) renderDetail(cat *catalog, item *catalogItem) apiItemDetail {
	files := make([]apiFile, 0, len(item.files))
	for i := range item.files {
		f := &item.files[i]
		status := f.status
		if f.shelfRel == "" && status == fileReady {
			status = filePartial
		}
		files = append(files, apiFile{
			ID: f.id, Name: f.name, Path: f.rel, Size: f.size,
			Progress: f.progress, Status: status,
			Watch: nilIfEmpty(f.watch), Position: f.position,
			Actions: s.fileActionList(cat, item, f),
		})
	}
	return apiItemDetail{
		apiItem: s.renderItem(cat, item),
		Magnet:  nilIfEmpty(item.magnet),
		Files:   files,
	}
}

func (s *server) fileAbs(f *catalogFile) (string, bool) {
	if f.shelfRel == "" {
		return "", false
	}
	return filepath.Join(s.root, filepath.FromSlash(f.shelfRel)), true
}
