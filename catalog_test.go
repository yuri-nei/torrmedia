package main

// catalog_test.go covers building the picture of the world. This is where
// the client used to get things wrong: this is exactly where we check that
// merging qBittorrent with the disk gives a single consistent answer.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogMergesTorrentAndDisk(t *testing.T) {
	env := newTestEnv(t)
	cat, item := env.firstItem(t)

	if len(cat.items) != 1 {
		t.Fatalf("items=%d, want 1", len(cat.items))
	}
	if item.title != "Release" || item.category != "Shows" {
		t.Fatalf("item=%+v", item)
	}
	if !item.torrentBacked() {
		t.Fatal("torrent-backed item reported as shelf-only")
	}
	if len(item.files) != 2 {
		t.Fatalf("files=%d, want 2", len(item.files))
	}
	for _, f := range item.files {
		if f.status != fileReady {
			t.Fatalf("file %q status=%s, want ready", f.name, f.status)
		}
		if f.shelfRel == "" {
			t.Fatalf("file %q has no shelf path", f.name)
		}
	}
}

// An item without a category sits right in the catalog root, and its folder
// is easy to mistake for a category; the downloaded files then pop up next to
// the item as separate items. Found in a live run: one item was shown
// twice.
func TestItemInRootIsNotTreatedAsCategory(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	writeTestFile(t, filepath.Join(root, "Release", "e01.mkv"), size)
	writeTestFile(t, filepath.Join(root, "Release", "e02.mkv"), size)

	fake := newFakeQbit(&fakeTorrent{
		hash: "0123456789012345678901234567890123456789", name: "Release",
		savePath: root, state: "downloading",
		files: []fakeFile{
			{name: filepath.Join("Release", "e01.mkv"), size: size, priority: 1, progress: 1},
			{name: filepath.Join("Release", "e02.mkv"), size: size, priority: 1, progress: 1},
		},
	})
	env := newTestEnvWith(t, root, fake)

	cat := env.buildCatalog()
	if len(cat.items) != 1 {
		names := []string{}
		for _, item := range cat.items {
			names = append(names, item.title)
		}
		t.Fatalf("items=%v, want one", names)
	}
	if len(cat.items[0].files) != 2 {
		t.Fatalf("files=%d, want 2", len(cat.items[0].files))
	}
}

// A file excluded from the torrent must differ from both a missing and a
// ready one: the whole UI and all actions rest on this distinction.
func TestCatalogFileStatuses(t *testing.T) {
	env := newTestEnv(t)
	const size = int64(1 << 20)

	// e02 is excluded from the torrent; e03 is in the torrent but not on
	// disk.
	env.fake.mu.Lock()
	env.fake.torrents[0].files[1].priority = 0
	env.fake.torrents[0].files = append(env.fake.torrents[0].files, fakeFile{
		name: filepath.Join("Release", "e03.mkv"), size: size, priority: 1,
	})
	env.fake.mu.Unlock()

	// e04 is on disk but incomplete.
	partial := filepath.Join(env.root, "shows", "Release", "e04.mkv")
	writeTestFile(t, partial, size)
	if err := os.Truncate(partial, size); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.OpenFile(partial, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	sparse.Truncate(0)
	sparse.Truncate(size) // logical size set, no blocks allocated
	sparse.Close()

	_, item := env.firstItem(t)
	got := map[string]string{}
	for _, f := range item.files {
		got[f.name] = f.status
	}
	for name, want := range map[string]string{
		"e01.mkv": fileReady,
		"e02.mkv": fileSkipped,
		"e03.mkv": fileMissing,
	} {
		if got[name] != want {
			t.Fatalf("%s status=%q, want %q (all: %v)", name, got[name], want, got)
		}
	}
}

// An item without a torrent must be visible and playable: torrmedia is
// self-sufficient.
func TestCatalogShowsShelfOnlyItem(t *testing.T) {
	env := newTestEnv(t)
	writeTestFile(t, filepath.Join(env.root, "movies", "Solo.mkv"), 1<<20)

	cat := env.buildCatalog()
	var shelf *catalogItem
	for _, item := range cat.items {
		if item.title == "Solo.mkv" {
			shelf = item
		}
	}
	if shelf == nil {
		t.Fatalf("shelf-only item is missing: %v", cat.items)
	}
	if shelf.torrentBacked() {
		t.Fatal("shelf-only item claims a torrent")
	}
	if shelf.status != statusShelf {
		t.Fatalf("status=%q, want %q", shelf.status, statusShelf)
	}
	if len(shelf.files) != 1 || shelf.files[0].status != fileReady {
		t.Fatalf("files=%+v", shelf.files)
	}
}

// qBittorrent being unreachable does not mean "the files belong to nobody".
// What is already downloaded can be watched, but the item's state is unknown
// and no actions are offered.
func TestCatalogDegradesWhenQbitIsDown(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.failInfo = true
	env.fake.mu.Unlock()

	cat := env.buildCatalog()
	if cat.qbitOK {
		t.Fatal("qbitOK=true while qBittorrent is failing")
	}
	if cat.degraded == "" {
		t.Fatal("degraded is empty")
	}
	if len(cat.items) != 1 {
		t.Fatalf("items=%d, want the shelf copy of the release", len(cat.items))
	}
	item := cat.items[0]
	if item.status != statusUnknown {
		t.Fatalf("status=%q, want %q", item.status, statusUnknown)
	}
	rendered := env.renderDetail(cat, item)
	if len(rendered.Actions) != 0 {
		t.Fatalf("degraded item still offers actions: %v", rendered.Actions)
	}
	for _, f := range rendered.Files {
		for _, action := range f.Actions {
			if action == actionDelete || action == actionEnable {
				t.Fatalf("degraded file offers %q: %+v", action, f)
			}
		}
		if f.Status == fileReady && !contains(f.Actions, actionPlay) {
			t.Fatalf("ready file cannot be played while degraded: %+v", f)
		}
	}
}

// Two torrents on one path cannot be addressed separately, so both lose
// their actions.
func TestCatalogMarksAmbiguousItems(t *testing.T) {
	env := newTestEnv(t)
	twin := *env.fake.torrents[0]
	twin.hash = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	env.fake.mu.Lock()
	env.fake.torrents = append(env.fake.torrents, &twin)
	env.fake.mu.Unlock()

	cat := env.buildCatalog()
	env.catalog.Store(cat)
	if len(cat.items) != 1 {
		t.Fatalf("items=%d, want one ambiguous item", len(cat.items))
	}
	detail := env.renderDetail(cat, cat.items[0])
	// The item must lose ALL actions: remove_with_data would take the second
	// torrent's data with it, and recheck would check the wrong one.
	if len(detail.Actions) != 0 {
		t.Fatalf("ambiguous item still offers %v", detail.Actions)
	}
	for _, f := range detail.Files {
		if contains(f.Actions, actionDelete) || contains(f.Actions, actionEnable) {
			t.Fatalf("ambiguous file still offers a destructive action: %+v", f)
		}
	}
}

// Matching ids are a special case. The other one is more dangerous: the items
// have DIFFERENT ids but share a file (a common path or a hardlink). This
// used to go undetected entirely.
func TestCatalogDetectsSharedFileAcrossDifferentItems(t *testing.T) {
	env := newTestEnv(t)
	const size = int64(1 << 20)
	shared := filepath.Join("Release", "e01.mkv") // same path, other torrent

	env.fake.mu.Lock()
	env.fake.torrents = append(env.fake.torrents, &fakeTorrent{
		hash: "abcdefabcdefabcdefabcdefabcdefabcdefabcd", name: "Other",
		savePath: env.fake.torrents[0].savePath, state: "stoppedUP",
		files: []fakeFile{{name: shared, size: size, priority: 1, progress: 1}},
	})
	env.fake.mu.Unlock()

	cat := env.buildCatalog()
	env.catalog.Store(cat)
	if len(cat.items) != 2 {
		t.Fatalf("items=%d, want two distinct ids", len(cat.items))
	}
	for _, item := range cat.items {
		if !item.ambiguous {
			t.Fatalf("item %q sharing a file is not marked ambiguous", item.title)
		}
		if actions := env.itemActionList(cat, item); len(actions) != 0 {
			t.Fatalf("item %q sharing a file offers %v", item.title, actions)
		}
		for i := range item.files {
			if item.files[i].rel != "e01.mkv" {
				continue
			}
			if !item.files[i].ambiguous {
				t.Fatalf("shared file in %q is not marked ambiguous", item.title)
			}
			if contains(env.fileActionList(cat, item, &item.files[i]), actionDelete) {
				t.Fatalf("shared file in %q still offers delete", item.title)
			}
		}
	}
}

// An item's id does not change whether or not its torrent has been removed:
// otherwise the client's open screen would fall apart right at the moment
// the torrent is deleted.
func TestItemIDSurvivesTorrentRemoval(t *testing.T) {
	env := newTestEnv(t)
	_, before := env.firstItem(t)

	env.fake.mu.Lock()
	env.fake.torrents = nil
	env.fake.mu.Unlock()

	cat := env.buildCatalog()
	after := cat.item(before.id)
	if after == nil {
		t.Fatalf("item id %q disappeared with the torrent", before.id)
	}
	if after.torrentBacked() {
		t.Fatal("item still claims a torrent")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// A torrent's name in qBittorrent is the magnet link's dn, and on rutor it is
// the same for every torrent ("rutor.info"). The catalog names an item after
// its contents.
func TestTitleComesFromContentNotMagnetName(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents[0].dn = "rutor.info"
	env.fake.mu.Unlock()
	_, item := env.firstItem(t)
	if item.title != "Release" {
		t.Fatalf("title=%q, want the content folder name", item.title)
	}
}

func TestTorrentTitle(t *testing.T) {
	cases := []struct {
		t      qbTorrent
		single bool
		want   string
	}{
		{qbTorrent{Name: "rutor.info", ContentPath: "/m/shows/House.S03.2160p"}, false, "House.S03.2160p"},
		{qbTorrent{Name: "rutor.info", ContentPath: "/m/movies/Film.2024.1080p.mkv"}, true, "Film.2024.1080p"},
		{qbTorrent{Name: "Fallback"}, false, "Fallback"},
	}
	for _, c := range cases {
		if got := torrentTitle(&c.t, c.single); got != c.want {
			t.Fatalf("torrentTitle(%+v)=%q, want %q", c.t, got, c.want)
		}
	}
}

// Once no torrent claimed it, the folder of an item added without a category
// was taken for a category again: after the torrent was removed with its
// files kept, or while qBittorrent was unavailable, the item fell apart into
// one item per file, and its id (which must survive removing the torrent)
// disappeared.
func TestRootItemStaysWholeWithoutItsTorrent(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	writeTestFile(t, filepath.Join(root, "Release", "e01.mkv"), size)
	writeTestFile(t, filepath.Join(root, "Release", "e02.mkv"), size)
	// A folder of folders that is not a category stays transparent.
	writeTestFile(t, filepath.Join(root, "Cartoons", "Show", "e01.mkv"), size)

	fake := newFakeQbit(&fakeTorrent{
		hash: "0123456789012345678901234567890123456789", name: "Release",
		savePath: root, state: "stoppedUP",
		files: []fakeFile{
			{name: filepath.Join("Release", "e01.mkv"), size: size, priority: 1, progress: 1},
			{name: filepath.Join("Release", "e02.mkv"), size: size, priority: 1, progress: 1},
		},
	})
	env := newTestEnvWith(t, root, fake)

	titles := func(cat *catalog) map[string]int {
		out := map[string]int{}
		for _, item := range cat.items {
			out[item.title] = len(item.files)
		}
		return out
	}
	before := env.buildCatalog()
	release := ""
	for _, item := range before.items {
		if item.title == "Release" {
			release = item.id
		}
	}
	if release == "" {
		t.Fatalf("items=%v", titles(before))
	}

	fake.mu.Lock()
	fake.torrents = nil
	fake.mu.Unlock()
	removed := env.buildCatalog()
	if got := titles(removed); len(got) != 2 || got["Release"] != 2 || got["Show"] != 1 {
		t.Fatalf("after removing the torrent: items=%v, want Release (2 files) and Show", got)
	}
	if removed.item(release) == nil {
		t.Fatal("the item lost its id when the torrent was removed")
	}

	fake.mu.Lock()
	fake.failInfo = true
	fake.mu.Unlock()
	outage := env.buildCatalog()
	if got := titles(outage); got["Release"] != 2 {
		t.Fatalf("during an outage: items=%v", got)
	}
}
