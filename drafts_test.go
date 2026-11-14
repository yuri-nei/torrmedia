package main

// drafts_test.go covers the "pick episodes" window: from a magnet to a
// running torrent.
//
// What is guarded here above all is not convenience but data safety: a draft
// is deleted together with its files, so every path by which a draft's hash
// could point at someone else's torrent is covered by a separate test.

import (
	"bufio"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const draftMagnet = "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01&dn=Series"

// draftEnv is a server where adding a magnet creates a torrent of three
// episodes.
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

// awaitDraft waits for the wanted draft status via the public endpoint.
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

// The full pass: the file list arrives, an unwanted file is deselected, the
// torrent starts.
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
	// Paths inside the torrent are the same ones the catalog will see later,
	// and in the same order, not in .torrent index order.
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

	// The draft is not visible in the catalog: there is no item yet.
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

	// The tags are removed, so the item has appeared in the catalog.
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

// Discarding removes both the torrent and whatever got downloaded while the
// selection was in progress.
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

// If the tag is gone, the torrent is no longer ours: its data must not be
// deleted.
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

// A magnet of an already downloaded torrent is rejected before adding:
// otherwise qBittorrent would put our tags on the existing torrent, and
// discarding would take its files with it.
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

// The same case, but the infohash could not be parsed from the magnet (a v2
// torrent): latching onto an existing torrent is caught by its add time.
func TestDraftRejectsAdoptedExistingTorrent(t *testing.T) {
	env := newTestEnv(t)
	// A torrent added long ago: that is exactly the kind qBittorrent sticks
	// the tags onto if the magnet turns out to be a duplicate.
	env.fake.mu.Lock()
	env.fake.torrents[0].addedOn = time.Now().Unix() - 3600
	env.fake.mu.Unlock()

	// The infohash of a v2 torrent cannot be parsed from the magnet, so the
	// duplicate is not visible before adding; only the time check is left.
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

// An empty selection means "keep the torrent for later": it is added with
// all priorities cleared and stays stopped. A torrent with nothing selected
// must not be started: it would be seeding nothing.
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

// A confirm that doesn't mention a category keeps the one given at creation.
// It used to clear it, and with automatic torrent management the torrent then
// moved to the root of the library. A category given on confirm replaces it;
// an empty one means "no category", as it does on POST /drafts.
func TestDraftConfirmCategory(t *testing.T) {
	cases := []struct {
		name    string
		member  string // the category member of the confirm body
		want    string
		changed bool // whether the torrent's category is set in qBittorrent
	}{
		{"omitted keeps it", ``, "Shows", false},
		{"null keeps it", `,"category":null`, "Shows", false},
		{"given replaces it", `,"category":"Movies"`, "Movies", true},
		{"empty clears it", `,"category":""`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := draftEnv(t)
			created := env.createDraft(t, `{"magnet":"`+draftMagnet+`","category":"Shows"}`)
			ready := env.awaitDraft(t, created.ID, draftReady)
			if ready.Category == nil || *ready.Category != "Shows" {
				t.Fatalf("draft category=%v", ready.Category)
			}

			body := `{"files":["` + ready.Files[0].ID + `"]` + tc.member + `}`
			if rec := env.do(t, "POST", "/drafts/"+created.ID+"/confirm", body); rec.Code != http.StatusAccepted {
				t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
			}
			done := env.awaitDraft(t, created.ID, draftConfirmed, draftFailed)
			env.inflight.Wait()
			if done.Status != draftConfirmed {
				t.Fatalf("confirm failed: %v", done.Error)
			}
			got := ""
			if done.Category != nil {
				got = *done.Category
			}
			env.fake.mu.Lock()
			inQbit := env.fake.find(env.fake.addHash).category
			env.fake.mu.Unlock()
			if got != tc.want || inQbit != tc.want {
				t.Fatalf("category: draft %q, qbittorrent %q, want %q", got, inQbit, tc.want)
			}
			setCategory := func(call string) bool { return strings.HasPrefix(call, "setCategory:") }
			if env.hasCall(setCategory) != tc.changed {
				t.Fatalf("calls=%v", env.fake.snapshotCalls())
			}
		})
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

// A draft must not make a real item with the same files ambiguous:
// otherwise adding an already downloaded series would strip it of all
// actions.
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

// An abandoned draft from a previous run is swept on timeout; a fresh one
// is not.
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

// The selection window lives on the draft's stream: a connecting client gets
// the state immediately, and the file list's arrival comes as the next
// frame.
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

// awaitDraftFrame reads the draft's stream until the wanted status arrives.
// The number of frames before it is not fixed: the client connects at an
// arbitrary moment, and the snapshot on connect may catch any stage.
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
		"magnet:?xt=urn:btmh:1220aabb&dn=v2":                           "", // v2 torrent
		"magnet:?xt=urn:btih:tooshort":                                 "",
		"https://example.invalid/file.torrent":                         "",
	}
	for magnet, want := range cases {
		if got := magnetInfoHash(magnet); got != want {
			t.Errorf("magnetInfoHash(%q) = %q, want %q", magnet, got, want)
		}
	}
}

// The selection window shows the files in the same order as the item screen
// after adding. In some releases' .torrent the episodes are shuffled, and
// the window used to repeat qBittorrent's index order as is.
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

// --- concurrent lifecycle ---
//
// A draft is touched by several parties at once: the resolver, the confirm
// and delete handlers, the sweeper. The tests below hold one of them at a
// known point and let another one in, instead of hoping that timing will
// line them up.

// qbitGate stands between the server and the fake qBittorrent and holds the
// requests it matches until the test opens it.
type qbitGate struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *qbitGate) open() { g.once.Do(func() { close(g.release) }) }

// await waits until a held request has reached the gate.
func (g *qbitGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the qbittorrent gate")
	}
}

// gateQbit routes the server's qBittorrent calls through a gate holding the
// requests match picks.
func (e *testEnv) gateQbit(t *testing.T, match func(*http.Request) bool) *qbitGate {
	t.Helper()
	g := &qbitGate{reached: make(chan struct{}, 1), release: make(chan struct{})}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if match(r) {
			select {
			case g.reached <- struct{}{}:
			default:
			}
			<-g.release
		}
		e.fake.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	t.Cleanup(g.open) // runs first: Close waits for the requests still held
	qb, err := newQbit(proxy.URL, "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	qb.stateLag = e.qb.stateLag
	e.qb = qb
	return g
}

func qbitPath(path string) func(*http.Request) bool {
	return func(r *http.Request) bool { return r.URL.Path == path }
}

// heldBody is a request body whose first read waits for the test: a handler
// that looks the draft up before decoding its body stops right between the
// two.
type heldBody struct {
	data    io.Reader
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldBody(data string) *heldBody {
	return &heldBody{data: strings.NewReader(data), reached: make(chan struct{}), release: make(chan struct{})}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.reached)
		<-b.release
	})
	return b.data.Read(p)
}

func (e *testEnv) hasCall(match func(string) bool) bool {
	for _, call := range e.fake.snapshotCalls() {
		if match(call) {
			return true
		}
	}
	return false
}

func isDeleteCall(call string) bool { return strings.HasPrefix(call, "delete:") }

// A confirm in flight owns its draft. The sweeper used to expire a draft by
// its age alone: past the TTL it deleted a confirming draft's torrent with its
// data right before the confirm removed the draft tags, or overwrote a
// finished confirm with "draft expired" while the item was downloading.
func TestSweepSparesDraftBeingConfirmed(t *testing.T) {
	env := draftEnv(t)
	gate := env.gateQbit(t, qbitPath("/api/v2/torrents/removeTags"))
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	ready := env.awaitDraft(t, created.ID, draftReady)

	body := `{"files":["` + ready.Files[1].ID + `"]}`
	if rec := env.do(t, "POST", "/drafts/"+created.ID+"/confirm", body); rec.Code != http.StatusAccepted {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	gate.await(t) // the confirm is about to turn the draft into an item

	env.draftTTL = time.Nanosecond
	env.sweepDrafts()
	if rec := env.do(t, "DELETE", "/drafts/"+created.ID, ""); rec.Code != http.StatusConflict {
		t.Fatalf("DELETE of a confirming draft = %d %s", rec.Code, rec.Body.String())
	}
	if d, _ := env.draftStore().get(created.ID); d.status != draftConfirming {
		t.Fatalf("confirming draft was taken over: %q %q", d.status, d.failure)
	}
	if env.hasCall(isDeleteCall) {
		t.Fatalf("torrent of a confirming draft was deleted: %v", env.fake.snapshotCalls())
	}

	gate.open()
	if d := env.awaitDraft(t, created.ID, draftConfirmed, draftFailed); d.Status != draftConfirmed {
		t.Fatalf("confirm failed: %v", d.Error)
	}
	env.sweepDrafts()
	env.inflight.Wait()
	if d := env.awaitDraft(t, created.ID, draftConfirmed, draftFailed); d.Status != draftConfirmed || d.Error != nil {
		t.Fatalf("confirmed draft was rewritten by the sweep: %+v", d)
	}
	env.fake.mu.Lock()
	torrent := env.fake.find(env.fake.addHash)
	env.fake.mu.Unlock()
	if torrent == nil || env.hasCall(isDeleteCall) {
		t.Fatalf("confirmed item was deleted: %v", env.fake.snapshotCalls())
	}
}

// A confirm that read the draft as ready must not go ahead once a DELETE has
// taken the draft over. Both used to pass their status checks and then both
// act: the confirm went on with a torrent that was being deleted with its
// data.
func TestDraftConfirmLosesToConcurrentDelete(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	ready := env.awaitDraft(t, created.ID, draftReady)

	body := newHeldBody(`{"files":["` + ready.Files[0].ID + `"],"category":"Shows"}`)
	req := httptest.NewRequest("POST", "/drafts/"+created.ID+"/confirm", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		env.routes().ServeHTTP(rec, req)
	}()
	select {
	case <-body.reached: // the handler has seen the draft ready and reads the body
	case <-time.After(5 * time.Second):
		t.Fatal("confirm handler did not read its body")
	}

	if del := env.do(t, "DELETE", "/drafts/"+created.ID, ""); del.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %s", del.Code, del.Body.String())
	}
	close(body.release)
	<-served
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm after a concurrent DELETE = %d %s", rec.Code, rec.Body.String())
	}

	env.inflight.Wait()
	if d, _ := env.draftStore().get(created.ID); d.status != draftDiscarded {
		t.Fatalf("draft is %q (%s), want discarded", d.status, d.failure)
	}
	confirmCall := func(call string) bool {
		return strings.HasPrefix(call, "prio") || strings.HasPrefix(call, "setCategory:") ||
			strings.HasPrefix(call, "removeTags:") || call == "start"
	}
	if env.hasCall(confirmCall) {
		t.Fatalf("a confirm ran on a discarded draft: %v", env.fake.snapshotCalls())
	}
}

// A DELETE while the magnet is still being added finds no hash to remove. The
// resolver used to carry on regardless: it marked the discarded draft ready
// again and left the torrent in qBittorrent. Now it sees that the draft was
// taken over and removes the torrent itself.
func TestDraftDiscardedWhileResolvingRemovesTorrent(t *testing.T) {
	env := draftEnv(t)
	gate := env.gateQbit(t, qbitPath("/api/v2/torrents/add"))
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	gate.await(t)

	if del := env.do(t, "DELETE", "/drafts/"+created.ID, ""); del.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %s", del.Code, del.Body.String())
	}
	env.awaitDraft(t, created.ID, draftDiscarded)
	gate.open()
	env.inflight.Wait()

	if d, _ := env.draftStore().get(created.ID); d.status != draftDiscarded {
		t.Fatalf("discarded draft came back as %q", d.status)
	}
	env.fake.mu.Lock()
	torrent := env.fake.find(env.fake.addHash)
	env.fake.mu.Unlock()
	if torrent != nil {
		t.Fatal("torrent of a discarded draft was left in qbittorrent")
	}
}

// Looking up the created item takes a catalog build. It used to run inside the
// drafts mutex, and every draft endpoint and the sweep waited for it.
func TestDraftConfirmFindsItemOutsideDraftsLock(t *testing.T) {
	env := draftEnv(t)
	var armed atomic.Bool
	gate := env.gateQbit(t, func(r *http.Request) bool {
		// The catalog lists all torrents; the confirm's own calls name one.
		return armed.Load() && r.URL.Path == "/api/v2/torrents/info" && r.URL.RawQuery == ""
	})
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	ready := env.awaitDraft(t, created.ID, draftReady)
	env.inflight.Wait()
	armed.Store(true)

	body := `{"files":["` + ready.Files[0].ID + `"]}`
	if rec := env.do(t, "POST", "/drafts/"+created.ID+"/confirm", body); rec.Code != http.StatusAccepted {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	gate.await(t) // the confirm is building the catalog to find the item

	got := make(chan *draft, 1)
	go func() {
		d, _ := env.draftStore().get(created.ID)
		got <- d
	}()
	select {
	case d := <-got:
		if d.status != draftConfirming {
			t.Fatalf("draft is %q while the item is looked up", d.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drafts store is locked while the confirm looks up the item")
	}

	gate.open()
	confirmed := env.awaitDraft(t, created.ID, draftConfirmed, draftFailed)
	env.inflight.Wait()
	if confirmed.Status != draftConfirmed || confirmed.Item == nil || *confirmed.Item == "" {
		t.Fatalf("confirm=%+v", confirmed)
	}
}

// The hub keeps a draft's last snapshot even with nobody subscribed: that is
// what a client connecting to the stream gets first. Once the sweep forgets
// the draft, the snapshot goes too; it used to stay for good.
func TestSweepForgetsDraftSnapshot(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	env.awaitDraft(t, created.ID, draftReady)
	if rec := env.do(t, "DELETE", "/drafts/"+created.ID, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body.String())
	}
	env.awaitDraft(t, created.ID, draftDiscarded)
	env.inflight.Wait()
	if _, ok := env.hub().snapshotFor(created.ID); !ok {
		t.Fatal("no snapshot is kept for the draft")
	}

	store := env.draftStore()
	store.mu.Lock()
	store.m[created.ID].settled = time.Now().Add(-time.Hour)
	store.mu.Unlock()
	env.sweepDrafts()

	if _, ok := store.get(created.ID); ok {
		t.Fatal("finished draft was not forgotten")
	}
	if _, ok := env.hub().snapshotFor(created.ID); ok {
		t.Fatal("the hub still keeps the snapshot of a forgotten draft")
	}
}

// A draft nobody acts on is still expired: claimed first, then removed with
// its data.
func TestSweepExpiresOpenDraft(t *testing.T) {
	env := draftEnv(t)
	created := env.createDraft(t, `{"magnet":"`+draftMagnet+`"}`)
	env.awaitDraft(t, created.ID, draftReady)
	env.inflight.Wait()

	env.draftTTL = time.Nanosecond
	env.sweepDrafts()

	d, _ := env.draftStore().get(created.ID)
	if d.status != draftFailed || d.failure != "draft expired" {
		t.Fatalf("expired draft is %q (%s)", d.status, d.failure)
	}
	if !env.hasCall(func(call string) bool { return call == "delete:deleteFiles=true" }) {
		t.Fatalf("expired draft's data was left behind: %v", env.fake.snapshotCalls())
	}
}
