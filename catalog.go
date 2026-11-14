package main

// catalog.go assembles the picture of the world: qBittorrent plus the disk.
//
// The client used to do this by merging two lists by file name. Both data-loss
// scenarios grew out of that: matching names in different categories deleted
// someone else's torrent, and an outage of one of the sources turned a
// finished file into "downloading" with an offer to cancel the download.
//
// Here the merge happens once, in one place, and the result already contains
// everything the client needs for rendering and for actions.

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// qbTorrentSnapshot is a torrent together with its file list, captured in a
// single pass. A separate type keeps catalog building from hitting the network
// midway.
type qbTorrentSnapshot struct {
	qbTorrent
	files []qbFile
}

type catalogFile struct {
	id       string // opaque, for the client
	name     string
	rel      string // path relative to the item, for display
	shelfRel string // path relative to the catalog root; "" if outside the root
	size     int64
	progress float64
	status   string
	priority string // "" if the file is excluded or there is no torrent

	watch    string // "", "started" or "watched"
	position int64

	qbitIndex int // -1 if the file is not in the torrent
	ambiguous bool
}

type catalogItem struct {
	// ambiguous: another torrent shares the item or its files. There is no way
	// to address them separately, so destructive actions are forbidden
	// entirely, both at the file level and at the item level.
	ambiguous bool

	id       string
	title    string
	shelfRel string // item path relative to the root; "" if outside the root
	hash     string // "" if there is no torrent
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

// ownerKey identifies exactly who claims a file. A pair rather than a hash,
// because one torrent can list the same path twice.
type ownerKey struct {
	hash  string
	index int
}

// ownershipIndex catches files claimed by more than one torrent. There are two
// keys because overlaps come in two kinds: the same path (two torrents in one
// folder) and the same inode (a hardlink between torrents). Checking only for
// matching item ids is not enough: that way an ambiguity with DIFFERENT ids
// went undetected entirely.
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

// contested reports whether more than one owner claims the file.
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

// relToRoot converts an absolute path into a catalog path. ok=false means the
// path lies outside the root: we show such an item, but there is nothing in it
// to play or delete, since it has no catalog files.
func (s *server) relToRoot(abs string) (string, bool) {
	rel, err := filepath.Rel(s.root, filepath.Clean(abs))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// diskStatus determines how complete a file is from the disk. It is separate
// from qBittorrent because qBittorrent considers a deleted file intact until
// the recheck.
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
	// Drafts are filtered out before anything else, including the ownership
	// index: a torrent the user is still only considering in the file picker
	// must neither show up in the catalog nor make a real item with the same
	// files ambiguous.
	torrents = withoutDrafts(torrents)
	// The ownership index is built BEFORE the items: you can only tell that two
	// torrents share a file by seeing all the torrents at once.
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

	claimed := map[string]bool{} // catalog paths already taken by torrents
	for i := range torrents {
		item := s.itemFromTorrent(&torrents[i], owners)
		if existing, clash := cat.byID[item.id]; clash {
			// Two torrents on one path: there is no way to address them
			// separately, so both are marked ambiguous and lose all actions.
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
			// While qBittorrent is silent we don't know whether a torrent backs
			// the item, so we don't pass it off as a standalone shelf.
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

// torrentContentAbs returns the item's root on disk: the folder for a
// multi-file torrent, or the file itself for a single-file one.
func torrentContentAbs(t *qbTorrent) string {
	if t.ContentPath != "" {
		return t.ContentPath
	}
	return filepath.Join(t.SavePath, t.Name)
}

// torrentTitle returns the item's title for the catalog: the name of its
// content on disk, not the torrent's name in qBittorrent. The torrent name is
// taken from the magnet link's dn and doesn't change once metadata arrives,
// and trackers put whatever they like there: rutor uses "rutor.info" for every
// torrent. The root folder (for a single-file torrent, the file itself) is
// named the way the release's author named it.
func torrentTitle(t *qbTorrent, single bool) string {
	if t.ContentPath == "" {
		return t.Name
	}
	name := filepath.Base(filepath.Clean(t.ContentPath))
	if single {
		// An extension in a movie title is noise; for episodes it shows in the
		// file list.
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if name == "" || name == "." || name == string(filepath.Separator) {
		return t.Name
	}
	return name
}

// torrentFileWithin returns the file's path RELATIVE to the item; the file id
// is built from it. Drafts (drafts.go) must compute it exactly the same way,
// otherwise a file's id before and after confirmation would differ.
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
		title: torrentTitle(&t.qbTorrent, len(t.files) == 1), shelfRel: shelfRel, hash: t.Hash,
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

	// With a single file in the torrent, content_path points at the file itself,
	// and the path within the item degenerates into the name.
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
			// One file shared by two torrents: touching it would silently change
			// the other torrent without stopping or rechecking it.
			item.ambiguous = true
		}
		item.files = append(item.files, catalogFile{
			id: fileID(within), name: filepath.Base(within), rel: within,
			shelfRel: fileShelfRel, size: f.Size, progress: f.Progress,
			status: status, priority: priorityName(f.Priority),
			qbitIndex: index, ambiguous: contested,
		})
	}
	sort.Slice(item.files, func(i, j int) bool { return fileOrderLess(item.files[i].rel, item.files[j].rel) })
	return item
}

// fileOrderLess orders the files within an item. The catalog and drafts share
// it: the episode picker must list the files the same way the item screen will
// later. qBittorrent's index order is the order inside the .torrent, and some
// releases have their episodes shuffled there.
func fileOrderLess(a, b string) bool { return a < b }

func normalizeETA(eta int64) *int64 {
	// qBittorrent reports 8640000 (100 days) as "unknown".
	if eta <= 0 || eta >= 8640000 {
		return nil
	}
	return &eta
}

// shelfItems finds items not backed by any torrent: top-level folders inside
// categories and standalone files in the root.
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
		// A top-level folder taken by a torrent must not be treated as a
		// category: it is an item sitting right in the catalog root (this happens
		// when no category was chosen). Otherwise its own files would pop up next
		// to it as separate items.
		if rel, ok := s.relToRoot(abs); ok && claimed[rel] {
			continue
		}
		// The remaining top-level folders are qBittorrent categories; they are
		// transparent.
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
		// Broken permissions or a detached disk must not look like "the item is
		// empty": better not to show it at all than to show it incomplete.
		log.Printf("shelf: walk %q: %v", dirAbs, err)
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return fileOrderLess(files[i].rel, files[j].rel) })
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
