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
	release, ok := env.streamReg().acquire(inodeKey(info))
	if !ok {
		t.Fatal("stream could not be acquired")
	}
	defer release()

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"?delete_files=true", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("remove status=%d body=%s, want 409", rec.Code, rec.Body)
	}
	if decodeJSON[apiError](t, rec.Body.Bytes()).Code != codeStreaming {
		t.Fatalf("body=%s", rec.Body)
	}

	rec = env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body)
	}
	if decodeJSON[apiError](t, rec.Body.Bytes()).Code != codeStreaming {
		t.Fatalf("body=%s", rec.Body)
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

func TestItemViewedClearFollowsActions(t *testing.T) {
	env := newTestEnv(t)
	cat, item := env.firstItem(t)

	if contains(env.itemActionList(cat, item), actionClearViewed) {
		t.Fatal("clear_viewed offered for an item without marks")
	}
	if rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/viewed", ""); rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", rec.Code)
	}

	base := "/items/" + item.id + "/files/" + item.files[0].id + "/viewed"
	if rec := env.do(t, http.MethodPut, base, `{"state":"watched"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put status=%d", rec.Code)
	}
	cat, item = env.firstItem(t)
	if !contains(env.itemActionList(cat, item), actionClearViewed) {
		t.Fatal("clear_viewed is not offered for an item with marks")
	}
	if rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/viewed", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("clear status=%d", rec.Code)
	}
	_, item = env.firstItem(t)
	if item.files[0].watch != "" {
		t.Fatalf("watch survived the clear: %q", item.files[0].watch)
	}
}

func TestDeleteWatchedKeepsUnwatched(t *testing.T) {
	env := newTestEnv(t)
	cat, item := env.firstItem(t)
	base := "/items/" + item.id + "/files"

	if contains(env.itemActionList(cat, item), actionDeleteWatched) {
		t.Fatal("delete_watched offered for an item without watch marks")
	}
	if rec := env.do(t, http.MethodDelete, base, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bare collection delete status=%d, want 400", rec.Code)
	}
	if rec := env.do(t, http.MethodDelete, base+"?watched=true", ""); rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body)
	}

	mark := func(file, state string) {
		t.Helper()
		rec := env.do(t, http.MethodPut, base+"/"+file+"/viewed", `{"state":"`+state+`"}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("mark %s: status=%d body=%s", state, rec.Code, rec.Body)
		}
	}
	mark(item.files[0].id, watchWatched)
	mark(item.files[1].id, watchStarted)

	cat, item = env.firstItem(t)
	if !contains(env.itemActionList(cat, item), actionDeleteWatched) {
		t.Fatal("delete_watched is not offered for an item with a watched file")
	}

	kept := filepath.Join(env.root, "shows", "Release", "e02.mkv")
	before, err := os.ReadFile(kept)
	if err != nil {
		t.Fatal(err)
	}
	rec := env.do(t, http.MethodDelete, base+"?watched=true", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	if op.State != opDone {
		t.Fatalf("operation failed: %+v", op)
	}

	after, err := os.ReadFile(kept)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the unwatched file was modified")
	}
	calls := strings.Join(env.fake.snapshotCalls(), ",")
	if !strings.Contains(calls, "prio0:0") {
		t.Fatalf("the watched file was not excluded from the torrent: %q", calls)
	}
	if strings.Contains(calls, "prio0:1") {
		t.Fatalf("the unwatched file was excluded too: %q", calls)
	}
}

func TestDeleteWatchedRunsAsOneBatch(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 150 * time.Millisecond
	_, item := env.firstItem(t)
	base := "/items/" + item.id + "/files"

	for _, file := range item.files {
		rec := env.do(t, http.MethodPut, base+"/"+file.id+"/viewed", `{"state":"watched"}`)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("mark %q: status=%d", file.name, rec.Code)
		}
	}
	env.firstItem(t)

	rec := env.do(t, http.MethodDelete, base+"?watched=true", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID); op.State != opDone {
		t.Fatalf("operation failed: %+v", op)
	}

	counts := map[string]int{}
	for _, call := range env.fake.snapshotCalls() {
		counts[call]++
	}
	if counts["prio0:0"] != 1 || counts["prio0:1"] != 1 {
		t.Fatalf("both watched files must be excluded exactly once: %v", counts)
	}
	if counts["recheck"] != 1 {
		t.Fatalf("recheck ran %d times, want one for the whole batch", counts["recheck"])
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
		if id := fileID(value); strings.ContainsAny(id, "/?&# %+") {
			t.Fatalf("file id %q contains characters that need escaping", id)
		}
	}
	for _, bad := range []string{"", "x", "p:", "p:!!!", "q:YWJj", "YWJj"} {
		if _, err := parseID(bad); err == nil {
			t.Fatalf("parseID(%q) accepted a malformed id", bad)
		}
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

func TestDeleteReservesInodeAgainstLateStream(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	target := filepath.Join(env.root, "shows", "Release", "e01.mkv")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	key := inodeKey(info)

	unreserve, free := env.streamReg().reserve(key)
	if !free {
		t.Fatal("could not reserve a quiet inode")
	}
	if _, ok := env.streamReg().acquire(key); ok {
		t.Fatal("stream started on an inode reserved for deletion")
	}
	rec := env.do(t, http.MethodGet,
		"/items/"+item.id+"/files/"+item.files[0].id+"/stream/e01.mkv", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stream of a reserved file: status=%d, want 409", rec.Code)
	}
	unreserve()

	release, ok := env.streamReg().acquire(key)
	if !ok {
		t.Fatal("could not acquire a quiet inode")
	}
	if _, free := env.streamReg().reserve(key); free {
		t.Fatal("delete reserved an inode that is being read")
	}
	release()
	if _, free := env.streamReg().reserve(key); !free {
		t.Fatal("inode stayed reserved after the stream ended")
	}
}

func TestStreamRevalidatesFileAfterAcquire(t *testing.T) {
	env := newTestEnv(t)
	target := filepath.Join(env.root, "shows", "Release", "e01.mkv")
	f, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !complete(before) {
		t.Fatalf("initial file is not complete: info=%+v err=%v", before, err)
	}

	writer, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Truncate(0); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	release, ok := env.streamReg().acquire(inodeKey(before))
	if !ok {
		t.Fatal("completed delete left the inode reserved")
	}
	defer release()
	current, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, current) {
		t.Fatal("test no longer exercises the same inode")
	}
	if validStreamInfo(before, current) {
		t.Fatal("post-acquire validation accepted a file trimmed before acquire")
	}
}

func TestDeleteReservesFileBeforeOperationStarts(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	unreserve, free, err := env.reserveDeleteFile(&item.files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("quiet file could not be reserved for delete")
	}
	defer unreserve()

	target := filepath.Join(env.root, "shows", "Release", "e01.mkv")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env.streamReg().acquire(inodeKey(info)); ok {
		t.Fatal("stream acquired an inode reserved before operation start")
	}
}

func TestBusyItemDoesNotOfferPlay(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)

	op := env.ops().start("test.block", item.id, nil)
	cat := env.buildCatalog()
	env.catalog.Store(cat)
	busy := cat.item(item.id)
	for i := range busy.files {
		if contains(env.fileActionList(cat, busy, &busy.files[i]), actionPlay) {
			t.Fatalf("busy item still offers play for %q", busy.files[i].name)
		}
	}
	env.ops().succeed(op.ID, nil, nil)
}

func TestSeveralDeletesOfOneItemBatchTogether(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 150 * time.Millisecond
	_, item := env.firstItem(t)

	ops := []string{}
	for _, f := range item.files {
		rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+f.id, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delete %q: status=%d body=%s", f.name, rec.Code, rec.Body)
		}
		ops = append(ops, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	}
	for _, id := range ops {
		if op := env.awaitOperation(t, id); op.State != opDone {
			t.Fatalf("operation %s: %+v", id, op)
		}
	}

	calls := env.fake.snapshotCalls()
	counts := map[string]int{}
	for _, c := range calls {
		counts[c]++
	}
	if counts["prio0:0"] != 1 || counts["prio0:1"] != 1 {
		t.Fatalf("both files must be excluded exactly once: %v", calls)
	}
	if counts["recheck"] != 1 {
		t.Fatalf("recheck ran %d times, want one for the whole batch: %v", counts["recheck"], calls)
	}

	for _, name := range []string{"e01.mkv", "e02.mkv"} {
		info, err := os.Stat(filepath.Join(env.root, "shows", "Release", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := env.streamReg().acquire(inodeKey(info)); !ok {
			t.Fatalf("%s is still reserved after the batch finished", name)
		}
	}
}
