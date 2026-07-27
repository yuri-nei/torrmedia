package main

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeFile struct {
	name     string
	size     int64
	priority int
	progress float64
}

type fakeTorrent struct {
	hash     string
	name     string
	savePath string
	category string
	magnet   string
	state    string
	files    []fakeFile
}

type fakeQbit struct {
	mu        sync.Mutex
	torrents  []*fakeTorrent
	pieceSize int64
	calls     []string
	failInfo  bool
	recheckIn time.Duration
}

func newFakeQbit(t *fakeTorrent) *fakeQbit {
	return &fakeQbit{torrents: []*fakeTorrent{t}, pieceSize: 64 << 10, recheckIn: 20 * time.Millisecond}
}

func (f *fakeQbit) snapshotCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeQbit) find(hash string) *fakeTorrent {
	for _, t := range f.torrents {
		if t.hash == hash {
			return t
		}
	}
	return nil
}

func (f *fakeQbit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ParseForm()
	hash := r.Form.Get("hashes")
	if hash == "" {
		hash = r.Form.Get("hash")
	}
	if hash == "" {
		hash = r.URL.Query().Get("hashes")
	}
	if hash == "" {
		hash = r.URL.Query().Get("hash")
	}

	switch r.URL.Path {
	case "/api/v2/auth/login":
		w.WriteHeader(http.StatusNoContent)
		return
	case "/api/v2/app/version":
		w.Write([]byte("v5.2.3"))
		return
	case "/api/v2/torrents/info":
		if f.failInfo {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		out := []qbTorrent{}
		for _, t := range f.torrents {
			if hash != "" && t.hash != hash {
				continue
			}
			var size, completed int64
			for _, file := range t.files {
				size += file.size
				completed += int64(float64(file.size) * file.progress)
			}
			out = append(out, qbTorrent{
				Hash: t.hash, Name: t.name, SavePath: t.savePath,
				ContentPath: filepath.Join(t.savePath, t.name), State: t.state,
				Category: t.category, MagnetURI: t.magnet, Size: size,
				Completed: completed, Progress: 1, AddedOn: 1700000000,
				NumSeeds: 1, NumComplete: 2, ETA: 8640000,
			})
		}
		json.NewEncoder(w).Encode(out)
		return
	case "/api/v2/torrents/files":
		t := f.find(hash)
		if t == nil {
			http.NotFound(w, r)
			return
		}
		out := make([]qbFile, 0, len(t.files))
		for i := range t.files {
			index := i
			out = append(out, qbFile{
				Index: &index, Name: t.files[i].name, Size: t.files[i].size,
				Priority: t.files[i].priority, Progress: t.files[i].progress,
			})
		}
		json.NewEncoder(w).Encode(out)
		return
	case "/api/v2/torrents/properties":
		json.NewEncoder(w).Encode(qbProperties{PieceSize: f.pieceSize})
		return
	case "/api/v2/torrents/categories":
		json.NewEncoder(w).Encode(map[string]qbCategory{
			"Shows":  {Name: "Shows", SavePath: "/var/media/library/shows"},
			"Movies": {Name: "Movies", SavePath: "/var/media/library/movies"},
		})
		return
	case "/api/v2/torrents/stop", "/api/v2/torrents/pause":
		f.calls = append(f.calls, "stop")
		if t := f.find(hash); t != nil {
			t.state = "stoppedUP"
		}
	case "/api/v2/torrents/start", "/api/v2/torrents/resume":
		f.calls = append(f.calls, "start")
		if t := f.find(hash); t != nil {
			t.state = "uploading"
		}
	case "/api/v2/torrents/setForceStart":
		f.calls = append(f.calls, "forceStart="+r.Form.Get("value"))
	case "/api/v2/torrents/filePrio":
		t := f.find(hash)
		id, err := strconv.Atoi(r.Form.Get("id"))
		priority, err2 := strconv.Atoi(r.Form.Get("priority"))
		if t == nil || err != nil || err2 != nil || id < 0 || id >= len(t.files) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		t.files[id].priority = priority
		f.calls = append(f.calls, "prio"+strconv.Itoa(priority)+":"+strconv.Itoa(id))
	case "/api/v2/torrents/recheck":
		f.calls = append(f.calls, "recheck")
		t := f.find(hash)
		if t != nil {
			t.state = "checkingUP"
			delay := f.recheckIn
			go func() {
				time.Sleep(delay)
				f.mu.Lock()
				t.state = "stoppedUP"
				f.mu.Unlock()
			}()
		}
	case "/api/v2/torrents/delete":
		f.calls = append(f.calls, "delete:deleteFiles="+r.Form.Get("deleteFiles"))
		kept := f.torrents[:0]
		for _, t := range f.torrents {
			if t.hash != hash {
				kept = append(kept, t)
			}
		}
		f.torrents = kept
	case "/api/v2/torrents/add":
		f.calls = append(f.calls, "add:category="+r.Form.Get("category"))
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func testRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
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

type testEnv struct {
	*server
	fake *fakeQbit
	root string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := testRoot(t)
	const size = int64(1 << 20)
	writeTestFile(t, filepath.Join(root, "shows", "Release", "e01.mkv"), size)
	writeTestFile(t, filepath.Join(root, "shows", "Release", "e02.mkv"), size)

	fake := newFakeQbit(&fakeTorrent{
		hash: "0123456789012345678901234567890123456789", name: "Release",
		savePath: filepath.Join(root, "shows"), category: "Shows",
		magnet: "magnet:?xt=urn:btih:0123456789012345678901234567890123456789&tr=http://tracker",
		state:  "stoppedUP",
		files: []fakeFile{
			{name: filepath.Join("Release", "e01.mkv"), size: size, priority: 1, progress: 1},
			{name: filepath.Join("Release", "e02.mkv"), size: size, priority: 1, progress: 1},
		},
	})
	return newTestEnvWith(t, root, fake)
}

func newTestEnvWith(t *testing.T, root string, fake *fakeQbit) *testEnv {
	t.Helper()
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	store, err := newViewedStore(filepath.Join(t.TempDir(), "viewed.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		root: root, qb: qb, viewed: store, playback: defaultPlaybackConfig(),
		deleteDebounce: time.Millisecond, operationTimeout: 3 * time.Second,
		pollInterval: 5 * time.Millisecond, statePollInterval: time.Hour,
		enableReserve:  512 << 20,
		availableBytes: func(string) (int64, error) { return 100 << 30, nil },
	}
	s.ensureRuntime()
	s.operations.notify = s.publishOperation
	return &testEnv{server: s, fake: fake, root: root}
}

func (e *testEnv) firstItem(t *testing.T) (*catalog, *catalogItem) {
	t.Helper()
	cat := e.buildCatalog()
	e.catalog.Store(cat)
	if len(cat.items) == 0 {
		t.Fatal("catalog is empty")
	}
	return cat, cat.items[0]
}

func (e *testEnv) awaitOperation(t *testing.T, id string) operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, ok := e.ops().get(id)
		if !ok {
			t.Fatalf("operation %s disappeared", id)
		}
		if op.State == opDone || op.State == opFailed {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s stuck in %s/%v", id, op.State, op.Phase)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *testEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.routes().ServeHTTP(rec, req)
	return rec
}
