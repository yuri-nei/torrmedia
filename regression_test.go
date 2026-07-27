package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdvertisedActionsMatchEnforcement(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents[0].files[1].priority = 0
	env.fake.mu.Unlock()

	cat := env.buildCatalog()
	env.catalog.Store(cat)
	for _, item := range cat.items {
		advertised := map[string]bool{}
		for _, a := range env.itemActionList(cat, item) {
			advertised[a] = true
		}
		for _, action := range itemActionOrder {
			denied := env.permitItem(cat, item, action) != nil
			if advertised[action] == denied {
				t.Fatalf("item %q action %q: advertised=%v denied=%v",
					item.title, action, advertised[action], denied)
			}
		}
		for i := range item.files {
			file := &item.files[i]
			shown := map[string]bool{}
			for _, a := range env.fileActionList(cat, item, file) {
				shown[a] = true
			}
			for _, action := range fileActionOrder {
				denied := env.permitFile(cat, item, file, action) != nil
				if shown[action] == denied {
					t.Fatalf("file %q action %q: advertised=%v denied=%v",
						file.name, action, shown[action], denied)
				}
			}
		}
	}
}

func TestDeleteFileFreesSpaceAndKeepsNeighbour(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	neighbour := filepath.Join(env.root, "shows", "Release", "e02.mkv")
	before, err := os.ReadFile(neighbour)
	if err != nil {
		t.Fatal(err)
	}

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	if op.State != opDone {
		t.Fatalf("operation failed: %+v", op)
	}

	after, err := os.ReadFile(neighbour)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("neighbouring file was modified")
	}
	if calls := strings.Join(env.fake.snapshotCalls(), ","); !strings.Contains(calls, "prio0:0") {
		t.Fatalf("file was not excluded from the torrent: %q", calls)
	}
}

func TestBusyItemOffersNoActions(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)

	release := make(chan struct{})
	op := env.ops().start("test.block", item.id, nil)
	go func() { <-release; env.ops().succeed(op.ID, nil, nil) }()

	cat := env.buildCatalog()
	if actions := env.itemActionList(cat, cat.item(item.id)); len(actions) != 0 {
		t.Fatalf("busy item offers %v", actions)
	}
	rec := env.do(t, http.MethodPost, "/items/"+item.id+"/stop", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body)
	}
	if decodeJSON[apiError](t, rec.Body.Bytes()).Code != codeBusy {
		t.Fatalf("body=%s", rec.Body)
	}
	close(release)
}

func TestActiveStreamBlocksDelete(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	target := filepath.Join(env.root, "shows", "Release", "e01.mkv")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	defer env.streamReg().acquire(inodeKey(info))()

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"?delete_files=true", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("remove status=%d body=%s, want 409", rec.Code, rec.Body)
	}
	if decodeJSON[apiError](t, rec.Body.Bytes()).Code != codeStreaming {
		t.Fatalf("body=%s", rec.Body)
	}

	rec = env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	if op.State != opFailed {
		t.Fatalf("delete of a streamed file succeeded: %+v", op)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("streamed file was trimmed")
	}
}

func TestSmallFileKeepsDataInsteadOfDestroyingSharedPieces(t *testing.T) {
	root := testRoot(t)
	const pieceSize = int64(64 << 10)
	size := pieceSize + 1024
	writeTestFile(t, filepath.Join(root, "shows", "Release", "sample.mkv"), size)
	fake := newFakeQbit(&fakeTorrent{
		hash: "0123456789012345678901234567890123456789", name: "Release",
		savePath: filepath.Join(root, "shows"), state: "stoppedUP",
		files: []fakeFile{{name: filepath.Join("Release", "sample.mkv"), size: size, priority: 1, progress: 1}},
	})
	env := newTestEnvWith(t, root, fake)
	_, item := env.firstItem(t)
	original, err := os.ReadFile(filepath.Join(root, "shows", "Release", "sample.mkv"))
	if err != nil {
		t.Fatal(err)
	}

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	if op.State != opDone {
		t.Fatalf("operation failed: %+v", op)
	}
	if len(op.Warnings) == 0 {
		t.Fatalf("kept data was not reported as a warning: %+v", op)
	}
	after, err := os.ReadFile(filepath.Join(root, "shows", "Release", "sample.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("small file was zeroed; boundary pieces of neighbours are now broken")
	}
}

func TestEnableCancelsQueuedDelete(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	writeTestFile(t, filepath.Join(root, "shows", "Release", "e01.mkv"), size)
	fake := newFakeQbit(&fakeTorrent{
		hash: "0123456789012345678901234567890123456789", name: "Release",
		savePath: filepath.Join(root, "shows"), state: "stoppedUP",
		files: []fakeFile{{name: filepath.Join("Release", "e01.mkv"), size: size, priority: 1, progress: 1}},
	})
	env := newTestEnvWith(t, root, fake)
	env.deleteDebounce = 400 * time.Millisecond
	_, item := env.firstItem(t)
	original, err := os.ReadFile(filepath.Join(root, "shows", "Release", "e01.mkv"))
	if err != nil {
		t.Fatal(err)
	}

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	deleteOp := decodeJSON[operation](t, rec.Body.Bytes()).ID
	waitForDeleteBatchSize(t, env.server, fake.torrents[0].hash, 1)

	if !env.cancelQueuedDelete(fake.torrents[0].hash, 0) {
		t.Fatal("queued delete was not cancelled")
	}
	op := env.awaitOperation(t, deleteOp)
	if op.State != opFailed {
		t.Fatalf("cancelled delete reported %s: %+v", op.State, op)
	}

	time.Sleep(600 * time.Millisecond)
	after, err := os.ReadFile(filepath.Join(root, "shows", "Release", "e01.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("file was trimmed after the delete had been cancelled")
	}
}

func waitForDeleteBatchSize(t *testing.T, s *server, hash string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.deleteMu.Lock()
		got := 0
		if batch := s.deleteBatches[hash]; batch != nil {
			got = len(batch.targets)
		}
		s.deleteMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued delete batch size=%d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOpenInRootRefusesSymlinkEscape(t *testing.T) {
	root := testRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.mkv"), []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "shows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "shows", "escape")); err != nil {
		t.Fatal(err)
	}
	s := &server{root: root}
	if f, err := s.openInRoot("shows/escape/secret.mkv"); err == nil {
		f.Close()
		t.Fatal("os.Root allowed a symlink escape out of the media root")
	}
}

func TestViewedStoreFailsOpenOnCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "viewed.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := newViewedStore(path)
	if err != nil {
		t.Fatalf("corrupt viewed.json still blocks startup: %v", err)
	}
	if len(store.snapshot()) != 0 {
		t.Fatal("corrupt store was not reset")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt file was not preserved for inspection: %v", matches)
	}
}

func TestViewedUpdateRevertsWhenSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	store, err := newViewedStore(filepath.Join(dir, "viewed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = store.update(func(items map[string]viewedEntry) {
		items["shows/x.mkv"] = viewedEntry{Path: "shows/x.mkv", State: watchWatched}
	})
	if err == nil {
		t.Fatal("save into a read-only directory reported success")
	}
	if len(store.snapshot()) != 0 {
		t.Fatal("in-memory state kept a change that was never written to disk")
	}
}

func TestTrackableRange(t *testing.T) {
	for header, want := range map[string]bool{
		"":                   true,
		"bytes=0-":           true,
		"bytes=1024-2047":    true,
		"bytes=-1024":        false,
		"bytes=0-99,200-299": false,
		"items=0-10":         false,
	} {
		if got := trackableRange(header); got != want {
			t.Fatalf("trackableRange(%q)=%v, want %v", header, got, want)
		}
	}
}

func TestRedactQueryHidesSecrets(t *testing.T) {
	got := redactQuery(map[string][]string{
		"magnet":       {"magnet:?xt=urn:btih:abc&tr=http://tracker/PASSKEY/announce"},
		"delete_files": {"true"},
	})
	if strings.Contains(got, "PASSKEY") || strings.Contains(got, "magnet:?") {
		t.Fatalf("secret leaked into the log line: %s", got)
	}
	if !strings.Contains(got, "delete_files=true") {
		t.Fatalf("useful parameters were lost: %s", got)
	}
}

func TestPostOKTreatsFailsBodyAsError(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents[0].state = "stoppedUP"
	env.fake.mu.Unlock()
	if err := env.qb.postOK("/api/v2/torrents/does-not-exist", nil); err == nil {
		t.Fatal("404 from qBittorrent was accepted as success")
	}
}

func TestIDRoundTrip(t *testing.T) {
	for _, value := range []string{
		"shows/Игра престолов.S07/эпизод 1.mkv",
		"movies/a b/c+d&e?f#g.mkv",
		"single.mkv",
	} {
		id := pathID(value)
		if strings.ContainsAny(id, "/?&# %+") {
			t.Fatalf("id %q contains characters that need escaping", id)
		}
		parsed, err := parseID(id)
		if err != nil || parsed.kind != idPath || parsed.value != value {
			t.Fatalf("parseID(%q)=%+v, %v", id, parsed, err)
		}
		back, err := parseFileID(fileID(value))
		if err != nil || back != value {
			t.Fatalf("file id round trip for %q: %q, %v", value, back, err)
		}
	}
	for _, bad := range []string{"", "x", "p:", "p:!!!", "q:YWJj", "YWJj"} {
		if _, err := parseID(bad); err == nil {
			t.Fatalf("parseID(%q) accepted a malformed id", bad)
		}
	}
	if _, err := parseFileID(fileID("../escape")); err == nil {
		t.Fatal("file id escaping the item was accepted")
	}
}

func TestRenderDoesNotMutateCatalog(t *testing.T) {
	env := newTestEnv(t)
	cat, item := env.firstItem(t)
	before := make([]string, len(item.files))
	for i, f := range item.files {
		before[i] = f.status
	}

	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				env.renderDetail(cat, item)
				env.renderList(cat)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	for i, f := range item.files {
		if f.status != before[i] {
			t.Fatalf("render changed catalog: file %q %q -> %q", f.name, before[i], f.status)
		}
	}
}

func TestPollUntilRefusesExpiredDeadline(t *testing.T) {
	start := time.Now()
	err := pollUntil(0, time.Millisecond, func() (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("expired deadline was accepted")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s on an expired deadline", elapsed)
	}
}
