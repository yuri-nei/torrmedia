package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newBatchServer(t *testing.T, root string, fake *batchFakeQbit, debounce time.Duration) *server {
	t.Helper()
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		root: root, qb: qb, deleteDebounce: debounce,
		operationTimeout: 2 * time.Second, pollInterval: 5 * time.Millisecond,
		enableReserve:  512 << 20,
		availableBytes: func(string) (int64, error) { return 100 << 30, nil },
	}
}

func oneFileFake(root string, rel string, size int64, priority int) *batchFakeQbit {
	return &batchFakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: filepath.Join(root, "shows"),
		pieceSize: 64 << 10, state: "stoppedUP",
		files: []batchFakeFile{
			{name: rel, size: size, priority: priority, progress: 1},
		},
	}
}

func TestServiceMutationsRejectedWhileDeleteBatchIsQueued(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	writeTestFile(t, filepath.Join(root, rel), size)

	fake := oneFileFake(root, filepath.Join("release", "episode.mkv"), size, 1)
	s := newBatchServer(t, root, fake, 2*time.Second)

	go func() {
		req := httptest.NewRequest(http.MethodDelete, "/file?path="+url.QueryEscape(rel), nil)
		s.handleDelete(httptest.NewRecorder(), req)
	}()
	waitForDeleteBatchSize(t, s, fake.hash, 1)

	for name, handler := range map[string]http.HandlerFunc{
		"remove":  s.handleRemove,
		"recheck": s.handleRecheck,
		"start":   s.handleStartStop("start"),
		"stop":    s.handleStartStop("stop"),
	} {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, "/x?hash="+fake.hash, nil))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s during a queued batch: status=%d body=%s, want 409",
				name, recorder.Code, recorder.Body.String())
		}
	}
	for _, call := range fake.snapshotCalls() {
		if call == "DELETE_TORRENT" || call == "recheck" || call == "start" {
			t.Fatalf("service endpoint mutated the torrent during a queued batch: %v",
				fake.snapshotCalls())
		}
	}
}

func TestEnableCancelsQueuedDeleteInsteadOfLosingData(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	original := writeTestFile(t, filepath.Join(root, rel), size)

	fake := oneFileFake(root, filepath.Join("release", "episode.mkv"), size, 1)
	s := newBatchServer(t, root, fake, 400*time.Millisecond)

	deleteRecorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodDelete, "/file?path="+url.QueryEscape(rel), nil)
		s.handleDelete(deleteRecorder, req)
		close(done)
	}()
	waitForDeleteBatchSize(t, s, fake.hash, 1)

	enableRecorder := httptest.NewRecorder()
	s.handleEnable(enableRecorder, httptest.NewRequest(http.MethodPost,
		"/file/enable?hash="+fake.hash+"&id=0", nil))
	if enableRecorder.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", enableRecorder.Code, enableRecorder.Body.String())
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled delete never answered its client")
	}
	if deleteRecorder.Code != http.StatusConflict {
		t.Fatalf("cancelled delete status=%d body=%s, want 409",
			deleteRecorder.Code, deleteRecorder.Body.String())
	}

	time.Sleep(600 * time.Millisecond)
	after, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("file was trimmed after the delete had been cancelled by enable")
	}
}

func TestActiveStreamBlocksTrim(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	original := writeTestFile(t, filepath.Join(root, rel), size)

	fake := oneFileFake(root, filepath.Join("release", "episode.mkv"), size, 1)
	s := newBatchServer(t, root, fake, time.Millisecond)

	info, err := os.Stat(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	release := s.streamReg().acquire(inodeKey(info))

	recorder := httptest.NewRecorder()
	s.handleDelete(recorder, httptest.NewRequest(http.MethodDelete,
		"/file?path="+url.QueryEscape(rel), nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("delete during playback status=%d body=%s, want 409",
			recorder.Code, recorder.Body.String())
	}
	after, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("streamed file was trimmed")
	}
	release()
}

func TestSmallFileKeepsDataInsteadOfDestroyingSharedPieces(t *testing.T) {
	root := testRoot(t)
	const pieceSize = int64(64 << 10)
	size := pieceSize + 1024
	rel := filepath.Join("shows", "release", "sample.mkv")
	original := writeTestFile(t, filepath.Join(root, rel), size)

	fake := oneFileFake(root, filepath.Join("release", "sample.mkv"), size, 1)
	s := newBatchServer(t, root, fake, time.Millisecond)

	recorder := httptest.NewRecorder()
	s.handleDelete(recorder, httptest.NewRequest(http.MethodDelete,
		"/file?path="+url.QueryEscape(rel), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	warnings, _ := response["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("kept data was not reported as a warning: %v", response)
	}
	after, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("small file was zeroed; boundary pieces of neighbours are now broken")
	}
	if calls := strings.Join(fake.snapshotCalls(), ","); !strings.Contains(calls, "prio0:0") {
		t.Fatalf("file was not excluded from the torrent: %q", calls)
	}
}

func TestListExposesExactOwnership(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	writeTestFile(t, filepath.Join(root, rel), size)

	fake := oneFileFake(root, filepath.Join("release", "episode.mkv"), size, 1)
	s := newBatchServer(t, root, fake, time.Millisecond)

	recorder := httptest.NewRecorder()
	s.handleList(recorder, httptest.NewRequest(http.MethodGet, "/list", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items         []item `json:"items"`
		QbitAvailable bool   `json:"qbit_available"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.QbitAvailable {
		t.Fatal("qbit_available=false with a healthy fake")
	}
	if len(response.Items) != 1 || len(response.Items[0].Files) != 1 {
		t.Fatalf("unexpected shelf shape: %+v", response.Items)
	}
	if got := response.Items[0].TorrentHash; got != fake.hash {
		t.Fatalf("item torrent_hash=%q, want %q", got, fake.hash)
	}
	file := response.Items[0].Files[0]
	if file.TorrentHash != fake.hash || file.QbitFileID != 0 || file.Owner != ownerTorrent {
		t.Fatalf("file identity=(%q,%q,%d), want (%q,%q,0)",
			file.Owner, file.TorrentHash, file.QbitFileID, ownerTorrent, fake.hash)
	}
}

func TestListDistinguishesUnownedFileFromAmbiguousOne(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	owned := filepath.Join("shows", "release", "episode.mkv")
	loose := filepath.Join("movies", "home-video.mkv")
	writeTestFile(t, filepath.Join(root, owned), size)
	writeTestFile(t, filepath.Join(root, loose), size)

	fake := oneFileFake(root, filepath.Join("release", "episode.mkv"), size, 1)
	s := newBatchServer(t, root, fake, time.Millisecond)

	recorder := httptest.NewRecorder()
	s.handleList(recorder, httptest.NewRequest(http.MethodGet, "/list", nil))
	var response struct {
		Items []item `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for _, it := range response.Items {
		for _, f := range it.Files {
			owners[f.Path] = f.Owner
		}
	}
	if owners[owned] != ownerTorrent {
		t.Fatalf("owned file owner=%q, want %q", owners[owned], ownerTorrent)
	}
	if owners[loose] != ownerNone {
		t.Fatalf("plain file owner=%q, want %q — the client would refuse to delete it",
			owners[loose], ownerNone)
	}
}

func TestListReportsQbitUnavailable(t *testing.T) {
	root := testRoot(t)
	writeTestFile(t, filepath.Join(root, "shows", "release", "episode.mkv"), 1<<20)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	dead.Close()
	qb, err := newQbit(dead.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{root: root, qb: qb}

	recorder := httptest.NewRecorder()
	s.handleList(recorder, httptest.NewRequest(http.MethodGet, "/list", nil))
	var response struct {
		QbitAvailable bool `json:"qbit_available"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.QbitAvailable {
		t.Fatal("qbit_available=true while qBittorrent is unreachable")
	}
}

func TestWaitRecheckAcceptsRecheckThatFinishedBeforeFirstPoll(t *testing.T) {
	fake := &batchFakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: "/tmp", pieceSize: 64 << 10,
		state: "stoppedDL",
		files: []batchFakeFile{{name: "release/episode.mkv", size: 1 << 20, priority: 0}},
	}
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := qb.waitRecheck(fake.hash, 0, 2*time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("waitRecheck: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitRecheck waited %s for an already finished recheck", elapsed)
	}
}

func TestPostOKTreatsFailsBodyAsError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "Fails.")
	}))
	defer upstream.Close()
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := qb.postOK("/api/v2/torrents/add", url.Values{"urls": {"magnet:?xt=urn:btih:x"}}); err == nil {
		t.Fatal("HTTP 200 with a Fails body was accepted as success")
	}
}

func TestListFailsLoudlyWhenRootIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := testRoot(t)
	blocked := filepath.Join(root, "shows")
	writeTestFile(t, filepath.Join(blocked, "release", "episode.mkv"), 1024)
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	s := &server{root: root}
	recorder := httptest.NewRecorder()
	s.handleList(recorder, httptest.NewRequest(http.MethodGet, "/list", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", recorder.Code, recorder.Body.String())
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
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
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
		items["shows/x.mkv"] = viewedEntry{Path: "shows/x.mkv", State: "watched"}
	})
	if err == nil {
		t.Fatal("save into a read-only directory reported success")
	}
	if len(store.snapshot()) != 0 {
		t.Fatal("in-memory state kept a change that was never written to disk")
	}
}

func TestOpenInRootRefusesSymlinkEscape(t *testing.T) {
	root := testRoot(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.mkv")
	if err := os.WriteFile(secret, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "shows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "shows", "escape")); err != nil {
		t.Fatal(err)
	}

	s := &server{root: root}
	if f, err := s.openInRoot(filepath.Join("shows", "escape", "secret.mkv")); err == nil {
		f.Close()
		t.Fatal("os.Root allowed a symlink escape out of the media root")
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
	got := redactQuery(url.Values{
		"magnet":   {"magnet:?xt=urn:btih:abc&tr=http://tracker/PASSKEY/announce"},
		"category": {"Shows"},
	})
	if strings.Contains(got, "PASSKEY") || strings.Contains(got, "magnet:?") {
		t.Fatalf("secret leaked into the log line: %s", got)
	}
	if !strings.Contains(got, "category=Shows") {
		t.Fatalf("useful parameters were lost: %s", got)
	}
}

func TestAddRejectsSavepathOutsideRoot(t *testing.T) {
	root := testRoot(t)
	fake := oneFileFake(root, filepath.Join("release", "episode.mkv"), 1<<20, 1)
	s := newBatchServer(t, root, fake, time.Millisecond)

	recorder := httptest.NewRecorder()
	s.handleAdd(recorder, httptest.NewRequest(http.MethodPost,
		"/add?magnet=magnet:?xt=urn:btih:abc&savepath=/etc", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("savepath outside the root: status=%d body=%s, want 400",
			recorder.Code, recorder.Body.String())
	}
}

func TestTorrentNotFoundIsDistinctFromTransportError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "[]")
	}))
	defer upstream.Close()
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qb.torrent("deadbeef"); !errors.Is(err, errTorrentNotFound) {
		t.Fatalf("missing torrent error=%v, want errTorrentNotFound", err)
	}
}
