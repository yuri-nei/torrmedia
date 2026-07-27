package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func decodeJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad JSON %s: %v", body, err)
	}
	return out
}

func TestHealthReportsContract(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	got := decodeJSON[healthStatus](t, rec.Body.Bytes())
	if got.Contract != contractVersion || got.Version != version {
		t.Fatalf("health=%+v", got)
	}
}

func TestItemsExposeNoInternals(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodGet, "/items", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	list := decodeJSON[apiItemList](t, rec.Body.Bytes())

	body := rec.Body.String()
	if list.Items[0].Magnet != nil {
		encoded, err := json.Marshal(*list.Items[0].Magnet)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.ReplaceAll(body, strings.Trim(string(encoded), `"`), "<magnet>")
	}
	for _, leak := range []string{
		env.fake.torrents[0].hash, env.root, "stoppedUP", "qbit_file_id", "torrent_hash",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaks %q outside the magnet: %s", leak, body)
		}
	}
	if len(list.Items) != 1 || list.Degraded != nil {
		t.Fatalf("list=%+v", list)
	}
	item := list.Items[0]
	if item.Status != statusStopped || item.FilesTotal != 2 || item.FilesReady != 2 {
		t.Fatalf("item=%+v", item)
	}
	if item.Magnet == nil || !strings.HasPrefix(*item.Magnet, "magnet:?") {
		t.Fatalf("magnet=%v", item.Magnet)
	}
	if item.FilesWatched != 0 {
		t.Fatalf("files_watched=%d", item.FilesWatched)
	}
}

func TestItemDetailAndStream(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)

	rec := env.do(t, http.MethodGet, "/items/"+item.id, "")
	detail := decodeJSON[apiItemDetail](t, rec.Body.Bytes())
	if len(detail.Files) != 2 {
		t.Fatalf("files=%d", len(detail.Files))
	}

	file := detail.Files[0]
	if !contains(file.Actions, actionPlay) {
		t.Fatalf("ready file cannot be played: %+v", file)
	}
	url := "/items/" + item.id + "/files/" + file.ID + "/stream/" + file.Name
	rec = env.do(t, http.MethodGet, url, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" || rec.Body.Len() == 0 {
		t.Fatalf("stream headers=%v len=%d", rec.Header(), rec.Body.Len())
	}
}

func TestUnknownResourcesAndMethods(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)

	for _, c := range []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/items/not-an-id", http.StatusBadRequest, codeBadRequest},
		{http.MethodGet, "/items/" + pathID("nope"), http.StatusNotFound, codeNotFound},
		{http.MethodDelete, "/items/" + item.id + "/files/" + fileID("nope"),
			http.StatusNotFound, codeNotFound},
		{http.MethodGet, "/operations/op_missing", http.StatusNotFound, codeNotFound},
	} {
		rec := env.do(t, c.method, c.path, "")
		if rec.Code != c.status {
			t.Fatalf("%s %s status=%d body=%s, want %d", c.method, c.path, rec.Code, rec.Body, c.status)
		}
		got := decodeJSON[apiError](t, rec.Body.Bytes())
		if got.Code != c.code || got.Error == "" {
			t.Fatalf("%s %s error=%+v, want code %q", c.method, c.path, got, c.code)
		}
	}

	rec := env.do(t, http.MethodPut, "/items", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Fatalf("status=%d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestMutationReturnsOperation(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)

	rec := env.do(t, http.MethodPost, "/items/"+item.id+"/start", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	op := decodeJSON[operation](t, rec.Body.Bytes())
	if op.ID == "" || op.Kind != "item.start" || op.Item != item.id {
		t.Fatalf("operation=%+v", op)
	}
	if got := rec.Header().Get("Location"); got != "/operations/"+op.ID {
		t.Fatalf("Location=%q", got)
	}

	done := env.awaitOperation(t, op.ID)
	if done.State != opDone {
		t.Fatalf("operation failed: %+v", done)
	}
	if done.FinishedAt == nil || done.Phase != nil {
		t.Fatalf("finished operation=%+v", done)
	}
	rec = env.do(t, http.MethodGet, "/operations/"+op.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestViewedIsImmediate(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	file := item.files[0]

	base := "/items/" + item.id + "/files/" + file.id + "/viewed"
	if rec := env.do(t, http.MethodPut, base, `{"state":"watched"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", rec.Code, rec.Body)
	}
	_, item = env.firstItem(t)
	if item.files[0].watch != watchWatched {
		t.Fatalf("watch=%q", item.files[0].watch)
	}

	if rec := env.do(t, http.MethodDelete, base, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d", rec.Code)
	}
	_, item = env.firstItem(t)
	if item.files[0].watch != "" {
		t.Fatalf("watch survived the clear: %q", item.files[0].watch)
	}
}

func TestAddAcceptsOnlyMagnet(t *testing.T) {
	env := newTestEnv(t)
	for body, want := range map[string]int{
		`{"magnet":"magnet:?xt=urn:btih:abc","category":"Shows"}`: http.StatusAccepted,
		`{"magnet":"http://10.0.0.1/x.torrent"}`:                  http.StatusForbidden,
		`{"magnet":"file:///etc/passwd"}`:                         http.StatusForbidden,
		`{"magnet":"magnet:?xt=urn:btih:abc","category":"a/b"}`:   http.StatusBadRequest,
	} {
		rec := env.do(t, http.MethodPost, "/items", body)
		if rec.Code != want {
			t.Fatalf("body=%s status=%d, want %d (%s)", body, rec.Code, want, rec.Body)
		}
	}
}

func TestTokenGuardsEverythingButHealth(t *testing.T) {
	env := newTestEnv(t)
	env.token = "secret"

	if rec := env.do(t, http.MethodGet, "/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("health with token configured: %d", rec.Code)
	}
	rec := env.do(t, http.MethodGet, "/items", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("items without token: %d", rec.Code)
	}
	if decodeJSON[apiError](t, rec.Body.Bytes()).Code != codeUnauthorized {
		t.Fatalf("body=%s", rec.Body)
	}
}

func TestCategoriesErrorIsNotAnEmptyList(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodGet, "/categories", "")
	got := decodeJSON[struct {
		Categories []struct {
			Name string `json:"name"`
		} `json:"categories"`
	}](t, rec.Body.Bytes())
	if len(got.Categories) != 2 || got.Categories[0].Name != "Movies" {
		t.Fatalf("categories=%+v", got)
	}

	env.fake.mu.Lock()
	env.fake.failInfo = true
	env.fake.mu.Unlock()
	env.qb.base.Host = "127.0.0.1:1"
	rec = env.do(t, http.MethodGet, "/categories", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("unreachable qBittorrent returned 200: %s", rec.Body)
	}
	if decodeJSON[apiError](t, rec.Body.Bytes()).Code != codeQbitUnavailable {
		t.Fatalf("body=%s", rec.Body)
	}
}

func TestAddOperationIsVisibleInTheListStream(t *testing.T) {
	env := newTestEnv(t)
	sub := env.hub().subscribe(topicList)
	defer env.hub().unsubscribe(sub)

	rec := env.do(t, http.MethodPost, "/items", `{"magnet":"magnet:?xt=urn:btih:abc"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	op := decodeJSON[operation](t, rec.Body.Bytes())
	if op.Item != "" {
		t.Fatalf("add operation is bound to item %q", op.Item)
	}
	env.awaitOperation(t, op.ID)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case frame := <-sub.ch:
			if frame.event != eventOperation {
				continue
			}
			if got := decodeJSON[operation](t, frame.data); got.ID == op.ID {
				return
			}
		case <-deadline:
			t.Fatal("add operation never appeared in the list stream")
		}
	}
}
