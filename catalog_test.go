package main

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

func TestCatalogFileStatuses(t *testing.T) {
	env := newTestEnv(t)
	const size = int64(1 << 20)

	env.fake.mu.Lock()
	env.fake.torrents[0].files[1].priority = 0
	env.fake.torrents[0].files = append(env.fake.torrents[0].files, fakeFile{
		name: filepath.Join("Release", "e03.mkv"), size: size, priority: 1,
	})
	env.fake.mu.Unlock()

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
	sparse.Truncate(size)
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
	if len(detail.Actions) != 0 {
		t.Fatalf("ambiguous item still offers %v", detail.Actions)
	}
	for _, f := range detail.Files {
		if contains(f.Actions, actionDelete) || contains(f.Actions, actionEnable) {
			t.Fatalf("ambiguous file still offers a destructive action: %+v", f)
		}
	}
}

func TestCatalogDetectsSharedFileAcrossDifferentItems(t *testing.T) {
	env := newTestEnv(t)
	const size = int64(1 << 20)
	shared := filepath.Join("Release", "e01.mkv")

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
