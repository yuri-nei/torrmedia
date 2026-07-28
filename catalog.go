package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type qbTorrentSnapshot struct {
	qbTorrent
	files []qbFile
}

type catalogFile struct {
	id       string
	name     string
	rel      string
	shelfRel string
	size     int64
	progress float64
	status   string

	watch    string
	position int64

	qbitIndex int
	ambiguous bool
}

type catalogItem struct {
	ambiguous bool

	id       string
	title    string
	shelfRel string
	hash     string
	category string
	magnet   string
	status   string
	addedAt  int64
	transfer apiTransfer
	files    []catalogFile

	size       int64
	downloaded int64
	progress   float64
}

func (c *catalogItem) torrentBacked() bool { return c.hash != "" }

type ownerKey struct {
	hash  string
	index int
}

type ownershipIndex struct {
	byPath  map[string][]ownerKey
	byInode map[string][]ownerKey
}

func newOwnershipIndex() *ownershipIndex {
	return &ownershipIndex{byPath: map[string][]ownerKey{}, byInode: map[string][]ownerKey{}}
}

func (idx *ownershipIndex) add(abs string, owner ownerKey) {
	key := filepath.Clean(abs)
	idx.byPath[key] = appendOwner(idx.byPath[key], owner)
	if info, err := os.Stat(abs); err == nil {
		if ino := inodeKey(info); ino != "" {
			idx.byInode[ino] = appendOwner(idx.byInode[ino], owner)
		}
	}
}

func appendOwner(owners []ownerKey, owner ownerKey) []ownerKey {
	for _, existing := range owners {
		if existing == owner {
			return owners
		}
	}
	return append(owners, owner)
}

func (idx *ownershipIndex) contested(abs string) bool {
	if len(idx.byPath[filepath.Clean(abs)]) > 1 {
		return true
	}
	if info, err := os.Stat(abs); err == nil {
		if ino := inodeKey(info); ino != "" && len(idx.byInode[ino]) > 1 {
			return true
		}
	}
	return false
}

type catalog struct {
	items    []*catalogItem
	byID     map[string]*catalogItem
	degraded string
	qbitOK   bool
}

func (c *catalog) item(id string) *catalogItem { return c.byID[id] }

func (c *catalogItem) file(id string) *catalogFile {
	for i := range c.files {
		if c.files[i].id == id {
			return &c.files[i]
		}
	}
	return nil
}

func (s *server) relToRoot(abs string) (string, bool) {
	rel, err := filepath.Rel(s.root, filepath.Clean(abs))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func diskStatus(abs string) (status string, size int64, progress float64) {
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return fileMissing, 0, 0
	}
	if complete(info) {
		return fileReady, info.Size(), 1
	}
	if info.Size() <= 0 {
		return fileMissing, 0, 0
	}
	return filePartial, info.Size(), float64(allocatedBytes(info)) / float64(info.Size())
}

func (s *server) buildCatalog() *catalog {
	cat := &catalog{byID: map[string]*catalogItem{}, qbitOK: true}

	torrents, err := s.qbitSnapshot()
	if err != nil {
		cat.qbitOK = false
		cat.degraded = "qbittorrent is unavailable: " + err.Error()
	}
	torrents = withoutDrafts(torrents)
	owners := newOwnershipIndex()
	for i := range torrents {
		t := &torrents[i]
		for j := range t.files {
			f := &t.files[j]
			if t.SavePath == "" || f.Name == "" || filepath.IsAbs(f.Name) {
				continue
			}
			index := j
			if f.Index != nil {
				index = *f.Index
			}
			owners.add(filepath.Join(t.SavePath, filepath.FromSlash(f.Name)),
				ownerKey{hash: t.Hash, index: index})
		}
	}

	claimed := map[string]bool{}
	for i := range torrents {
		item := s.itemFromTorrent(&torrents[i], owners)
		if existing, clash := cat.byID[item.id]; clash {
			markAmbiguous(existing)
			markAmbiguous(item)
			continue
		}
		cat.byID[item.id] = item
		cat.items = append(cat.items, item)
		if item.shelfRel != "" {
			claimed[item.shelfRel] = true
		}
	}

	for _, shelf := range s.shelfItems(claimed) {
		if _, clash := cat.byID[shelf.id]; clash {
			continue
		}
		if !cat.qbitOK {
			shelf.status = statusUnknown
		}
		cat.byID[shelf.id] = shelf
		cat.items = append(cat.items, shelf)
	}

	sort.Slice(cat.items, func(i, j int) bool { return cat.items[i].title < cat.items[j].title })
	s.enrichCatalogViewed(cat)
	return cat
}

func markAmbiguous(item *catalogItem) {
	item.ambiguous = true
	item.status = statusError
	for i := range item.files {
		item.files[i].ambiguous = true
	}
}

func torrentContentAbs(t *qbTorrent) string {
	if t.ContentPath != "" {
		return t.ContentPath
	}
	return filepath.Join(t.SavePath, t.Name)
}

func torrentFileWithin(contentAbs, savePath, name string, single bool) string {
	within := filepath.ToSlash(name)
	if single {
		return filepath.Base(within)
	}
	abs := filepath.Join(savePath, filepath.FromSlash(name))
	if rel, err := filepath.Rel(contentAbs, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return within
}

func (s *server) itemFromTorrent(t *qbTorrentSnapshot, owners *ownershipIndex) *catalogItem {
	contentAbs := torrentContentAbs(&t.qbTorrent)
	shelfRel, inRoot := s.relToRoot(contentAbs)
	item := &catalogItem{
		title: t.Name, shelfRel: shelfRel, hash: t.Hash,
		category: t.Category, magnet: t.MagnetURI,
		status: normalizeState(t.State), addedAt: t.AddedOn,
		size: t.Size, downloaded: t.Completed, progress: t.Progress,
		transfer: apiTransfer{
			Down: t.DlSpeed, Up: t.UpSpeed, Ratio: t.Ratio,
			Seeds: t.NumSeeds, Peers: t.NumLeechs, ETA: normalizeETA(t.ETA),
		},
	}
	if inRoot {
		item.id = pathID(shelfRel)
	} else {
		item.id = hashID(t.Hash)
	}

	singleFile := len(t.files) == 1
	for i := range t.files {
		f := &t.files[i]
		index := i
		if f.Index != nil {
			index = *f.Index
		}
		abs := filepath.Join(t.SavePath, filepath.FromSlash(f.Name))
		within := torrentFileWithin(contentAbs, t.SavePath, f.Name, singleFile)
		fileShelfRel, fileInRoot := s.relToRoot(abs)
		if !fileInRoot {
			fileShelfRel = ""
		}

		status, _, _ := diskStatus(abs)
		if f.Priority == 0 {
			status = fileSkipped
		}
		contested := owners.contested(abs)
		if contested {
			item.ambiguous = true
		}
		item.files = append(item.files, catalogFile{
			id: fileID(within), name: filepath.Base(within), rel: within,
			shelfRel: fileShelfRel, size: f.Size, progress: f.Progress,
			status: status, qbitIndex: index, ambiguous: contested,
		})
	}
	sort.Slice(item.files, func(i, j int) bool { return item.files[i].rel < item.files[j].rel })
	return item
}

func normalizeETA(eta int64) *int64 {
	if eta <= 0 || eta >= 8640000 {
		return nil
	}
	return &eta
}

func (s *server) shelfItems(claimed map[string]bool) []*catalogItem {
	items := []*catalogItem{}
	add := func(abs string, dir bool) {
		rel, ok := s.relToRoot(abs)
		if !ok || claimed[rel] {
			return
		}
		item := &catalogItem{
			id: pathID(rel), title: filepath.Base(abs), shelfRel: rel,
			status: statusShelf, progress: 1,
		}
		if dir {
			item.files = s.shelfFiles(abs, rel)
		} else if f, ok := s.shelfFile(abs, rel, filepath.Base(abs)); ok {
			item.files = []catalogFile{f}
		}
		if len(item.files) == 0 {
			return
		}
		for _, f := range item.files {
			item.size += f.size
			if f.status == fileReady {
				item.downloaded += f.size
			}
		}
		items = append(items, item)
	}

	top, err := os.ReadDir(s.root)
	if err != nil {
		log.Printf("shelf: read root: %v", err)
		return items
	}
	for _, entry := range top {
		if hidden(entry.Name()) {
			continue
		}
		abs := filepath.Join(s.root, entry.Name())
		if !entry.IsDir() {
			if entry.Type().IsRegular() {
				add(abs, false)
			}
			continue
		}
		if rel, ok := s.relToRoot(abs); ok && claimed[rel] {
			continue
		}
		sub, err := os.ReadDir(abs)
		if err != nil {
			log.Printf("shelf: read %q: %v", abs, err)
			continue
		}
		for _, child := range sub {
			if hidden(child.Name()) {
				continue
			}
			childAbs := filepath.Join(abs, child.Name())
			switch {
			case child.IsDir():
				add(childAbs, true)
			case child.Type().IsRegular():
				add(childAbs, false)
			}
		}
	}
	return items
}

func (s *server) shelfFile(abs, shelfRel, within string) (catalogFile, bool) {
	status, size, progress := diskStatus(abs)
	if status == fileMissing {
		return catalogFile{}, false
	}
	return catalogFile{
		id: fileID(within), name: filepath.Base(within), rel: within,
		shelfRel: shelfRel, size: size, progress: progress,
		status: status, qbitIndex: -1,
	}, true
}

func (s *server) shelfFiles(dirAbs, dirRel string) []catalogFile {
	files := []catalogFile{}
	err := filepath.WalkDir(dirAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dirAbs && hidden(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		within, relErr := filepath.Rel(dirAbs, p)
		if relErr != nil {
			return relErr
		}
		shelfRel, ok := s.relToRoot(p)
		if !ok {
			return nil
		}
		if f, ok := s.shelfFile(p, shelfRel, filepath.ToSlash(within)); ok {
			files = append(files, f)
		}
		return nil
	})
	if err != nil {
		log.Printf("shelf: walk %q: %v", dirAbs, err)
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files
}

func (s *server) enrichCatalogViewed(cat *catalog) {
	if s.viewed == nil {
		return
	}
	viewed := s.viewed.snapshot()
	for _, item := range cat.items {
		for i := range item.files {
			entry, ok := viewed[filepath.FromSlash(item.files[i].shelfRel)]
			if !ok || entry.State == "" {
				continue
			}
			item.files[i].watch = entry.State
			item.files[i].position = entry.Pos
		}
	}
}
