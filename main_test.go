package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeQbit struct {
	mu         sync.Mutex
	hash       string
	name       string
	savePath   string
	fileName   string
	fileSize   int64
	pieceSize  int64
	state      string
	priority   int
	progress   float64
	amountLeft int64
	calls      []string
	noTorrent  bool
	startDelay time.Duration
	startStuck bool
}

type batchFakeFile struct {
	name     string
	size     int64
	priority int
	progress float64
}

type batchFakeQbit struct {
	mu        sync.Mutex
	hash      string
	name      string
	savePath  string
	pieceSize int64
	state     string
	files     []batchFakeFile
	calls     []string
}

func (f *batchFakeQbit) snapshotCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *batchFakeQbit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/api/v2/torrents/info":
		json.NewEncoder(w).Encode([]qbTorrent{{
			Hash: f.hash, Name: f.name, SavePath: f.savePath,
			ContentPath: filepath.Join(f.savePath, f.name), State: f.state,
		}})
		return
	case "/api/v2/torrents/files":
		files := make([]qbFile, 0, len(f.files))
		for i := range f.files {
			index := i
			files = append(files, qbFile{
				Index: &index, Name: f.files[i].name, Size: f.files[i].size,
				Priority: f.files[i].priority, Progress: f.files[i].progress,
			})
		}
		json.NewEncoder(w).Encode(files)
		return
	case "/api/v2/torrents/properties":
		json.NewEncoder(w).Encode(qbProperties{PieceSize: f.pieceSize})
		return
	case "/api/v2/torrents/stop", "/api/v2/torrents/pause":
		f.calls = append(f.calls, "stop")
		f.state = "stoppedUP"
	case "/api/v2/torrents/start", "/api/v2/torrents/resume":
		f.calls = append(f.calls, "start")
		f.state = "uploading"
	case "/api/v2/torrents/filePrio":
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := strconv.Atoi(r.Form.Get("id"))
		if err != nil || id < 0 || id >= len(f.files) {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		priority, err := strconv.Atoi(r.Form.Get("priority"))
		if err != nil {
			http.Error(w, "bad priority", http.StatusBadRequest)
			return
		}
		f.files[id].priority = priority
		f.calls = append(f.calls, fmt.Sprintf("prio%d:%d", priority, id))
	case "/api/v2/torrents/recheck":
		f.calls = append(f.calls, "recheck")
		f.state = "checkingUP"
		go func() {
			time.Sleep(40 * time.Millisecond)
			f.mu.Lock()
			f.state = "stoppedDL"
			for i := range f.files {
				if f.files[i].priority == 0 {
					f.files[i].progress = 0.125
				}
			}
			f.mu.Unlock()
		}()
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQbit) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeQbit) snapshotCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeQbit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/api/v2/torrents/info":
		w.Header().Set("Content-Type", "application/json")
		if f.noTorrent {
			io.WriteString(w, "[]")
			return
		}
		json.NewEncoder(w).Encode([]qbTorrent{{
			Hash: f.hash, Name: f.name, SavePath: f.savePath,
			ContentPath: filepath.Join(f.savePath, f.name),
			State:       f.state, AmountLeft: f.amountLeft,
		}})
		return
	case "/api/v2/torrents/files":
		index := 0
		json.NewEncoder(w).Encode([]qbFile{{
			Index: &index, Name: f.fileName, Size: f.fileSize,
			Priority: f.priority, Progress: f.progress,
		}})
		return
	case "/api/v2/torrents/properties":
		json.NewEncoder(w).Encode(qbProperties{PieceSize: f.pieceSize})
		return
	case "/api/v2/torrents/stop", "/api/v2/torrents/pause":
		f.record("stop")
		f.state = "stoppedUP"
	case "/api/v2/torrents/start", "/api/v2/torrents/resume":
		f.record("start")
		if !f.startStuck {
			if f.startDelay > 0 {
				delay := f.startDelay
				go func() {
					time.Sleep(delay)
					f.mu.Lock()
					f.state = "downloading"
					f.amountLeft = f.fileSize
					f.mu.Unlock()
				}()
			} else {
				f.state = "downloading"
				f.amountLeft = f.fileSize
			}
		}
	case "/api/v2/torrents/filePrio":
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		priority, err := strconv.Atoi(r.Form.Get("priority"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.priority = priority
		f.record("prio" + strconv.Itoa(priority))
	case "/api/v2/torrents/recheck":
		f.record("recheck")
		f.state = "checkingDL"
		go func() {
			time.Sleep(40 * time.Millisecond)
			f.mu.Lock()
			f.state = "stoppedDL"
			f.progress = 0.125
			f.amountLeft = f.fileSize * 7 / 8
			f.mu.Unlock()
		}()
	case "/api/v2/torrents/delete":
		f.record("DELETE_TORRENT")
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func newTestServer(t *testing.T, root string, fake *fakeQbit) (*server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(fake)
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		root:             root,
		qb:               qb,
		deleteDebounce:   time.Millisecond,
		operationTimeout: 2 * time.Second,
		pollInterval:     5 * time.Millisecond,
		enableReserve:    512 << 20,
		availableBytes: func(string) (int64, error) {
			return 100 << 30, nil
		},
	}, upstream
}

func writeTestFile(t *testing.T, path string, size int64) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, int(size))
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func testRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func waitForDeleteBatchSize(t *testing.T, s *server, hash string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
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

func TestHTTPRoutesReachRegisteredHandlers(t *testing.T) {
	s := &server{root: testRoot(t)}
	handler := s.routes()

	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodGet, path: "/health", status: http.StatusOK},
		{method: http.MethodGet, path: "/list", status: http.StatusOK},
		{method: http.MethodPost, path: "/health", status: http.StatusMethodNotAllowed},
	} {
		req := httptest.NewRequest(test.method, test.path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != test.status {
			t.Fatalf("%s %s status=%d body=%s, want %d",
				test.method, test.path, recorder.Code, recorder.Body.String(), test.status)
		}
	}
}

func TestDeleteTorrentFileTrimsAndNeverDeletesTorrent(t *testing.T) {
	root := testRoot(t)
	const pieceSize = int64(64 << 10)
	const fileSize = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	abs := filepath.Join(root, rel)
	original := writeTestFile(t, abs, fileSize)

	fake := &fakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: filepath.Join(root, "shows"),
		fileName: filepath.Join("release", "episode.mkv"),
		fileSize: fileSize, pieceSize: pieceSize,
		state: "downloading", priority: 1, progress: 1,
	}
	s, upstream := newTestServer(t, root, fake)
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodDelete, "/file?path="+url.QueryEscape(rel), nil)
	rec := httptest.NewRecorder()
	s.handleDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}

	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("trimmed placeholder must remain: %v", err)
	}
	if info.Size() != fileSize {
		t.Fatalf("logical size=%d, want %d", info.Size(), fileSize)
	}
	trimmed, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(trimmed[:pieceSize], original[:pieceSize]) {
		t.Fatal("prefix boundary piece was not preserved")
	}
	if !bytes.Equal(trimmed[fileSize-pieceSize:], original[fileSize-pieceSize:]) {
		t.Fatal("suffix boundary piece was not preserved")
	}
	if !bytes.Equal(trimmed[pieceSize:fileSize-pieceSize], make([]byte, fileSize-2*pieceSize)) {
		t.Fatal("interior was not made sparse/zero-filled")
	}

	calls := strings.Join(fake.snapshotCalls(), ",")
	if calls != "stop,prio0,recheck,start" {
		t.Fatalf("mutation order=%q", calls)
	}
	if strings.Contains(calls, "DELETE_TORRENT") {
		t.Fatal("single-file DELETE must never call torrents/delete")
	}

	// APFS keeps old physical extents accounted after truncate+regrow. Production
	// is Linux/ext4, where the sparse allocation and readiness guard are verified.
	if runtime.GOOS == "linux" {
		if complete(info) {
			t.Fatal("trimmed sparse placeholder must not be exposed as complete")
		}
		getReq := httptest.NewRequest(http.MethodGet, "/file?path="+url.QueryEscape(rel), nil)
		getRec := httptest.NewRecorder()
		s.handleFile(getRec, getReq)
		if getRec.Code != http.StatusNotFound {
			t.Fatalf("direct GET of sparse placeholder status=%d, want 404", getRec.Code)
		}
	}
}

func TestDeleteDebounceBatchesFilesIntoOneRecheck(t *testing.T) {
	root := testRoot(t)
	const pieceSize = int64(64 << 10)
	const fileSize = int64(1 << 20)
	release := filepath.Join("shows", "release")
	rels := []string{
		filepath.Join(release, "episode-1.mkv"),
		filepath.Join(release, "episode-2.mkv"),
	}
	originals := make([][]byte, 0, len(rels))
	for _, rel := range rels {
		originals = append(originals, writeTestFile(t, filepath.Join(root, rel), fileSize))
	}

	fake := &batchFakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: filepath.Join(root, "shows"),
		pieceSize: pieceSize, state: "downloading",
		files: []batchFakeFile{
			{name: filepath.Join("release", "episode-1.mkv"), size: fileSize, priority: 1, progress: 1},
			{name: filepath.Join("release", "episode-2.mkv"), size: fileSize, priority: 1, progress: 1},
		},
	}
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		root: root, qb: qb, deleteDebounce: 40 * time.Millisecond,
		operationTimeout: 2 * time.Second, pollInterval: 5 * time.Millisecond,
	}

	recorders := []*httptest.ResponseRecorder{
		httptest.NewRecorder(),
		httptest.NewRecorder(),
	}
	done := make(chan int, len(rels))
	for i, rel := range rels {
		if i > 0 {
			// The second selection is inside the debounce window and must
			// extend it, rather than starting an independent operation.
			time.Sleep(20 * time.Millisecond)
		}
		go func(index int, path string) {
			req := httptest.NewRequest(http.MethodDelete,
				"/file?path="+url.QueryEscape(path), nil)
			s.handleDelete(recorders[index], req)
			done <- index
		}(i, rel)
		waitForDeleteBatchSize(t, s, fake.hash, i+1)
	}
	time.Sleep(30 * time.Millisecond)
	if calls := fake.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("batch started before 40ms of silence after the last selection: %v", calls)
	}

	for range rels {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("debounced DELETE did not complete")
		}
	}
	for i, recorder := range recorders {
		if recorder.Code != http.StatusOK {
			t.Fatalf("DELETE %d status=%d body=%s", i, recorder.Code, recorder.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["batch_size"] != float64(2) {
			t.Fatalf("DELETE %d response is not the shared two-file batch: %v", i, response)
		}
	}

	if calls := strings.Join(fake.snapshotCalls(), ","); calls !=
		"stop,prio0:0,prio0:1,recheck,start" {
		t.Fatalf("batch mutation order=%q", calls)
	}
	for i, rel := range rels {
		trimmed, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(trimmed[:pieceSize], originals[i][:pieceSize]) ||
			!bytes.Equal(trimmed[fileSize-pieceSize:], originals[i][fileSize-pieceSize:]) {
			t.Fatalf("%s boundary pieces were not preserved", rel)
		}
		if !bytes.Equal(trimmed[pieceSize:fileSize-pieceSize],
			make([]byte, fileSize-2*pieceSize)) {
			t.Fatalf("%s interior was not made sparse/zero-filled", rel)
		}
	}
}

func TestDeleteByHashAndIDBatchesPartialAndMissingFiles(t *testing.T) {
	root := testRoot(t)
	const fileSize = int64(1 << 20)
	rels := []string{
		filepath.Join("release", "partial.mkv"),
		filepath.Join("release", "not-created.mkv"),
	}
	partialAbs := filepath.Join(root, rels[0])
	if err := os.MkdirAll(filepath.Dir(partialAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	partial, err := os.Create(partialAbs)
	if err != nil {
		t.Fatal(err)
	}
	if err := partial.Truncate(fileSize); err != nil {
		partial.Close()
		t.Fatal(err)
	}
	if _, err := partial.WriteAt(bytes.Repeat([]byte{0x5a}, 4096), 0); err != nil {
		partial.Close()
		t.Fatal(err)
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}

	fake := &batchFakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: root,
		pieceSize: 64 << 10, state: "stoppedDL",
		files: []batchFakeFile{
			{name: rels[0], size: fileSize, priority: 1, progress: 0.01},
			{name: rels[1], size: fileSize, priority: 1, progress: 0},
		},
	}
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		root: root, qb: qb, deleteDebounce: 40 * time.Millisecond,
		operationTimeout: 2 * time.Second, pollInterval: 5 * time.Millisecond,
	}

	recorders := []*httptest.ResponseRecorder{
		httptest.NewRecorder(),
		httptest.NewRecorder(),
	}
	done := make(chan int, len(recorders))
	for i := range recorders {
		go func(fileID int) {
			req := httptest.NewRequest(http.MethodDelete,
				"/file?hash="+url.QueryEscape(fake.hash)+"&id="+strconv.Itoa(fileID), nil)
			s.handleDelete(recorders[fileID], req)
			done <- fileID
		}(i)
		waitForDeleteBatchSize(t, s, fake.hash, i+1)
	}
	for range recorders {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("hash+ID delete batch did not complete")
		}
	}
	for i, recorder := range recorders {
		if recorder.Code != http.StatusOK {
			t.Fatalf("DELETE %d status=%d body=%s", i, recorder.Code, recorder.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["batch_size"] != float64(2) {
			t.Fatalf("DELETE %d response is not the shared two-file batch: %v", i, response)
		}
	}
	if calls := strings.Join(fake.snapshotCalls(), ","); calls !=
		"prio0:0,prio0:1,recheck" {
		t.Fatalf("hash+ID batch mutation order=%q", calls)
	}
	if _, err := os.Stat(filepath.Join(root, rels[1])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file was unexpectedly created: %v", err)
	}
}

func TestDeleteBatchPreflightFailureDoesNotTrimValidSibling(t *testing.T) {
	root := testRoot(t)
	const fileSize = int64(1 << 20)
	rels := []string{
		filepath.Join("shows", "release", "episode-1.mkv"),
		filepath.Join("shows", "release", "episode-2.mkv"),
	}
	firstOriginal := writeTestFile(t, filepath.Join(root, rels[0]), fileSize)
	writeTestFile(t, filepath.Join(root, rels[1]), fileSize)

	fake := &batchFakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: filepath.Join(root, "shows"),
		pieceSize: 64 << 10, state: "stoppedUP",
		files: []batchFakeFile{
			{name: filepath.Join("release", "episode-1.mkv"), size: fileSize, priority: 1, progress: 1},
			{name: filepath.Join("release", "episode-2.mkv"), size: fileSize, priority: 1, progress: 1},
		},
	}
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		root: root, qb: qb, deleteDebounce: 80 * time.Millisecond,
		operationTimeout: 2 * time.Second, pollInterval: 5 * time.Millisecond,
	}

	recorders := []*httptest.ResponseRecorder{
		httptest.NewRecorder(),
		httptest.NewRecorder(),
	}
	done := make(chan struct{}, len(rels))
	for i, rel := range rels {
		go func(index int, path string) {
			req := httptest.NewRequest(http.MethodDelete,
				"/file?path="+url.QueryEscape(path), nil)
			s.handleDelete(recorders[index], req)
			done <- struct{}{}
		}(i, rel)
		waitForDeleteBatchSize(t, s, fake.hash, i+1)
	}

	// Simulate an external replacement after both requests were accepted.
	// The package must fail closed before touching its still-valid sibling.
	if err := os.Remove(filepath.Join(root, rels[1])); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, rels[1]), fileSize)

	for range rels {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("failed batch did not return")
		}
	}
	for i, recorder := range recorders {
		if recorder.Code != http.StatusConflict {
			t.Fatalf("DELETE %d status=%d body=%s", i, recorder.Code, recorder.Body.String())
		}
	}
	if calls := fake.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("preflight failure mutated qBittorrent: %v", calls)
	}
	firstAfter, err := os.ReadFile(filepath.Join(root, rels[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstAfter, firstOriginal) {
		t.Fatal("valid sibling changed despite package preflight failure")
	}
}

func TestEnableUsesOnePriorityChangeAndStart(t *testing.T) {
	root := testRoot(t)
	const fileSize = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(fileSize); err != nil {
		t.Fatal(err)
	}
	f.Close()

	fake := &fakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: filepath.Join(root, "shows"),
		fileName: filepath.Join("release", "episode.mkv"),
		fileSize: fileSize, pieceSize: 64 << 10,
		state: "stoppedDL", priority: 0, progress: 0.125,
		startDelay: 30 * time.Millisecond,
	}
	s, upstream := newTestServer(t, root, fake)
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost,
		"/file/enable?hash="+fake.hash+"&id=0", nil)
	rec := httptest.NewRecorder()
	s.handleEnable(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls := strings.Join(fake.snapshotCalls(), ","); calls != "prio1,start" {
		t.Fatalf("enable mutation order=%q, want prio1,start", calls)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["state"] != "downloading" {
		t.Fatalf("enable returned before start acknowledgement: %v", response)
	}
}

func TestEnableStartTimeoutStopsAndRollsBackPriority(t *testing.T) {
	root := testRoot(t)
	fake := &fakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: filepath.Join(root, "shows"),
		fileName: filepath.Join("release", "episode.mkv"),
		fileSize: 1 << 20, pieceSize: 64 << 10,
		state: "stoppedDL", priority: 0, startStuck: true,
	}
	if err := os.MkdirAll(fake.savePath, 0o755); err != nil {
		t.Fatal(err)
	}
	s, upstream := newTestServer(t, root, fake)
	defer upstream.Close()
	s.operationTimeout = 50 * time.Millisecond

	req := httptest.NewRequest(http.MethodPost,
		"/file/enable?hash="+fake.hash+"&id=0", nil)
	rec := httptest.NewRecorder()
	s.handleEnable(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls := strings.Join(fake.snapshotCalls(), ","); calls != "prio1,start,stop,prio0" {
		t.Fatalf("timeout cleanup order=%q", calls)
	}
	fake.mu.Lock()
	priority := fake.priority
	state := fake.state
	fake.mu.Unlock()
	if priority != 0 || !qbitStoppedState(state) {
		t.Fatalf("timeout left unsafe state=%s priority=%d", state, priority)
	}
}

func TestEnableInsufficientStorageDoesNotMutateTorrent(t *testing.T) {
	root := testRoot(t)
	fake := &fakeQbit{
		hash: "0123456789012345678901234567890123456789",
		name: "release", savePath: filepath.Join(root, "shows"),
		fileName: filepath.Join("release", "episode.mkv"),
		fileSize: 1 << 30, pieceSize: 64 << 10,
		state: "stoppedDL", priority: 0,
	}
	if err := os.MkdirAll(fake.savePath, 0o755); err != nil {
		t.Fatal(err)
	}
	s, upstream := newTestServer(t, root, fake)
	defer upstream.Close()
	s.enableReserve = 0
	s.availableBytes = func(string) (int64, error) { return 1 << 20, nil }

	req := httptest.NewRequest(http.MethodPost,
		"/file/enable?hash="+fake.hash+"&id=0", nil)
	rec := httptest.NewRecorder()
	s.handleEnable(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls := fake.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("insufficient-space preflight mutated torrent: %v", calls)
	}
}

func TestDeleteNonTorrentFileRemovesOnlyRequestedPath(t *testing.T) {
	root := testRoot(t)
	targetRel := filepath.Join("movies", "target.mkv")
	siblingRel := filepath.Join("movies", "keep.mkv")
	target := filepath.Join(root, targetRel)
	sibling := filepath.Join(root, siblingRel)
	writeTestFile(t, target, 4096)
	writeTestFile(t, sibling, 4096)

	fake := &fakeQbit{noTorrent: true}
	s, upstream := newTestServer(t, root, fake)
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodDelete,
		"/file?path="+url.QueryEscape(targetRel), nil)
	rec := httptest.NewRecorder()
	s.handleDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target still exists or unexpected error: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling was affected: %v", err)
	}
	if calls := fake.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("non-torrent delete mutated qBit: %v", calls)
	}
}

func TestFindFileRefusesInodeOwnedByMultipleTorrentEntries(t *testing.T) {
	root := testRoot(t)
	first := filepath.Join(root, "shows", "first", "episode.mkv")
	second := filepath.Join(root, "shows", "second", "episode.mkv")
	writeTestFile(t, first, 4096)
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}
	target, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			json.NewEncoder(w).Encode([]qbTorrent{
				{Hash: "hash1", SavePath: filepath.Join(root, "shows")},
				{Hash: "hash2", SavePath: filepath.Join(root, "shows")},
			})
		case "/api/v2/torrents/files":
			index := 0
			name := filepath.Join("first", "episode.mkv")
			if r.URL.Query().Get("hash") == "hash2" {
				name = filepath.Join("second", "episode.mkv")
			}
			json.NewEncoder(w).Encode([]qbFile{{Index: &index, Name: name, Priority: 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qb.findFile(target); err == nil || !strings.Contains(err.Error(), "multiple torrent") {
		t.Fatalf("ambiguous inode error=%v", err)
	}
}
