package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type gatewayFake struct {
	inner       *batchFakeQbit
	category    string
	size        int64
	forceStart  bool
	deleteCalls []string
}

func (g *gatewayFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v2/torrents/info":
		json.NewEncoder(w).Encode([]qbTorrent{{
			Hash: g.inner.hash, Name: g.inner.name, SavePath: g.inner.savePath,
			ContentPath: filepath.Join(g.inner.savePath, g.inner.name),
			State:       g.inner.state, Progress: 1, Category: g.category,
			Size: g.size, ForceStart: g.forceStart,
		}})
		return
	case "/api/v2/torrents/categories":
		json.NewEncoder(w).Encode(map[string]qbCategory{
			"Shows":  {Name: "Shows", SavePath: "/var/media/library/shows"},
			"Movies": {Name: "Movies", SavePath: "/var/media/library/movies"},
		})
		return
	case "/api/v2/torrents/setForceStart":
		r.ParseForm()
		g.forceStart = r.Form.Get("value") == "true"
		g.deleteCalls = append(g.deleteCalls, "forceStart="+r.Form.Get("value"))
		w.WriteHeader(http.StatusOK)
		return
	case "/api/v2/torrents/delete":
		r.ParseForm()
		g.deleteCalls = append(g.deleteCalls, "delete:deleteFiles="+r.Form.Get("deleteFiles"))
		w.WriteHeader(http.StatusOK)
		return
	}
	g.inner.ServeHTTP(w, r)
}

func newGatewayServer(t *testing.T, root string, fake *gatewayFake) *server {
	t.Helper()
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	qb, err := newQbit(upstream.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		root: root, qb: qb, deleteDebounce: time.Millisecond,
		operationTimeout: 2 * time.Second, pollInterval: 5 * time.Millisecond,
		enableReserve:  512 << 20,
		availableBytes: func(string) (int64, error) { return 100 << 30, nil },
	}
}

func gatewayFixture(t *testing.T) (*server, *gatewayFake, string) {
	t.Helper()
	root := testRoot(t)
	const size = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	writeTestFile(t, filepath.Join(root, rel), size)
	fake := &gatewayFake{
		inner:    oneFileFake(root, filepath.Join("release", "episode.mkv"), size, 1),
		category: "Shows", size: size,
	}
	return newGatewayServer(t, root, fake), fake, rel
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON %s: %v", recorder.Body.String(), err)
	}
	return out
}

func TestTorrentsExposeShelfItemPath(t *testing.T) {
	s, fake, _ := gatewayFixture(t)
	recorder := httptest.NewRecorder()
	s.handleTorrents(recorder, httptest.NewRequest(http.MethodGet, "/torrents", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := decode(t, recorder)
	if body["ok"] != true || body["qbit_available"] != true {
		t.Fatalf("envelope missing: %v", body)
	}
	list, _ := body["torrents"].([]any)
	if len(list) != 1 {
		t.Fatalf("torrents=%v", list)
	}
	torrent, _ := list[0].(map[string]any)
	if got := torrent["item_path"]; got != "shows/release" {
		t.Fatalf("item_path=%v, want shows/release", got)
	}
	if got := torrent["category"]; got != "Shows" {
		t.Fatalf("category=%v", got)
	}
	if _, ok := torrent["force_start"]; !ok {
		t.Fatalf("force_start missing: %v", torrent)
	}
	_ = fake
}

func TestTorrentOutsideRootHasEmptyItemPath(t *testing.T) {
	s, _, _ := gatewayFixture(t)
	outside := &qbTorrent{Hash: "x", Name: "other", SavePath: "/srv/elsewhere",
		ContentPath: "/srv/elsewhere/other"}
	if got := s.itemPath(outside); got != "" {
		t.Fatalf("item_path=%q for a torrent outside the root, want empty", got)
	}
}

func TestTorrentFilesCarryIdAndShelfPath(t *testing.T) {
	s, fake, rel := gatewayFixture(t)
	recorder := httptest.NewRecorder()
	s.handleTorrent(recorder, httptest.NewRequest(http.MethodGet,
		"/torrent?hash="+fake.inner.hash, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := decode(t, recorder)
	files, _ := body["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files=%v", files)
	}
	file, _ := files[0].(map[string]any)
	if got, ok := file["id"].(float64); !ok || int(got) != 0 {
		t.Fatalf("file id=%v", file["id"])
	}
	if got := file["path"]; got != filepath.ToSlash(rel) {
		t.Fatalf("file path=%v, want %v", got, rel)
	}
	if file["ready"] != true {
		t.Fatalf("complete file is not ready: %v", file)
	}
}

func TestCategoriesReturnedAndErrorIsNotEmptyList(t *testing.T) {
	s, _, _ := gatewayFixture(t)
	recorder := httptest.NewRecorder()
	s.handleCategories(recorder, httptest.NewRequest(http.MethodGet, "/categories", nil))
	body := decode(t, recorder)
	list, _ := body["categories"].([]any)
	if len(list) != 2 {
		t.Fatalf("categories=%v", list)
	}
	first, _ := list[0].(map[string]any)
	if first["name"] != "Movies" {
		t.Fatalf("categories are not sorted: %v", list)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	dead.Close()
	qb, err := newQbit(dead.URL, "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	broken := &server{root: s.root, qb: qb}
	recorder = httptest.NewRecorder()
	broken.handleCategories(recorder, httptest.NewRequest(http.MethodGet, "/categories", nil))
	if recorder.Code == http.StatusOK {
		t.Fatalf("unreachable qBittorrent returned 200: %s", recorder.Body.String())
	}
	if decode(t, recorder)["ok"] != false {
		t.Fatalf("error response without ok=false: %s", recorder.Body.String())
	}
}

func TestRemoveKeepsFilesUnlessExplicitlyAsked(t *testing.T) {
	s, fake, _ := gatewayFixture(t)
	recorder := httptest.NewRecorder()
	s.handleRemove(recorder, httptest.NewRequest(http.MethodPost,
		"/remove?hash="+fake.inner.hash, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := strings.Join(fake.deleteCalls, ","); got != "delete:deleteFiles=false" {
		t.Fatalf("default remove deleted data: %q", got)
	}

	fake.deleteCalls = nil
	recorder = httptest.NewRecorder()
	s.handleRemove(recorder, httptest.NewRequest(http.MethodPost,
		"/remove?hash="+fake.inner.hash+"&delete_files=true", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := strings.Join(fake.deleteCalls, ","); got != "delete:deleteFiles=true" {
		t.Fatalf("explicit remove did not delete data: %q", got)
	}
	if decode(t, recorder)["files_deleted"] != true {
		t.Fatalf("response does not report the data deletion: %s", recorder.Body.String())
	}
}

func TestRemoveWithDataRefusedWhileStreaming(t *testing.T) {
	s, fake, rel := gatewayFixture(t)
	info, err := os.Stat(filepath.Join(s.root, rel))
	if err != nil {
		t.Fatal(err)
	}
	defer s.streamReg().acquire(inodeKey(info))()

	recorder := httptest.NewRecorder()
	s.handleRemove(recorder, httptest.NewRequest(http.MethodPost,
		"/remove?hash="+fake.inner.hash+"&delete_files=true", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", recorder.Code, recorder.Body.String())
	}
	if len(fake.deleteCalls) != 0 {
		t.Fatalf("qBittorrent was called anyway: %v", fake.deleteCalls)
	}
}

func TestStopClearsForceStart(t *testing.T) {
	s, fake, _ := gatewayFixture(t)
	fake.forceStart = true
	fake.inner.state = "uploading"

	recorder := httptest.NewRecorder()
	s.handleStartStop("stop")(recorder, httptest.NewRequest(http.MethodPost,
		"/stop?hash="+fake.inner.hash, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fake.forceStart {
		t.Fatal("force start survived the stop")
	}
}

func TestAddAcceptsOnlyMagnetByDefault(t *testing.T) {
	s, _, _ := gatewayFixture(t)
	for link, wantOK := range map[string]bool{
		"magnet:?xt=urn:btih:abc":       true,
		"http://192.168.1.1/admin":      false,
		"https://example.invalid/x.tor": false,
		"file:///etc/passwd":            false,
		"ftp://example.invalid/x":       false,
	} {
		err := s.checkAddLink(link)
		if (err == nil) != wantOK {
			t.Fatalf("checkAddLink(%q) err=%v, want ok=%v", link, err, wantOK)
		}
	}
	s.allowURLAdd = true
	if err := s.checkAddLink("https://example.invalid/x.torrent"); err != nil {
		t.Fatalf("TORRMEDIA_ALLOW_URL_ADD did not enable http(s): %v", err)
	}

	recorder := httptest.NewRecorder()
	s.allowURLAdd = false
	s.handleAdd(recorder, httptest.NewRequest(http.MethodPost,
		"/add?magnet="+url.QueryEscape("http://10.0.0.1/x")+"&category=Shows", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", recorder.Code, recorder.Body.String())
	}
}

func TestAddFileReadsOnlyFromTorrentDir(t *testing.T) {
	s, _, _ := gatewayFixture(t)
	s.torrentDir = t.TempDir()
	for _, name := range []string{"../config.env", "sub/x.torrent", ".config.env"} {
		recorder := httptest.NewRecorder()
		s.handleAdd(recorder, httptest.NewRequest(http.MethodPost,
			"/add?file="+url.QueryEscape(name), nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("file=%q status=%d body=%s, want 400",
				name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestGatewayMutationsRespectTheCoordinator(t *testing.T) {
	root := testRoot(t)
	const size = int64(1 << 20)
	rel := filepath.Join("shows", "release", "episode.mkv")
	writeTestFile(t, filepath.Join(root, rel), size)
	fake := &gatewayFake{
		inner:    oneFileFake(root, filepath.Join("release", "episode.mkv"), size, 1),
		category: "Shows", size: size,
	}
	s := newGatewayServer(t, root, fake)
	s.deleteDebounce = 2 * time.Second

	go func() {
		req := httptest.NewRequest(http.MethodDelete, "/file?path="+url.QueryEscape(rel), nil)
		s.handleDelete(httptest.NewRecorder(), req)
	}()
	waitForDeleteBatchSize(t, s, fake.inner.hash, 1)

	recorder := httptest.NewRecorder()
	s.handleRemove(recorder, httptest.NewRequest(http.MethodPost,
		"/remove?hash="+fake.inner.hash+"&delete_files=true", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("remove during a queued batch: status=%d body=%s, want 409",
			recorder.Code, recorder.Body.String())
	}
	if len(fake.deleteCalls) != 0 {
		t.Fatalf("qBittorrent was mutated during a queued batch: %v", fake.deleteCalls)
	}
}
