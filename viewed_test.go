package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func testViewedStore(t *testing.T) *viewedStore {
	t.Helper()
	store, err := newViewedStore(filepath.Join(t.TempDir(), "viewed.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testPlaybackConfig() playbackConfig {
	return playbackConfig{
		watchedFraction:     0.70,
		endFraction:         0.90,
		startedBytes:        100,
		startedFraction:     0.003,
		minPlayChunk:        80,
		manualResetFraction: 0.05,
		flushInterval:       time.Hour,
	}
}

func request(t *testing.T, handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestFileAliasHeadETagAndRange(t *testing.T) {
	root := testRoot(t)
	rel := filepath.Join("shows", "release", "эпизод 1.mkv")
	body := writeTestFile(t, filepath.Join(root, rel), 4096)
	handler := (&server{root: root}).routes()
	query := "?path=" + url.QueryEscape(rel)

	short := request(t, handler, http.MethodGet, "/file"+query, map[string]string{"Range": "bytes=20-119"})
	alias := request(t, handler, http.MethodGet, "/file/decorative-name.mkv"+query, map[string]string{"Range": "bytes=20-119"})
	for _, rec := range []*httptest.ResponseRecorder{short, alias} {
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("Range status=%d body=%s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Range"); got != "bytes 20-119/4096" {
			t.Fatalf("Content-Range=%q", got)
		}
		if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Fatalf("Accept-Ranges=%q", got)
		}
		if got := rec.Header().Get("Content-Type"); got != "video/x-matroska" {
			t.Fatalf("Content-Type=%q", got)
		}
		if got := rec.Header().Get("ETag"); got == "" {
			t.Fatal("ETag is empty")
		}
		if got := rec.Body.Bytes(); string(got) != string(body[20:120]) {
			t.Fatal("Range body differs from file")
		}
	}
	if short.Header().Get("ETag") != alias.Header().Get("ETag") {
		t.Fatalf("short ETag=%q alias ETag=%q", short.Header().Get("ETag"), alias.Header().Get("ETag"))
	}

	head := request(t, handler, http.MethodHead, "/file/ignored-basename.avi"+query, nil)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d body=%s", head.Code, head.Body.String())
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD returned %d body bytes", head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD Content-Length=%q", got)
	}

	notModified := request(t, handler, http.MethodGet, "/file/anything"+query,
		map[string]string{"If-None-Match": alias.Header().Get("ETag")})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("If-None-Match status=%d body=%q", notModified.Code, notModified.Body.String())
	}

	deleteAlias := request(t, handler, http.MethodDelete, "/file/anything"+query, nil)
	if deleteAlias.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE alias status=%d body=%s", deleteAlias.Code, deleteAlias.Body.String())
	}
}

func TestPlaybackTrackingThresholdsAndListProjection(t *testing.T) {
	root := testRoot(t)
	rel := filepath.Join("shows", "release", "episode.mkv")
	writeTestFile(t, filepath.Join(root, rel), 1000)
	store := testViewedStore(t)
	s := &server{root: root, viewed: store, playback: testPlaybackConfig()}
	handler := s.routes()
	target := "/file/episode.mkv?path=" + url.QueryEscape(rel)

	probe := request(t, handler, http.MethodGet, target, map[string]string{"Range": "bytes=0-49"})
	if probe.Code != http.StatusPartialContent {
		t.Fatalf("probe status=%d body=%s", probe.Code, probe.Body.String())
	}
	if entries := store.list(rel, ""); len(entries) != 0 {
		t.Fatalf("probe created entries: %+v", entries)
	}

	request(t, handler, http.MethodGet, target, map[string]string{"Range": "bytes=50-169"})
	entry := store.list(rel, "")[0]
	if entry.State != "started" || entry.Served != 120 || entry.Pos != 170 {
		t.Fatalf("started entry=%+v", entry)
	}

	request(t, handler, http.MethodGet, target, map[string]string{"Range": "bytes=900-979"})
	entry = store.list(rel, "")[0]
	if entry.State != "watched" || entry.Pos != 980 {
		t.Fatalf("watched entry=%+v", entry)
	}

	list := request(t, handler, http.MethodGet, "/list?path="+url.QueryEscape(filepath.Join("shows", "release")), nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var response struct {
		Items []item `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || len(response.Items[0].Files) != 1 {
		t.Fatalf("list response=%+v", response)
	}
	file := response.Items[0].Files[0]
	if file.Watch != "watched" || file.Pos != 980 {
		t.Fatalf("list watch projection=%+v", file)
	}
}

func TestManualViewedOverridesAndPersists(t *testing.T) {
	root := testRoot(t)
	rel := filepath.Join("shows", "release", "episode.mkv")
	writeTestFile(t, filepath.Join(root, rel), 1000)
	storePath := filepath.Join(t.TempDir(), "viewed.json")
	store, err := newViewedStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{root: root, viewed: store, playback: testPlaybackConfig()}
	handler := s.routes()
	pathQuery := "path=" + url.QueryEscape(rel)

	set := request(t, handler, http.MethodPost, "/viewed?"+pathQuery+"&state=started", nil)
	if set.Code != http.StatusOK {
		t.Fatalf("POST viewed status=%d body=%s", set.Code, set.Body.String())
	}
	entry := store.list(rel, "")[0]
	if entry.State != "started" || !entry.Manual || entry.Size != 1000 {
		t.Fatalf("manual started entry=%+v", entry)
	}

	remove := request(t, handler, http.MethodDelete, "/viewed?"+pathQuery, nil)
	if remove.Code != http.StatusOK {
		t.Fatalf("DELETE viewed status=%d body=%s", remove.Code, remove.Body.String())
	}
	entry = store.list(rel, "")[0]
	if entry.State != "" || !entry.Manual || entry.Served != 0 || entry.Pos != 0 {
		t.Fatalf("manual reset entry=%+v", entry)
	}

	target := "/file/episode.mkv?" + pathQuery
	request(t, handler, http.MethodGet, target, map[string]string{"Range": "bytes=0-39"})
	entry = store.list(rel, "")[0]
	if entry.State != "" || !entry.Manual {
		t.Fatalf("less than reset threshold overrode manual reset: %+v", entry)
	}
	request(t, handler, http.MethodGet, target, map[string]string{"Range": "bytes=40-109"})
	entry = store.list(rel, "")[0]
	if entry.State != "started" || entry.Manual || entry.Served != 110 {
		t.Fatalf("re-watch did not return control to auto tracking: %+v", entry)
	}

	reloaded, err := newViewedStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	entry = reloaded.list(rel, "")[0]
	if entry.State != "started" || entry.Served != 110 {
		t.Fatalf("reloaded entry=%+v", entry)
	}
}

func TestDeleteViewedItemPurgesAllStatuses(t *testing.T) {
	root := testRoot(t)
	itemPath := filepath.Join("shows", "release")
	rels := []string{
		filepath.Join(itemPath, "episode-1.mkv"),
		filepath.Join(itemPath, "episode-2.mkv"),
	}
	for _, rel := range rels {
		writeTestFile(t, filepath.Join(root, rel), 1000)
	}
	store := testViewedStore(t)
	s := &server{root: root, viewed: store, playback: testPlaybackConfig()}
	handler := s.routes()
	request(t, handler, http.MethodPost, "/viewed?path="+url.QueryEscape(rels[0]), nil)
	request(t, handler, http.MethodPost, "/viewed?path="+url.QueryEscape(rels[1])+"&state=started", nil)

	purge := request(t, handler, http.MethodDelete, "/viewed?item="+url.QueryEscape(itemPath), nil)
	if purge.Code != http.StatusOK {
		t.Fatalf("DELETE viewed item status=%d body=%s", purge.Code, purge.Body.String())
	}
	entries := store.list("", itemPath)
	if len(entries) != 0 {
		t.Fatalf("item purge left entries=%+v", entries)
	}
}

func TestDeleteFileKeepsViewedStatus(t *testing.T) {
	root := testRoot(t)
	rel := filepath.Join("movies", "watched.mkv")
	writeTestFile(t, filepath.Join(root, rel), 1000)
	store := testViewedStore(t)
	s := &server{root: root, viewed: store, playback: testPlaybackConfig()}
	handler := s.routes()
	request(t, handler, http.MethodPost, "/viewed?path="+url.QueryEscape(rel), nil)

	deleted := request(t, handler, http.MethodDelete, "/file?path="+url.QueryEscape(rel), nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("DELETE file status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	entries := store.list(rel, "")
	if len(entries) != 1 || entries[0].State != "watched" {
		t.Fatalf("file deletion changed viewed status: %+v", entries)
	}
}

func TestConcurrentPlaybackUpdatesDoNotLoseBytes(t *testing.T) {
	store := testViewedStore(t)
	s := &server{viewed: store, playback: testPlaybackConfig()}
	if err := s.trackPlayback("shows/release/episode.mkv", "shows/release", 1000, 0, 100, 100); err != nil {
		t.Fatal(err)
	}
	const (
		workers = 8
		chunk   = 25
	)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.trackPlayback("shows/release/episode.mkv", "shows/release", 1000, 0, chunk, chunk); err != nil {
				t.Errorf("trackPlayback: %v", err)
			}
		}()
	}
	wg.Wait()
	entry := store.list("shows/release/episode.mkv", "")[0]
	if entry.Served != 100+workers*chunk || entry.State != "started" {
		t.Fatalf("concurrent entry=%+v", entry)
	}
}

func TestPlaybackTrackerFlushesBeforeRequestFinishes(t *testing.T) {
	store := testViewedStore(t)
	cfg := testPlaybackConfig()
	cfg.flushInterval = 5 * time.Millisecond
	s := &server{viewed: store, playback: cfg}
	tracker := newPlaybackTracker(s, "shows/release/episode.mkv", 1000, 200)
	tracker.total.Add(120)

	deadline := time.Now().Add(time.Second)
	for len(store.list("shows/release/episode.mkv", "")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("periodic flush did not persist playback")
		}
		time.Sleep(time.Millisecond)
	}
	tracker.total.Add(30)
	tracker.finish()

	entry := store.list("shows/release/episode.mkv", "")[0]
	if entry.Served != 150 || entry.Pos != 350 {
		t.Fatalf("periodic/final flush double-counted or lost bytes: %+v", entry)
	}
}
