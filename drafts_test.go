package main

import (
	"bufio"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const draftMagnet = "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01&dn=Series"

func draftEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.addHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	env.fake.addName = "Series"
	env.fake.addSavePath = filepath.Join(env.root, "shows")
	env.fake.addFiles = []fakeFile{
		{name: filepath.Join("Series", "s01e01.mkv"), size: 3 << 30, priority: 1},
		{name: filepath.Join("Series", "s01e02.mkv"), size: 3 << 30, priority: 1},
		{name: filepath.Join("Series", "extras", "sample.mkv"), size: 1 << 20, priority: 1},
	}
	env.fake.metadataIn = 20 * time.Millisecond
	env.fake.mu.Unlock()
	return env
}

func (e *testEnv) createDraft(t *testing.T, body string) apiDraft {
	t.Helper()
	rec := e.do(t, "POST", "/drafts", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /drafts = %d %s", rec.Code, rec.Body.String())
	}
	var d apiDraft
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Status != draftResolving {
		t.Fatalf("fresh draft is %q", d.Status)
	}
	return d
}

func (e *testEnv) awaitDraft(t *testing.T, id string, want ...string) apiDraft {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := e.do(t, "GET", "/drafts/"+id, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /drafts/%s = %d %s", id, rec.Code, rec.Body.String())
		}
		var d apiDraft
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		for _, status := range want {
			if d.Status == status {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("draft stuck in %q (error=%v), wanted %v", d.Status, d.Error, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDraftConfirmAppliesSelection(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	ready := env.awaitDraft(t, created.ID, draftReady)

	if len(ready.Files) != 3 {
		t.Fatalf("files=%+v", ready.Files)
	}
	if ready.Title != "Series" || ready.Size != (3<<30)+(3<<30)+(1<<20) {
		t.Fatalf("draft=%+v", ready)
	}
	for _, f := range ready.Files {
		if !f.Wanted {
			t.Fatalf("file %q is not preselected", f.Name)
		}
	}
	var paths []string
	byPath := map[string]string{}
	for _, f := range ready.Files {
		paths = append(paths, f.Path)
		byPath[f.Path] = f.ID
	}
	if got := strings.Join(paths, " "); got != "extras/sample.mkv s01e01.mkv s01e02.mkv" {
		t.Fatalf("paths=%q", got)
	}
	if ready.Storage.Free == nil || *ready.Storage.Free != 100<<30 {
		t.Fatalf("storage=%+v", ready.Storage)
	}

	var list apiItemList
	if err := json.Unmarshal(env.do(t, "GET", "/items", "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.Title == "Series" {
			t.Fatal("draft leaked into the catalog")
		}
	}

	body := `{"files":["` + byPath["s01e01.mkv"] + `","` + byPath["s01e02.mkv"] + `"],"category":"Shows"}`
	rec := env.do(t, "POST", "/drafts/"+created.ID+"/confirm", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	confirmed := env.awaitDraft(t, created.ID, draftConfirmed, draftFailed)
	if confirmed.Status != draftConfirmed {
		t.Fatalf("confirm failed: %v", confirmed.Error)
	}
	if confirmed.Item == nil || *confirmed.Item == "" {
		t.Fatal("confirmed draft has no item id")
	}

	calls := strings.Join(env.fake.snapshotCalls(), " ")
	if !strings.Contains(calls, "add:stopCondition=MetadataReceived") {
		t.Fatalf("add without stop condition: %s", calls)
	}
	if !strings.Contains(calls, "prio0:2") {
		t.Fatalf("unselected file was not excluded: %s", calls)
	}
	if strings.Contains(calls, "prio0:0") || strings.Contains(calls, "prio0:1") {
		t.Fatalf("selected files were excluded: %s", calls)
	}
	if !strings.Contains(calls, "removeTags:") || !strings.Contains(calls, "setCategory:Shows") {
		t.Fatalf("draft was not turned into a normal item: %s", calls)
	}

	if err := json.Unmarshal(env.do(t, "GET", "/items", "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range list.Items {
		if item.Title == "Series" {
			found = true
		}
	}
	if !found {
		t.Fatal("confirmed item is missing from the catalog")
	}
}

func TestDraftDiscardDeletesData(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	env.awaitDraft(t, created.ID, draftReady)

	rec := env.do(t, "DELETE", "/drafts/"+created.ID, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body.String())
	}
	env.awaitDraft(t, created.ID, draftDiscarded, draftFailed)

	calls := strings.Join(env.fake.snapshotCalls(), " ")
	if !strings.Contains(calls, "delete:deleteFiles=true") {
		t.Fatalf("draft data was left behind: %s", calls)
	}
}

func TestDraftDiscardRefusesUntaggedTorrent(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	env.awaitDraft(t, created.ID, draftReady)

	env.fake.mu.Lock()
	env.fake.torrents[len(env.fake.torrents)-1].tags = nil
	env.fake.mu.Unlock()

	env.do(t, "DELETE", "/drafts/"+created.ID, "")
	failed := env.awaitDraft(t, created.ID, draftFailed, draftDiscarded)
	if failed.Status != draftFailed {
		t.Fatalf("untagged torrent was discarded anyway: %+v", failed)
	}
	if calls := strings.Join(env.fake.snapshotCalls(), " "); strings.Contains(calls, "delete:") {
		t.Fatalf("data of a foreign torrent was deleted: %s", calls)
	}
}

func TestDraftRefusesTorrentAlreadyInLibrary(t *testing.T) {
	env := draftEnv(t)
	rec := env.do(t, "POST", "/drafts",
		`{"magnet":"magnet:?xt=urn:btih:0123456789012345678901234567890123456789"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /drafts = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already in the library") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if calls := strings.Join(env.fake.snapshotCalls(), " "); strings.Contains(calls, "add:") {
		t.Fatalf("duplicate was added anyway: %s", calls)
	}
}

func TestDraftRejectsAdoptedExistingTorrent(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	env.fake.torrents[0].addedOn = time.Now().Unix() - 3600
	env.fake.mu.Unlock()

	created := env.createDraft(t, `{"magnet":"magnet:?xt=urn:btmh:1220ff&dn=v2"}`)

	d, ok := env.draftStore().get(created.ID)
	if !ok {
		t.Fatal("draft disappeared")
	}
	env.fake.mu.Lock()
	env.fake.torrents[0].tags = []string{draftTag, d.tag}
	env.fake.mu.Unlock()

	failed := env.awaitDraft(t, created.ID, draftFailed)
	if failed.Error == nil || !strings.Contains(*failed.Error, "already in the library") {
		t.Fatalf("draft=%+v", failed)
	}
	env.fake.mu.Lock()
	tags := append([]string(nil), env.fake.torrents[0].tags...)
	env.fake.mu.Unlock()
	if len(tags) != 0 {
		t.Fatalf("tags were left on a foreign torrent: %v", tags)
	}
	if calls := strings.Join(env.fake.snapshotCalls(), " "); strings.Contains(calls, "delete:") {
		t.Fatalf("foreign torrent was deleted: %s", calls)
	}
}

func TestDraftConfirmWithoutSelectionAddsStopped(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	env.awaitDraft(t, created.ID, draftReady)

	rec := env.do(t, "POST", "/drafts/"+created.ID+"/confirm", `{"files":[]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	done := env.awaitDraft(t, created.ID, draftConfirmed, draftFailed)
	if done.Status != draftConfirmed {
		t.Fatalf("confirm failed: %v", done.Error)
	}

	var item apiItemDetail
	body := env.do(t, "GET", "/items/"+*done.Item, "").Body.Bytes()
	if err := json.Unmarshal(body, &item); err != nil {
		t.Fatal(err)
	}
	for _, f := range item.Files {
		if f.Status != fileSkipped {
			t.Fatalf("file %q is %q, want skipped", f.Name, f.Status)
		}
	}
	if item.Status != statusStopped {
		t.Fatalf("item status=%q, want stopped", item.Status)
	}
	for _, call := range env.fake.snapshotCalls() {
		if call == "start" {
			t.Fatal("torrent with nothing selected was started")
		}
	}
}

func TestDraftConfirmRejectsUnknownFile(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	env.awaitDraft(t, created.ID, draftReady)

	rec := env.do(t, "POST", "/drafts/"+created.ID+"/confirm", `{"files":["bm9wZQ"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("confirm with unknown file = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDraftDoesNotContestExistingItem(t *testing.T) {
	env := newTestEnv(t)
	env.fake.mu.Lock()
	existing := env.fake.torrents[0]
	env.fake.torrents = append(env.fake.torrents, &fakeTorrent{
		hash: "ffffffffffffffffffffffffffffffffffffffff", name: existing.name,
		savePath: existing.savePath, state: "stoppedDL",
		tags: []string{draftTag, draftTag + "-1234"}, addedOn: time.Now().Unix(),
		files: existing.files,
	})
	env.fake.mu.Unlock()

	cat, item := env.firstItem(t)
	if len(cat.items) != 1 {
		t.Fatalf("draft is visible in the catalog: %d items", len(cat.items))
	}
	if item.ambiguous {
		t.Fatal("draft made a real item ambiguous")
	}
}

func TestSweepRemovesOrphanedDrafts(t *testing.T) {
	env := newTestEnv(t)
	env.draftTTL = time.Minute
	env.fake.mu.Lock()
	env.fake.torrents = append(env.fake.torrents,
		&fakeTorrent{
			hash: "1111111111111111111111111111111111111111", name: "Stale",
			savePath: filepath.Join(env.root, "shows"), state: "stoppedDL",
			tags: []string{draftTag, draftTag + "-stale"}, addedOn: time.Now().Unix() - 3600,
		},
		&fakeTorrent{
			hash: "2222222222222222222222222222222222222222", name: "Fresh",
			savePath: filepath.Join(env.root, "shows"), state: "stoppedDL",
			tags: []string{draftTag, draftTag + "-fresh"}, addedOn: time.Now().Unix(),
		})
	env.fake.mu.Unlock()

	env.sweepDrafts()

	env.fake.mu.Lock()
	defer env.fake.mu.Unlock()
	for _, torrent := range env.fake.torrents {
		if torrent.name == "Stale" {
			t.Fatal("stale draft survived the sweep")
		}
	}
	found := false
	for _, torrent := range env.fake.torrents {
		if torrent.name == "Fresh" {
			found = true
		}
	}
	if !found {
		t.Fatal("fresh draft was swept away")
	}
}

func TestDraftEventsDeliverReadyState(t *testing.T) {
	env := draftEnv(t)
	env.fake.mu.Lock()
	env.fake.metadataIn = 150 * time.Millisecond
	env.fake.mu.Unlock()
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)

	srv := httptest.NewServer(env.routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/drafts/" + created.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ready := awaitDraftFrame(t, bufio.NewReader(resp.Body), draftReady, 5*time.Second)
	if len(ready.Files) != 3 || ready.Title != "Series" {
		t.Fatalf("ready frame=%+v", ready)
	}
}

func awaitDraftFrame(t *testing.T, body *bufio.Reader, status string, timeout time.Duration) apiDraft {
	t.Helper()
	type result struct {
		draft apiDraft
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		var event string
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == eventState:
				var d apiDraft
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d); err != nil {
					ch <- result{err: err}
					return
				}
				if d.Status == status {
					ch <- result{draft: d}
					return
				}
			}
		}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read draft stream: %v", got.err)
		}
		return got.draft
	case <-time.After(timeout):
		t.Fatalf("no %q frame within %s", status, timeout)
		return apiDraft{}
	}
}

func TestStorageReportsFreeSpace(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, "GET", "/storage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /storage = %d", rec.Code)
	}
	var storage apiStorage
	if err := json.Unmarshal(rec.Body.Bytes(), &storage); err != nil {
		t.Fatal(err)
	}
	if storage.Free == nil || *storage.Free != 100<<30 ||
		storage.Total == nil || *storage.Total != 200<<30 ||
		storage.Reserve != 512<<20 {
		t.Fatalf("storage=%+v", storage)
	}
}

func TestMagnetInfoHash(t *testing.T) {
	const hexHash = "0123456789abcdef0123456789abcdef01234567"
	raw, err := hex.DecodeString(hexHash)
	if err != nil {
		t.Fatal(err)
	}
	base32Hash := base32.StdEncoding.EncodeToString(raw)

	cases := map[string]string{
		"magnet:?xt=urn:btih:0123456789ABCDEF0123456789abcdef01234567": hexHash,
		"magnet:?dn=x&xt=urn:btih:" + base32Hash:                       hexHash,
		"magnet:?xt=urn:btmh:1220aabb&dn=v2":                           "",
		"magnet:?xt=urn:btih:tooshort":                                 "",
		"https://example.invalid/file.torrent":                         "",
	}
	for magnet, want := range cases {
		if got := magnetInfoHash(magnet); got != want {
			t.Errorf("magnetInfoHash(%q) = %q, want %q", magnet, got, want)
		}
	}
}

func TestDraftFilesFollowCatalogOrder(t *testing.T) {
	env := draftEnv(t)
	env.fake.addFiles = []fakeFile{
		{name: filepath.Join("Series", "s01e03.mkv"), size: 1 << 20, priority: 1},
		{name: filepath.Join("Series", "s01e01.mkv"), size: 1 << 20, priority: 1},
		{name: filepath.Join("Series", "s01e02.mkv"), size: 1 << 20, priority: 1},
	}
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	ready := env.awaitDraft(t, created.ID, draftReady)

	var ids, paths []string
	for _, f := range ready.Files {
		ids = append(ids, f.ID)
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, " "); got != "s01e01.mkv s01e02.mkv s01e03.mkv" {
		t.Fatalf("draft order=%q", got)
	}

	body := `{"files":["` + strings.Join(ids, `","`) + `"]}`
	if rec := env.do(t, "POST", "/drafts/"+created.ID+"/confirm", body); rec.Code != http.StatusAccepted {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	confirmed := env.awaitDraft(t, created.ID, draftConfirmed, draftFailed)
	if confirmed.Status != draftConfirmed || confirmed.Item == nil {
		t.Fatalf("confirm failed: %+v", confirmed)
	}
	detail := decodeJSON[apiItemDetail](t, env.do(t, "GET", "/items/"+*confirmed.Item, "").Body.Bytes())
	var after []string
	for _, f := range detail.Files {
		after = append(after, f.ID)
	}
	if strings.Join(after, " ") != strings.Join(ids, " ") {
		t.Fatalf("item order %v differs from draft order %v", after, ids)
	}
}
