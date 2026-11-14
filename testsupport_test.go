package main

// testsupport_test.go is the shared test scaffolding: a fake qBittorrent and
// server assembly. The fake deliberately behaves like the real thing: it
// stores priorities, changes state on commands and does not finish a recheck
// instantly.

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
	name     string // path relative to save_path
	size     int64
	priority int
	progress float64
}

type fakeTorrent struct {
	hash     string
	name     string // name of the root folder on disk
	dn       string // qBittorrent name if it differs from the folder (magnet dn)
	savePath string
	category string
	magnet   string
	state    string
	files    []fakeFile
	tags     []string
	addedOn  int64

	// Recheck as qBittorrent 5.3 does it (TorrentImpl::forceRecheck): it
	// starts a stopped torrent itself for the duration of the check and stops
	// it afterwards, and a start during the check does not cancel that.
	checking       bool
	stopAfterCheck bool
	checkProgress  float64 // progress in the API while the check runs
	checkActive    bool    // left the check queue (see fakeQbit.serialChecks)
	// errorState, when set ("error" or "missingFiles"), is what the API
	// reports instead of the stopped or running state, as qBittorrent does:
	// stop keeps it, start clears it, a recheck clears missingFiles.
	errorState string
}

func (t *fakeTorrent) hasTag(tag string) bool {
	for _, existing := range t.tags {
		if existing == tag {
			return true
		}
	}
	return false
}

type fakeQbit struct {
	mu        sync.Mutex
	torrents  []*fakeTorrent
	pieceSize int64
	calls     []string
	failInfo  bool // /torrents/info responds with an error
	recheckIn time.Duration
	// serialChecks models libtorrent's check queue: one torrent is checked at
	// a time, the others wait as "checking" at 0%.
	serialChecks bool
	// vanishing names a torrent that /torrents/info still lists but whose
	// files already answer 404, as if it were removed between the requests.
	vanishing string
	// stateLag is how long after a recheck the API still returns the previous
	// state: qBittorrent caches it and recomputes it once per refresh
	// interval.
	stateLag time.Duration

	// Magnet add scenario. While addHash is empty, /torrents/add is only
	// recorded in calls; that is how tests that do not care about adding
	// behave.
	addHash        string
	addName        string
	addSavePath    string
	addFiles       []fakeFile
	metadataIn     time.Duration // how long after add the file list appears
	filesMissing   bool          // 404 before metadata, not an empty list
	ignoreStopCond bool          // qBittorrent "does not support" stopCondition
}

func newFakeQbit(t *fakeTorrent) *fakeQbit {
	return &fakeQbit{torrents: []*fakeTorrent{t}, pieceSize: 64 << 10, recheckIn: 20 * time.Millisecond}
}

// otherCheckActive reports whether a torrent other than t is being checked
// right now. The caller holds f.mu.
func (f *fakeQbit) otherCheckActive(t *fakeTorrent) bool {
	for _, other := range f.torrents {
		if other != t && other.checkActive {
			return true
		}
	}
	return false
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
			addedOn := t.addedOn
			if addedOn == 0 {
				addedOn = 1700000000
			}
			shown := t.name
			if t.dn != "" {
				shown = t.dn
			}
			progress := 1.0
			state := t.state
			if t.checking {
				progress = t.checkProgress
			} else if t.errorState != "" {
				state = t.errorState
			}
			out = append(out, qbTorrent{
				Hash: t.hash, Name: shown, SavePath: t.savePath,
				ContentPath: filepath.Join(t.savePath, t.name), State: state,
				Category: t.category, MagnetURI: t.magnet, Size: size,
				Completed: completed, Progress: progress, AddedOn: addedOn,
				NumSeeds: 1, ETA: 8640000, Tags: strings.Join(t.tags, ", "),
			})
		}
		json.NewEncoder(w).Encode(out)
		return
	case "/api/v2/torrents/files":
		t := f.find(hash)
		if t == nil || t.hash == f.vanishing {
			http.NotFound(w, r)
			return
		}
		if len(t.files) == 0 && f.filesMissing {
			// Until the magnet's metadata arrives, there is no file list.
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
			t.stopAfterCheck = t.checking
		}
	case "/api/v2/torrents/start", "/api/v2/torrents/resume":
		f.calls = append(f.calls, "start")
		// During a check the torrent is already "started" for its sake: start
		// changes nothing, and the stop after the check stays in effect.
		if t := f.find(hash); t != nil && !t.checking {
			t.state, t.errorState = "uploading", ""
		}
	case "/api/v2/torrents/setForceStart":
		f.calls = append(f.calls, "forceStart="+r.Form.Get("value"))
	case "/api/v2/torrents/filePrio":
		t := f.find(hash)
		priority, err := strconv.Atoi(r.Form.Get("priority"))
		if t == nil || err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// id comes as a single value or a "|"-separated list, as in the WebAPI.
		for _, raw := range strings.Split(r.Form.Get("id"), "|") {
			id, err := strconv.Atoi(raw)
			if err != nil || id < 0 || id >= len(t.files) {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			t.files[id].priority = priority
			f.calls = append(f.calls, "prio"+strconv.Itoa(priority)+":"+strconv.Itoa(id))
		}
	case "/api/v2/torrents/removeTags":
		t := f.find(hash)
		if t == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, raw := range strings.Split(r.Form.Get("tags"), ",") {
			drop := strings.TrimSpace(raw)
			kept := t.tags[:0]
			for _, tag := range t.tags {
				if tag != drop {
					kept = append(kept, tag)
				}
			}
			t.tags = kept
		}
		f.calls = append(f.calls, "removeTags:"+r.Form.Get("tags"))
	case "/api/v2/torrents/setCategory":
		t := f.find(hash)
		if t == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		t.category = r.Form.Get("category")
		f.calls = append(f.calls, "setCategory:"+t.category)
	case "/api/v2/torrents/recheck":
		f.calls = append(f.calls, "recheck")
		t := f.find(hash)
		if t != nil && !t.checking {
			t.checking, t.checkProgress = true, 0
			t.stopAfterCheck = qbitStoppedState(t.state)
			lag, total := f.stateLag, f.recheckIn
			if lag <= 0 {
				t.state = "checkingUP"
			}
			issued := time.Now()
			started := issued
			t.checkActive = !(f.serialChecks && f.otherCheckActive(t))
			go func() {
				for {
					time.Sleep(time.Millisecond)
					f.mu.Lock()
					if time.Since(issued) >= lag {
						t.state = "checkingUP"
					}
					if !t.checkActive {
						if f.otherCheckActive(t) {
							f.mu.Unlock()
							continue
						}
						t.checkActive, started = true, time.Now()
					}
					elapsed := time.Since(started)
					if elapsed >= total {
						t.checking, t.checkActive = false, false
						if t.errorState == "missingFiles" {
							t.errorState = ""
						}
						if t.stopAfterCheck {
							t.state = "stoppedUP"
						} else {
							t.state = "uploading"
						}
						f.mu.Unlock()
						return
					}
					t.checkProgress = float64(elapsed) / float64(total)
					f.mu.Unlock()
				}
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
		if f.addHash != "" {
			f.calls = append(f.calls, "add:tags="+r.Form.Get("tags"),
				"add:stopCondition="+r.Form.Get("stopCondition"))
			added := &fakeTorrent{
				hash: f.addHash, name: f.addName, savePath: f.addSavePath,
				category: r.Form.Get("category"), magnet: r.Form.Get("urls"),
				state: "metaDL", addedOn: time.Now().Unix(),
			}
			for _, raw := range strings.Split(r.Form.Get("tags"), ",") {
				if tag := strings.TrimSpace(raw); tag != "" {
					added.tags = append(added.tags, tag)
				}
			}
			f.torrents = append(f.torrents, added)
			// Metadata does not arrive instantly, and qBittorrent stops the
			// torrent itself if it was asked to when adding.
			files, delay := f.addFiles, f.metadataIn
			stopOnMetadata := r.Form.Get("stopCondition") == "MetadataReceived" && !f.ignoreStopCond
			go func() {
				time.Sleep(delay)
				f.mu.Lock()
				added.files = files
				if stopOnMetadata {
					added.state = "stoppedDL"
				} else {
					added.state = "downloading"
				}
				f.mu.Unlock()
			}()
		}
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// testRoot returns the catalog root with symlinks resolved, as in
// production.
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

// newTestEnv starts a server with a single item "Release" of two episodes,
// both fully on disk.
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
	// With a margin over the fake's stateLag (realisticRecheck), but without
	// seconds of waiting.
	qb.stateLag = 200 * time.Millisecond
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
		totalBytes:     func(string) (int64, error) { return 200 << 30, nil },
		// In tests a draft must fit into seconds: there is no reason to wait
		// for real minutes of metadata here.
		draftAddTimeout:      2 * time.Second,
		draftMetadataTimeout: 2 * time.Second,
		draftTTL:             time.Hour,
	}
	s.ensureRuntime()
	s.operations.notify = s.publishOperation
	return &testEnv{server: s, fake: fake, root: root}
}

// firstItem returns the catalog's only item; handy for most tests.
func (e *testEnv) firstItem(t *testing.T) (*catalog, *catalogItem) {
	t.Helper()
	cat := e.buildCatalog()
	e.catalog.Store(cat)
	if len(cat.items) == 0 {
		t.Fatal("catalog is empty")
	}
	return cat, cat.items[0]
}

// realisticRecheck turns on state lag in the fake, as in a live qBittorrent:
// for the first 40 ms after a recheck the API still returns the previous
// state.
func (e *testEnv) realisticRecheck(total time.Duration) {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	e.fake.stateLag = 40 * time.Millisecond
	e.fake.recheckIn = total
}

// settledState waits for the fake to finish the recheck and returns the
// torrent's final state, the one the user will find it in.
func (e *testEnv) settledState(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.fake.mu.Lock()
		checking, state := e.fake.torrents[0].checking, e.fake.torrents[0].state
		e.fake.mu.Unlock()
		if !checking {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatal("fake qBittorrent is still checking")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitOperation waits for an operation to finish and returns it.
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
	req.Host = "127.0.0.1:8091" // a client names the server by address; httptest's default is example.com
	rec := httptest.NewRecorder()
	e.routes().ServeHTTP(rec, req)
	return rec
}
