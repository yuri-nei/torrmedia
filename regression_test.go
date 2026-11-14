package main

// regression_test.go holds tests for scenarios that once already led to data
// loss or to the UI lying. The contract has been rewritten twice since then,
// but these checks describe BEHAVIOUR, so they move along with it.

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The invariant permissions.go exists for: the list of actions sent to the
// client and the check at request entry are one function. The client cannot
// show a button the server would refuse to execute, and vice versa.
func TestAdvertisedActionsMatchEnforcement(t *testing.T) {
	env := newTestEnv(t)
	// Vary the states: one file is excluded, the other is ready.
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

// Deleting an episode of a live torrent frees space while keeping the
// boundary pieces shared with neighbours, and does not touch the neighbouring
// file.
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

// While an operation is running there is no way to start a second one, and
// this is visible in actions, not only in the response to a request.
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

// A file the player is reading right now must not be zeroed: otherwise the
// player gets zeros under a Content-Length it has already been sent.
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

	// Removing the item together with its data must be refused at once.
	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"?delete_files=true", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("remove status=%d body=%s, want 409", rec.Code, rec.Body)
	}
	if decodeJSON[apiError](t, rec.Body.Bytes()).Code != codeStreaming {
		t.Fatalf("body=%s", rec.Body)
	}

	// Deleting the episode itself is also refused synchronously: the inode is
	// reserved before the operation is created, so an operation that is
	// doomed from the start is never launched.
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

// A file shorter than two pieces must not be zeroed: all of its bytes lie in
// pieces shared with neighbours. There is nothing to free, but plenty to
// corrupt.
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

// Clearing an item's viewed marks goes through the same check as advertising
// it in actions: the handler used to decide by its own condition, and the
// client showed a menu entry the server then refused.
func TestItemViewedClearFollowsActions(t *testing.T) {
	env := newTestEnv(t)
	cat, item := env.firstItem(t)

	// No marks: the action is neither offered nor executed.
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

// Deleting what was watched does not touch what is not finished: that is
// what the action exists for. Also: without the explicit flag the file
// collection is not deleted at all, and without a single mark the action is
// neither offered nor executed.
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

	// e01 is watched to the end, e02 only started: exactly the first one must
	// be deleted.
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

// All watched episodes go in one delete batch: however many there are, the
// torrent is stopped once and rechecked once.
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
	env.firstItem(t) // actions see marks from the same snapshot as the client

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

// An intermediate symlink pointing outside must not lead file operations
// out of the root.
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

// A corrupt viewed.json must not block startup: with Restart=on-failure that
// would be an endless crash loop, and the whole media library would be
// unavailable.
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

// A mark change is applied in memory only together with a successful write.
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

// Multi-range and suffix ranges used to corrupt viewing metadata.
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

// qBittorrent can answer HTTP 200 with the body "Fails."; that is a refusal,
// not success.
func TestPostOKTreatsFailsBodyAsError(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents[0].state = "stoppedUP"
	env.fake.mu.Unlock()
	if err := env.qb.postOK("/api/v2/torrents/does-not-exist", nil); err == nil {
		t.Fatal("404 from qBittorrent was accepted as success")
	}
}

// Identifiers are opaque but must survive any names.
func TestIDRoundTrip(t *testing.T) {
	for _, value := range []string{
		"shows/Игра престолов.S07/эпизод 1.mkv",
		"movies/a b/c+d&e?f#g.mkv",
		"single.mkv",
	} {
		id := pathID(value)
		// ":" is legal in a path segment (RFC 3986, pchar), but everything else
		// would need escaping and would fall apart on a normalizing proxy.
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

// The catalog lives in a cache and is shared by all requests: rendering has
// no right to write to it. Only -race finds this kind of race, so we check
// explicitly.
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

// An expired deadline must not turn into a new wait of several minutes.
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

// P1: seconds pass between the "file is not being read" check and the
// zeroing, so the player managed to start reading and got zeros under the
// Content-Length it had been sent. An inode claim closes the window: while
// a delete is in progress, no stream opens.
func TestDeleteReservesInodeAgainstLateStream(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	target := filepath.Join(env.root, "shows", "Release", "e01.mkv")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	key := inodeKey(info)

	// A delete claim prevents opening for reading.
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

	// And symmetrically: while the file is being read, no claim can be taken.
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

// A stream could pass permitFile and the first complete check before DELETE
// took the inode. If the delete managed to finish entirely and release its
// claim before acquire, the stale metadata still must not be used for
// ServeContent.
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

// DELETE reserves the inode synchronously, before the operation is created
// and the 202 is sent. Otherwise a stream that had already passed permitFile
// could slip in before body started.
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

// While an operation runs on an item, play disappears only for the files it
// deletes. The other ready episodes can still be watched: the operation does
// not touch their data, and there is no reason to wait for the whole
// torrent's recheck to finish because of another episode.
func TestOperationHidesPlayOnlyForDoomedFiles(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	doomed := item.files[0].id

	op := env.ops().start("test.block", item.id, nil, doomed)
	cat := env.buildCatalog()
	env.catalog.Store(cat)
	busy := cat.item(item.id)
	for i := range busy.files {
		f := &busy.files[i]
		offered := contains(env.fileActionList(cat, busy, f), actionPlay)
		if f.id == doomed && offered {
			t.Fatalf("file being deleted still offers play: %q", f.name)
		}
		if f.id != doomed && !offered {
			t.Fatalf("untouched ready file lost play during an operation: %q", f.name)
		}
	}

	// The operation has finished, so the restriction goes away with it.
	env.ops().succeed(op.ID, nil, nil)
	if !contains(env.fileActionList(cat, busy, &busy.files[0]), actionPlay) {
		t.Fatal("play did not come back after the operation finished")
	}
}

// The same through the API, on a real delete: while an episode waits for its
// delete batch, the neighbour plays, but the one being deleted does not,
// neither in actions nor in the stream.
func TestNeighbourPlaysWhileFileIsBeingDeleted(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 300 * time.Millisecond
	_, item := env.firstItem(t)
	victim, neighbour := item.files[0], item.files[1]

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+victim.id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body)
	}
	opID := decodeJSON[operation](t, rec.Body.Bytes()).ID

	detail := decodeJSON[apiItemDetail](t, env.do(t, http.MethodGet, "/items/"+item.id, "").Body.Bytes())
	for _, f := range detail.Files {
		switch f.ID {
		case victim.id:
			if contains(f.Actions, actionPlay) {
				t.Fatalf("file being deleted offers play: %+v", f)
			}
		case neighbour.id:
			if !contains(f.Actions, actionPlay) {
				t.Fatalf("neighbour lost play while another file is deleted: %+v", f)
			}
		}
	}

	stream := func(f catalogFile) int {
		return env.do(t, http.MethodGet, "/items/"+item.id+"/files/"+f.id+"/stream/"+f.name, "").Code
	}
	if code := stream(neighbour); code != http.StatusOK {
		t.Fatalf("neighbour stream during delete: status=%d", code)
	}
	if code := stream(victim); code != http.StatusConflict {
		t.Fatalf("stream of the file being deleted: status=%d, want 409", code)
	}

	if op := env.awaitOperation(t, opID); op.State != opDone {
		t.Fatalf("delete failed: %+v", op)
	}
}

// Batching is the point of the debounce window: several episodes of one item
// need only one stop and one recheck. It was unreachable through the API
// because the second delete was rejected as "item busy", and the whole
// mechanism remained dead code.
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

	// Both episodes are excluded, but there is one stop and one recheck for
	// all of them.
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

	// And the inode claims are released: the files can be opened for reading
	// again.
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

// "Deleted the watched episode, queued the next one for download, went off
// to watch": while the delete was waiting for its batch and running, enable
// used to not be offered at all, so the episode never downloaded. Now it
// queues behind the batch and afterwards starts the torrent that the batch
// left stopped (it was stopped before the batch as well).
func TestEnableQueuesBehindDeleteOfNeighbour(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 150 * time.Millisecond
	env.realisticRecheck(120 * time.Millisecond)
	env.fake.mu.Lock()
	env.fake.torrents[0].files[1].priority = 0 // e02 is excluded
	env.fake.mu.Unlock()
	_, item := env.firstItem(t)
	watched, next := item.files[0], item.files[1]

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+watched.id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body)
	}
	deleteOp := decodeJSON[operation](t, rec.Body.Bytes()).ID

	detail := decodeJSON[apiItemDetail](t, env.do(t, http.MethodGet, "/items/"+item.id, "").Body.Bytes())
	if len(detail.Actions) != 0 {
		t.Fatalf("item actions while deleting: %v", detail.Actions)
	}
	for _, f := range detail.Files {
		if f.ID == next.id && !contains(f.Actions, actionEnable) {
			t.Fatalf("enable is not offered while a neighbour is being deleted: %+v", f)
		}
	}

	rec = env.do(t, http.MethodPost, "/items/"+item.id+"/files/"+next.id+"/enable", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enable: status=%d body=%s", rec.Code, rec.Body)
	}
	enableOp := decodeJSON[operation](t, rec.Body.Bytes()).ID

	for _, id := range []string{deleteOp, enableOp} {
		if op := env.awaitOperation(t, id); op.State != opDone {
			t.Fatalf("operation %s: %+v", id, op)
		}
	}

	// Enable did not slip into the batch: the priority and the start come
	// after the recheck.
	calls := env.fake.snapshotCalls()
	index := func(call string) int {
		for i, c := range calls {
			if c == call {
				return i
			}
		}
		t.Fatalf("no %q in %v", call, calls)
		return -1
	}
	recheck, enable, start := index("recheck"), index("prio1:1"), index("start")
	if !(index("prio0:0") < recheck && recheck < enable && enable < start) {
		t.Fatalf("enable must run after the delete batch and start the torrent: %v", calls)
	}
	if state := env.settledState(t); qbitStoppedState(state) {
		t.Fatalf("torrent left %s: the enabled file would never download", state)
	}
}

// The reverse order: an episode was enabled, the torrent started, and then
// the neighbour was deleted. The batch stops the torrent for the trim and
// must start it again, otherwise the enabled episode would stall halfway.
func TestDeleteAfterEnableKeepsTorrentRunning(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 50 * time.Millisecond
	env.realisticRecheck(120 * time.Millisecond)
	env.fake.mu.Lock()
	env.fake.torrents[0].files[1].priority = 0
	env.fake.mu.Unlock()
	_, item := env.firstItem(t)
	watched, next := item.files[0], item.files[1]

	rec := env.do(t, http.MethodPost, "/items/"+item.id+"/files/"+next.id+"/enable", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enable: status=%d body=%s", rec.Code, rec.Body)
	}
	if op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID); op.State != opDone {
		t.Fatalf("enable: %+v", op)
	}

	env.catalog.Store(env.buildCatalog())
	rec = env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+watched.id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body)
	}
	if op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID); op.State != opDone {
		t.Fatalf("delete: %+v", op)
	}

	calls := env.fake.snapshotCalls()
	if len(calls) == 0 || calls[len(calls)-1] != "start" {
		t.Fatalf("delete batch must end by restarting the running torrent: %v", calls)
	}
	state := env.settledState(t)
	env.fake.mu.Lock()
	prio := env.fake.torrents[0].files[1].priority
	env.fake.mu.Unlock()
	if qbitStoppedState(state) || prio == 0 {
		t.Fatalf("after the batch: state=%s e02 priority=%d", state, prio)
	}
}

// A 2160p season takes minutes to check, longer than the general operation
// timeout. The batch used to give up waiting on timeout ("recheck did not
// finish; torrent left stopped") and never started the torrent. As long as
// the check is progressing, we have to wait.
func TestLongRecheckStillRestartsTorrent(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 20 * time.Millisecond
	env.operationTimeout = 300 * time.Millisecond
	env.realisticRecheck(900 * time.Millisecond)
	env.fake.mu.Lock()
	env.fake.torrents[0].state = "downloading"
	env.fake.mu.Unlock()
	_, item := env.firstItem(t)

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body)
	}
	if op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID); op.State != opDone {
		t.Fatalf("delete: %+v", op)
	}
	if state := env.settledState(t); qbitStoppedState(state) {
		t.Fatalf("torrent left %s after a long recheck", state)
	}
}

// While the batch rechecks the torrent (minutes for a large one), the delete
// operation reports this as a phase instead of sitting in "waiting" until the
// end: otherwise it looks like a hang in the client's header.
func TestDeleteReportsBatchPhases(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 20 * time.Millisecond
	env.realisticRecheck(80 * time.Millisecond)
	env.fake.mu.Lock()
	env.fake.torrents[0].state = "downloading"
	env.fake.mu.Unlock()
	var mu sync.Mutex
	var phases []string
	publish := env.operations.notify
	env.operations.notify = func(op operation) {
		if op.Phase != nil {
			mu.Lock()
			if len(phases) == 0 || phases[len(phases)-1] != *op.Phase {
				phases = append(phases, *op.Phase)
			}
			mu.Unlock()
		}
		publish(op)
	}
	_, item := env.firstItem(t)

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body)
	}
	if op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID); op.State != opDone {
		t.Fatalf("delete: %+v", op)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"waiting", "stopping", "trimming", "rechecking", "restarting"}
	if !slices.Equal(phases, want) {
		t.Fatalf("phases=%v, want %v", phases, want)
	}
}

// A manual recheck of a stopped torrent: the operation must not report
// "done" while qBittorrent is still checking and the API still shows the old
// stoppedUP.
func TestItemRecheckWaitsForRealCheck(t *testing.T) {
	env := newTestEnv(t)
	env.realisticRecheck(150 * time.Millisecond)
	_, item := env.firstItem(t)

	rec := env.do(t, http.MethodPost, "/items/"+item.id+"/recheck", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("recheck: status=%d body=%s", rec.Code, rec.Body)
	}
	if op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID); op.State != opDone {
		t.Fatalf("recheck: %+v", op)
	}
	env.fake.mu.Lock()
	checking := env.fake.torrents[0].checking
	env.fake.mu.Unlock()
	if checking {
		t.Fatal("recheck operation finished while qBittorrent was still checking")
	}
}

// Priority can be changed only for a file that is still downloading: an
// excluded one needs enable, and a ready one has no use for ordering. The
// result is read back from qBittorrent.
func TestFilePriorityOnlyForUnfinishedFiles(t *testing.T) {
	env := newTestEnv(t)
	// e01 is a truncated stub on disk, e02 is excluded.
	abs := filepath.Join(env.root, "shows", "Release", "e01.mkv")
	if err := os.Truncate(abs, 0); err != nil {
		t.Fatal(err)
	}
	env.fake.mu.Lock()
	env.fake.torrents[0].files[0].progress = 0
	env.fake.torrents[0].files[1].priority = 0
	env.fake.mu.Unlock()

	_, item := env.firstItem(t)
	detail := decodeJSON[apiItemDetail](t, env.do(t, http.MethodGet, "/items/"+item.id, "").Body.Bytes())
	byName := map[string]apiFile{}
	for _, f := range detail.Files {
		byName[f.Name] = f
	}
	e01, e02 := byName["e01.mkv"], byName["e02.mkv"]
	if e01.Priority == nil || *e01.Priority != priorityNormal || !contains(e01.Actions, actionSetPriority) {
		t.Fatalf("e01=%+v", e01)
	}
	if e02.Priority != nil || contains(e02.Actions, actionSetPriority) {
		t.Fatalf("skipped file must have neither priority nor set_priority: %+v", e02)
	}

	rec := env.do(t, http.MethodPut, "/items/"+item.id+"/files/"+e02.ID+"/priority", `{"priority":"high"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("skipped: status=%d body=%s", rec.Code, rec.Body)
	}
	rec = env.do(t, http.MethodPut, "/items/"+item.id+"/files/"+e01.ID+"/priority", `{"priority":"urgent"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown priority: status=%d body=%s", rec.Code, rec.Body)
	}
	rec = env.do(t, http.MethodPut, "/items/"+item.id+"/files/"+e01.ID+"/priority", `{"priority":"max"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	op := decodeJSON[operation](t, rec.Body.Bytes())
	if op.Kind != "file.priority" {
		t.Fatalf("operation=%+v", op)
	}
	if done := env.awaitOperation(t, op.ID); done.State != opDone {
		t.Fatalf("operation failed: %+v", done)
	}
	env.fake.mu.Lock()
	got := env.fake.torrents[0].files[0].priority
	env.fake.mu.Unlock()
	if got != 7 {
		t.Fatalf("qbit priority=%d, want 7", got)
	}
	_, item = env.firstItem(t)
	detail = decodeJSON[apiItemDetail](t, env.do(t, http.MethodGet, "/items/"+item.id, "").Body.Bytes())
	for _, f := range detail.Files {
		if f.Name == "e01.mkv" && (f.Priority == nil || *f.Priority != priorityMax) {
			t.Fatalf("e01 after set=%+v", f)
		}
		if f.Name == "e02.mkv" && f.Status == fileReady && contains(f.Actions, actionSetPriority) {
			t.Fatalf("ready file must not offer set_priority: %+v", f)
		}
	}
}

func TestPriorityNames(t *testing.T) {
	for raw, want := range map[int]string{0: "", 1: priorityNormal, 4: priorityNormal, 6: priorityHigh, 7: priorityMax} {
		if got := priorityName(raw); got != want {
			t.Fatalf("priorityName(%d)=%q, want %q", raw, got, want)
		}
	}
	for _, name := range []string{priorityNormal, priorityHigh, priorityMax} {
		v, ok := priorityValue(name)
		if !ok || priorityName(v) != name {
			t.Fatalf("round trip %q -> %d", name, v)
		}
	}
}

// qBittorrent splits the "urls" field of torrents/add by line breaks, so a
// magnet with a newline inside used to smuggle an arbitrary http(s) URL past
// the magnet-only check and make qBittorrent fetch it.
func TestMagnetWithLineBreakIsRejected(t *testing.T) {
	env := draftEnv(t)
	smuggled := `"magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01\nhttp://169.254.169.254/x.torrent"`
	for _, path := range []string{"/items", "/drafts"} {
		rec := env.do(t, http.MethodPost, path, `{"magnet":`+smuggled+`}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("POST %s: status=%d, want 403 (%s)", path, rec.Code, rec.Body)
		}
	}
	for _, call := range env.fake.snapshotCalls() {
		if strings.HasPrefix(call, "add:") {
			t.Fatalf("qBittorrent was asked to add something: %v", env.fake.snapshotCalls())
		}
	}
}

// fileActionsNow rebuilds the catalog from the fake and returns the actions
// offered for every file of the first item, by file name.
func (e *testEnv) fileActionsNow(t *testing.T) map[string][]string {
	t.Helper()
	cat := e.buildCatalog()
	e.catalog.Store(cat)
	if len(cat.items) == 0 {
		t.Fatal("catalog is empty")
	}
	item := cat.items[0]
	out := map[string][]string{}
	for i := range item.files {
		out[item.files[i].name] = e.fileActionList(cat, item, &item.files[i])
	}
	return out
}

// A check that qBittorrent runs on its own used to leave delete and enable
// on offer, and both operations then always failed with "being checked".
func TestExternalCheckHidesDeleteAndEnable(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents[0].files[1].priority = 0
	env.fake.mu.Unlock()
	setState := func(state string) {
		env.fake.mu.Lock()
		env.fake.torrents[0].state = state
		env.fake.mu.Unlock()
	}
	for _, state := range []string{"checkingUP", "checkingResumeData", "moving"} {
		setState(state)
		actions := env.fileActionsNow(t)
		if contains(actions["e01.mkv"], actionDelete) || contains(actions["e02.mkv"], actionEnable) {
			t.Fatalf("%s: delete/enable offered during qBittorrent's own check: %v", state, actions)
		}
	}
	setState("stoppedUP")
	actions := env.fileActionsNow(t)
	if !contains(actions["e01.mkv"], actionDelete) || !contains(actions["e02.mkv"], actionEnable) {
		t.Fatalf("delete/enable did not come back after the check: %v", actions)
	}
}

// While one of our own operations runs, the check is ours and a delete
// queues behind it, so it stays on offer: that is how episodes get batched.
func TestDeleteStaysOfferedDuringOwnRecheck(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	op := env.ops().start(opFileDelete, item.id, &item.files[1].id, item.files[1].id)
	defer env.ops().succeed(op.ID, nil, nil)
	env.fake.mu.Lock()
	env.fake.torrents[0].state = "checkingUP"
	env.fake.mu.Unlock()
	if actions := env.fileActionsNow(t); !contains(actions["e01.mkv"], actionDelete) {
		t.Fatalf("delete is not offered while our own batch rechecks: %v", actions)
	}
}

// A delete queued during the removal of the item could only fail: by the
// time the batch ran, the torrent was gone.
func TestDeleteNotOfferedWhileItemIsRemoved(t *testing.T) {
	for _, kind := range []string{opItemRemove, opItemRemoveWithData} {
		env := newTestEnv(t)
		_, item := env.firstItem(t)
		op := env.ops().start(kind, item.id, nil)
		if actions := env.fileActionsNow(t); contains(actions["e01.mkv"], actionDelete) {
			t.Fatalf("%s: delete offered while the item is being removed: %v", kind, actions)
		}
		env.ops().succeed(op.ID, nil, nil)
		if actions := env.fileActionsNow(t); !contains(actions["e01.mkv"], actionDelete) {
			t.Fatalf("%s: delete did not come back: %v", kind, actions)
		}
	}
}

// A torrent saved outside the media root can be complete in qBittorrent, but
// there is nothing to stream: play used to be offered and the stream gave 404.
func TestNoPlayForFilesOutsideRoot(t *testing.T) {
	env := newTestEnv(t)
	outside := t.TempDir()
	for _, name := range []string{"e01.mkv", "e02.mkv"} {
		path := filepath.Join(outside, "Release", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env.fake.mu.Lock()
	env.fake.torrents[0].savePath = outside
	env.fake.mu.Unlock()
	cat := env.buildCatalog()
	env.catalog.Store(cat)
	for _, item := range cat.items {
		for i := range item.files {
			f := &item.files[i]
			if f.shelfRel == "" && contains(env.fileActionList(cat, item, f), actionPlay) {
				t.Fatalf("play offered for %q outside the root", f.name)
			}
		}
	}
}

// libtorrent checks one torrent at a time; a queued check sits at 0% while
// another one runs. The wait used to count that as a stall: with two big
// seasons trimmed at once, the second batch failed after the operation
// timeout and left its torrent stopped.
func TestRecheckWaitingInQueueIsNotAStall(t *testing.T) {
	env := newTestEnv(t)
	first := env.fake.torrents[0]
	second := &fakeTorrent{
		hash: "abcdefabcdefabcdefabcdefabcdefabcdefabcd", name: "Other",
		savePath: filepath.Join(env.root, "shows"), state: "stoppedUP",
		files: []fakeFile{{name: filepath.Join("Other", "e01.mkv"), size: 1 << 20, priority: 1, progress: 1}},
	}
	env.fake.mu.Lock()
	env.fake.torrents = append(env.fake.torrents, second)
	env.fake.serialChecks = true
	env.fake.recheckIn = 300 * time.Millisecond
	env.fake.mu.Unlock()

	if err := env.qb.recheck(first.hash); err != nil {
		t.Fatal(err)
	}
	if err := env.qb.recheck(second.hash); err != nil {
		t.Fatal(err)
	}
	// The stall limit is shorter than the first check, during which the
	// second one does not move at all.
	got, err := env.qb.waitChecked(second.hash, 150*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("queued check treated as stalled: %v", err)
	}
	if qbitCheckingState(got.State) {
		t.Fatalf("returned while still checking: %s", got.State)
	}
}

// A piece size the trim refuses used to be found only inside the trim, after
// the torrent was stopped and the priority dropped: the file ended up
// excluded with all its data still on disk, and an excluded file is never
// offered for delete again.
func TestUnsafePieceSizeChangesNothing(t *testing.T) {
	env := newTestEnv(t)
	env.deleteDebounce = 20 * time.Millisecond
	env.fake.mu.Lock()
	env.fake.pieceSize = 128 << 20
	env.fake.torrents[0].state = "uploading"
	env.fake.mu.Unlock()
	_, item := env.firstItem(t)

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body)
	}
	op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	if op.State != opFailed || op.Error == nil || !strings.Contains(*op.Error, "piece size") {
		t.Fatalf("operation: %+v", op)
	}
	for _, call := range env.fake.snapshotCalls() {
		if call == "stop" || strings.HasPrefix(call, "prio") || call == "recheck" {
			t.Fatalf("qBittorrent was touched: %v", env.fake.snapshotCalls())
		}
	}
	_, after := env.firstItem(t)
	if after.files[0].status != fileReady {
		t.Fatalf("file status after the refused delete: %s", after.files[0].status)
	}
}

// qBittorrent reports a torrent with a file error as error or missingFiles
// even after it is stopped. That is exactly the full-disk case in which the
// user deletes episodes to free space, and the batch used to wait for a
// stopped state that never came, fail after the operation timeout and
// restart the torrent with nothing deleted.
func TestDeleteFromErroredTorrent(t *testing.T) {
	for _, errorState := range []string{"error", "missingFiles"} {
		t.Run(errorState, func(t *testing.T) {
			env := newTestEnv(t)
			env.deleteDebounce = 20 * time.Millisecond
			env.operationTimeout = 2 * time.Second
			env.qb.stateLag = 50 * time.Millisecond
			env.fake.mu.Lock()
			env.fake.torrents[0].state = "downloading"
			env.fake.torrents[0].errorState = errorState
			env.fake.mu.Unlock()
			_, item := env.firstItem(t)
			if !contains(env.fileActionList(env.catalog.Load(), item, &item.files[0]), actionDelete) {
				t.Fatal("delete is not offered for an errored torrent")
			}

			rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+item.files[0].id, "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body)
			}
			op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
			if op.State != opDone {
				t.Fatalf("delete: %+v", op)
			}
			calls := env.fake.snapshotCalls()
			if len(calls) == 0 || calls[len(calls)-1] != "start" {
				t.Fatalf("the torrent was not restarted after the batch: %v", calls)
			}
		})
	}
}

// Removing an item with no torrent deletes its files one by one. When none
// of them could be deleted, the operation still reported done and the client
// showed the item as removed.
func TestShelfRemoveFailsWhenNothingWasDeleted(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents = nil
	env.fake.mu.Unlock()
	_, item := env.firstItem(t)
	if item.torrentBacked() {
		t.Fatal("expected a shelf item")
	}
	dir := filepath.Join(env.root, "shows", "Release")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"?delete_files=true", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("remove: status=%d body=%s", rec.Code, rec.Body)
	}
	op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	if op.State != opFailed {
		t.Fatalf("nothing was deleted, yet the operation is %s: %+v", op.State, op)
	}
	if _, err := os.Stat(filepath.Join(dir, "e01.mkv")); err != nil {
		t.Fatalf("file should still be there: %v", err)
	}
}

// Operation results and messages are part of the API, and the API promises
// that infohashes, qBittorrent file ids and disk paths never leave the
// server. A delete batch used to return "torrent": <infohash> and the file's
// path in the library, and operating system errors carried absolute paths.
func TestOperationsDoNotLeakInternals(t *testing.T) {
	root := testRoot(t)
	writeTestFile(t, filepath.Join(root, "shows", "Release", "small.mkv"), 100<<10)
	writeTestFile(t, filepath.Join(root, "shows", "Release", "big.mkv"), 4<<20)
	const hash = "0123456789012345678901234567890123456789"
	fake := newFakeQbit(&fakeTorrent{
		hash: hash, name: "Release", savePath: filepath.Join(root, "shows"),
		category: "Shows", state: "stoppedUP",
		files: []fakeFile{
			{name: filepath.Join("Release", "small.mkv"), size: 100 << 10, priority: 1, progress: 1},
			{name: filepath.Join("Release", "big.mkv"), size: 4 << 20, priority: 1, progress: 1},
		},
	})
	env := newTestEnvWith(t, root, fake)
	env.deleteDebounce = 150 * time.Millisecond
	_, item := env.firstItem(t)

	ops := map[string]string{}
	for _, f := range item.files {
		rec := env.do(t, http.MethodDelete, "/items/"+item.id+"/files/"+f.id, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delete %s: status=%d body=%s", f.name, rec.Code, rec.Body)
		}
		ops[f.name] = decodeJSON[operation](t, rec.Body.Bytes()).ID
	}
	for name, id := range ops {
		if op := env.awaitOperation(t, id); op.State != opDone {
			t.Fatalf("%s: %+v", name, op)
		}
		body := env.do(t, http.MethodGet, "/operations/"+id, "").Body.String()
		for _, leak := range []string{hash, root, "shows/Release", `"torrent"`} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s: operation leaks %q: %s", name, leak, body)
			}
		}
		op := decodeJSON[operation](t, []byte(body))
		kept := false
		for _, w := range op.Warnings {
			kept = kept || strings.Contains(w, "data kept")
		}
		// The small file is shorter than two pieces: its data stays on disk,
		// and only its own operation says so.
		if kept != (name == "small.mkv") {
			t.Fatalf("%s: warnings=%v", name, op.Warnings)
		}
	}
}

func TestShelfRemoveWarningsHaveNoAbsolutePaths(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents = nil
	env.fake.mu.Unlock()
	_, item := env.firstItem(t)
	// One file can be deleted, the other cannot: a partial removal is done
	// with a warning.
	locked := filepath.Join(env.root, "shows", "Release", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(locked, "e03.mkv"), 1<<20)
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	_, item = env.firstItem(t)

	rec := env.do(t, http.MethodDelete, "/items/"+item.id+"?delete_files=true", "")
	op := env.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
	if len(op.Warnings) == 0 {
		t.Fatalf("expected a warning for the locked file: %+v", op)
	}
	for _, w := range op.Warnings {
		if strings.Contains(w, env.root) {
			t.Fatalf("warning carries an absolute path: %q", w)
		}
	}
}
