package main

// drafts.go implements item drafts: what the user sees in the "choose
// episodes" window.
//
// Regular clients show a torrent's file list before the download starts. In
// BitTorrent there is no file list until the metadata has been downloaded, and
// only a running torrent can download it. So a draft is a real torrent in
// qBittorrent, added with `stopCondition=MetadataReceived`: it starts up,
// fetches the metadata and pauses itself without downloading any content.
//
// Until a draft is confirmed, the catalog doesn't show it (see withoutDrafts):
// there is no item yet, only an offer to create one. Hence three rules, each
// closing off its own way of losing data:
//
//  1. A draft carries TWO tags: the shared `torrmedia-draft` and its own
//     unique one. The sweeper needs the shared one; the unique one is for
//     finding exactly our own torrent.
//  2. A draft is deleted together with its data only after re-checking the
//     unique tag. No tag means it is no longer our draft, and it must not be
//     touched.
//  3. If qBittorrent attached our tags to an ALREADY existing torrent (which is
//     what it does with a duplicate), the tags are removed and the draft is
//     rejected. Otherwise cancelling the picker would delete someone else's
//     downloaded files.

import (
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Draft statuses. A lifecycle of its own instead of an operation: a draft has
// its own state, its own stream and its own end, and wrapping it in an
// operation as well would mean describing the same thing twice.
//
// Every status change is a compare-and-set (drafts.transition): it applies
// only from the statuses the caller expects, so of two parties that both saw
// a draft "ready", only one gets to act on it. Whoever moves a draft out of
// resolving or ready owns it from then on:
//
//	resolving → ready             the resolver, once the file list is known
//	resolving → failed            the resolver, when the torrent didn't resolve
//	ready → confirming            POST /confirm, handing the draft to confirmDraft
//	confirming → confirmed|failed confirmDraft
//	resolving|ready → discarding  DELETE or the sweeper, which then own it
//	discarding → discarded|failed whoever moved it to discarding
//
// Finished statuses never change again. A torrent is deleted only by the owner
// of a draft in discarding, or by the resolver while the draft has not become
// ready yet, so nothing deletes the torrent of a draft that is being or has
// been confirmed.
const (
	draftResolving  = "resolving"  // waiting for the torrent's metadata
	draftReady      = "ready"      // file list known, waiting for the user's choice
	draftConfirming = "confirming" // applying the choice and starting the torrent
	draftConfirmed  = "confirmed"  // item created, the draft is gone
	draftDiscarding = "discarding" // removing the torrent with whatever got downloaded
	draftDiscarded  = "discarded"
	draftFailed     = "failed"
)

// draftOpen are the statuses of a draft nobody has started to confirm or
// discard yet: it waits for its metadata or for the user's choice. Only from
// these may DELETE or the sweeper take a draft over.
var draftOpen = []string{draftResolving, draftReady}

const (
	// draftTag marks every draft; the sweeper finds abandoned ones by it.
	draftTag = "torrmedia-draft"
	// draftIDPrefix sets draft topics apart from item ids in the event stream.
	draftIDPrefix = "dr_"
	// draftLimit is how many picker windows may be open at once. Each one keeps
	// a live torrent in qBittorrent, hence the limit.
	draftLimit = 8
)

type draftFile struct {
	id        string
	name      string
	rel       string
	size      int64
	qbitIndex int
	wanted    bool
}

type draft struct {
	id       string
	tag      string
	hash     string
	category string
	status   string
	title    string
	size     int64
	failure  string
	item     string // id of the created item, known after confirmation
	files    []draftFile
	created  time.Time
	settled  time.Time // when the draft reached a terminal status
}

func (d *draft) finished() bool {
	return d.status == draftConfirmed || d.status == draftDiscarded || d.status == draftFailed
}

func (d *draft) clone() *draft {
	copied := *d
	copied.files = append([]draftFile(nil), d.files...)
	return &copied
}

// drafts is an in-memory store. A draft has no reason to survive a restart: a
// client that lost its stream will reopen the window anyway, and the sweeper
// will pick up an abandoned torrent by its tag.
type drafts struct {
	mu sync.Mutex
	m  map[string]*draft
}

func newDrafts() *drafts { return &drafts{m: map[string]*draft{}} }

func (d *drafts) add(item *draft) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	live := 0
	for _, existing := range d.m {
		if !existing.finished() {
			live++
		}
	}
	if live >= draftLimit {
		return false
	}
	d.m[item.id] = item
	return true
}

func (d *drafts) get(id string) (*draft, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	item, ok := d.m[id]
	if !ok {
		return nil, false
	}
	return item.clone(), true
}

// transition is the only way a draft changes. mutate runs under the mutex and
// only while the draft is in one of the from statuses, so checking the status
// and changing it are a single step. It returns a copy of the draft as it is
// now and whether mutate was applied; a nil copy means there is no such
// draft.
func (d *drafts) transition(id string, from []string, mutate func(*draft)) (*draft, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	item, ok := d.m[id]
	if !ok {
		return nil, false
	}
	if !slices.Contains(from, item.status) {
		return item.clone(), false
	}
	wasFinished := item.finished()
	mutate(item)
	if !wasFinished && item.finished() {
		item.settled = time.Now()
	}
	return item.clone(), true
}

func (d *drafts) list() []*draft {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*draft, 0, len(d.m))
	for _, item := range d.m {
		out = append(out, item.clone())
	}
	return out
}

func (d *drafts) forget(id string) {
	d.mu.Lock()
	delete(d.m, id)
	d.mu.Unlock()
}

func (s *server) draftStore() *drafts {
	s.ensureRuntime()
	return s.draftsStore
}

func isDraftTopic(topic string) bool { return strings.HasPrefix(topic, draftIDPrefix) }

// withoutDrafts drops from a qBittorrent snapshot the torrents the user hasn't
// confirmed yet. To the catalog they don't exist: no files have been chosen,
// there is nothing to download, and the space they take is the metadata plus a
// few megabytes.
func withoutDrafts(torrents []qbTorrentSnapshot) []qbTorrentSnapshot {
	out := make([]qbTorrentSnapshot, 0, len(torrents))
	for i := range torrents {
		if torrents[i].hasTag(draftTag) {
			continue
		}
		out = append(out, torrents[i])
	}
	return out
}

// --- lifecycle ---

func (s *server) newDraft(category string) *draft {
	id := draftIDPrefix + newRandomHex(6)
	return &draft{
		id: id, tag: draftTag + "-" + strings.TrimPrefix(id, draftIDPrefix),
		category: category, status: draftResolving, created: time.Now(),
	}
}

// transitionDraft changes the state if the draft is in one of the from
// statuses (see drafts.transition) and immediately broadcasts the change to
// subscribers: the picker window lives on this stream and has no other way to
// learn that the metadata has arrived.
func (s *server) transitionDraft(id string, from []string, mutate func(*draft)) (*draft, bool) {
	updated, ok := s.draftStore().transition(id, from, mutate)
	if ok {
		s.publishDraft(updated)
	}
	return updated, ok
}

// failDraft ends a draft with an error, but only from the status the caller
// owns: a draft taken over in the meantime keeps the outcome of whoever took
// it over.
func (s *server) failDraft(id, from string, err error) {
	log.Printf("draft %s: %v", id, err)
	s.transitionDraft(id, []string{from}, func(d *draft) {
		d.status = draftFailed
		d.failure = err.Error()
	})
}

// resolveDraft adds the torrent, waits for its metadata and stops it.
func (s *server) resolveDraft(id, magnet string) {
	current, ok := s.draftStore().get(id)
	if !ok || current.status != draftResolving {
		return // discarded before the torrent was even added
	}
	addedAfter := time.Now().Unix() - 30 // slack for clock skew and qBittorrent's queue

	form := map[string]string{
		"urls": magnet,
		"tags": draftTag + "," + current.tag,
		// qBittorrent stops the torrent by itself once it has the metadata:
		// there is no point downloading content before confirmation.
		"stopCondition": "MetadataReceived",
	}
	if current.category != "" {
		form["category"] = current.category
	}
	if err := s.qb.addForm(form); err != nil {
		s.failDraft(id, draftResolving, err)
		return
	}

	torrent, err := s.awaitDraftTorrent(current.tag, addedAfter)
	if err != nil {
		s.failDraft(id, draftResolving, err)
		return
	}
	resolving := []string{draftResolving}
	if _, ok := s.transitionDraft(id, resolving, func(d *draft) {
		d.hash = torrent.Hash
		if d.title == "" {
			d.title = torrent.Name
		}
	}); !ok {
		s.dropTakenOverTorrent(id, current.tag, torrent.Hash)
		return
	}

	files, err := s.awaitDraftMetadata(torrent.Hash)
	if err != nil {
		// No metadata means nothing to download, but the torrent is already
		// sitting in qBittorrent and would keep looking for peers. Remove it
		// along with whatever has landed on disk.
		s.discardTorrent(current.tag, torrent.Hash)
		s.failDraft(id, draftResolving, err)
		return
	}

	// stopCondition should already have stopped the torrent; our own stop covers
	// the case where the metadata was already in the session (qBittorrent
	// #20122): then the condition never fires and the torrent downloads
	// everything.
	if err := s.stopDraftTorrent(torrent.Hash); err != nil {
		s.discardTorrent(current.tag, torrent.Hash)
		s.failDraft(id, draftResolving, fmt.Errorf("draft torrent could not be stopped: %w", err))
		return
	}

	fresh, err := s.qb.torrent(torrent.Hash)
	if err != nil {
		s.failDraft(id, draftResolving, err)
		return
	}
	if _, ok := s.transitionDraft(id, resolving, func(d *draft) {
		d.status = draftReady
		d.title = torrentTitle(fresh, len(files) == 1)
		d.files = draftFilesOf(fresh, files)
		d.size = 0
		for _, f := range d.files {
			d.size += f.size
		}
	}); !ok {
		s.dropTakenOverTorrent(id, current.tag, torrent.Hash)
	}
}

// dropTakenOverTorrent removes the torrent of a draft that was discarded or
// expired while it was still resolving. Whoever took the draft over may have
// done so before its hash was known and then had nothing to remove, so the
// resolver cleans up after itself. The draft cannot have been confirmed: only
// the resolver makes a draft ready, and it never did.
func (s *server) dropTakenOverTorrent(id, tag, hash string) {
	if err := s.discardTorrent(tag, hash); err != nil {
		log.Printf("draft %s: remove torrent after takeover: %v", id, err)
	}
}

// awaitDraftTorrent finds the just-added torrent by its unique tag.
//
// This is also where a duplicate is caught: asked to add a torrent it already
// has, qBittorrent reports success and simply appends our tags to it. We tell
// the two apart by the added time, and in that case remove the tags and
// refuse, because cancelling such a "draft" would delete someone else's
// files.
func (s *server) awaitDraftTorrent(tag string, addedAfter int64) (*qbTorrent, error) {
	var found *qbTorrent
	err := pollUntil(s.draftAddTimeout, s.pollInterval, func() (bool, error) {
		list, err := s.qb.torrentsWithTag(tag)
		if err != nil {
			return false, err
		}
		if len(list) == 0 {
			return false, nil
		}
		found = &list[0]
		return true, nil
	})
	if err != nil {
		if found == nil {
			return nil, errors.New("qbittorrent did not accept the magnet link")
		}
		return nil, err
	}
	if found.AddedOn > 0 && found.AddedOn < addedAfter {
		if err := s.qb.removeTags(found.Hash, []string{draftTag, tag}); err != nil {
			log.Printf("draft: could not unmark existing torrent %s: %v", found.Hash, err)
		}
		return nil, errors.New("this torrent is already in the library")
	}
	return found, nil
}

func (s *server) awaitDraftMetadata(hash string) ([]qbFile, error) {
	var files []qbFile
	err := pollUntil(s.draftMetadataTimeout, s.pollInterval, func() (bool, error) {
		if _, err := s.qb.torrent(hash); err != nil {
			if errors.Is(err, errTorrentNotFound) {
				return false, errors.New("draft torrent disappeared from qbittorrent")
			}
			return false, err
		}
		got, ok, err := s.qb.filesIfKnown(hash)
		if err != nil || !ok {
			return false, err
		}
		files = got
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("torrent metadata was not received: %w", err)
	}
	return files, nil
}

func (s *server) stopDraftTorrent(hash string) error {
	if err := s.qb.switchTorrent(hash, "stop"); err != nil {
		return err
	}
	timeout := 30 * time.Second
	if s.operationTimeout < timeout {
		timeout = s.operationTimeout
	}
	return s.qb.waitStopped(hash, timeout, s.pollInterval)
}

// draftFilesOf builds the item's file list with exactly the same paths and ids
// the catalog will see later: a client that picked an episode in the window
// will find it under the same id.
func draftFilesOf(t *qbTorrent, files []qbFile) []draftFile {
	contentAbs := torrentContentAbs(t)
	single := len(files) == 1
	out := make([]draftFile, 0, len(files))
	for i := range files {
		f := &files[i]
		index := i
		if f.Index != nil {
			index = *f.Index
		}
		within := torrentFileWithin(contentAbs, t.SavePath, f.Name, single)
		out = append(out, draftFile{
			id: fileID(within), name: filepath.Base(within), rel: within,
			size: f.Size, qbitIndex: index,
			// Everything is checked: regular clients do the same, and an extra
			// file here costs less than an unnoticed cleared checkbox on an
			// episode that was wanted.
			wanted: true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return fileOrderLess(out[i].rel, out[j].rel) })
	return out
}

// confirmDraft applies the user's choice and starts the torrent. The handler
// has already moved the draft to confirming, so from here on it is ours alone:
// neither DELETE nor the sweeper touches a confirming draft. A nil category
// keeps the one the draft was created with.
func (s *server) confirmDraft(id string, wanted map[string]bool, category *string) {
	current, ok := s.draftStore().get(id)
	if !ok || current.status != draftConfirming {
		return
	}
	target := current.category
	if category != nil {
		target = *category
	}
	fail := func(err error) { s.failDraft(id, draftConfirming, err) }
	skip := []int{}
	for _, f := range current.files {
		if !wanted[f.id] {
			skip = append(skip, f.qbitIndex)
		}
	}

	unlock, err := s.guardTorrent(current.hash)
	if err != nil {
		fail(err)
		return
	}
	defer unlock()

	// Priorities are cleared while the torrent is stopped and confirmed by
	// reading them back: starting with priorities still set would download
	// everything, and there is no room for that.
	if len(skip) > 0 {
		if err := s.qb.setFilePrios(current.hash, skip, 0); err != nil {
			fail(err)
			return
		}
		if err := s.qb.waitFilePriorities(current.hash, skip, 0, s.operationTimeout, s.pollInterval); err != nil {
			fail(err)
			return
		}
	}
	if target != current.category {
		if err := s.qb.setCategory(current.hash, target); err != nil {
			fail(err)
			return
		}
	}
	// Tags are removed BEFORE starting: from this point on it is a regular
	// torrent, and the sweeper won't touch it even if the start fails.
	if err := s.qb.removeTags(current.hash, []string{draftTag, current.tag}); err != nil {
		fail(err)
		return
	}
	s.poke()

	// With nothing to download, the torrent stays stopped. Starting a torrent
	// with all priorities cleared would mean seeding nothing.
	if len(wanted) > 0 {
		if err := s.qb.switchTorrent(current.hash, "start"); err != nil {
			// The item has been created and is visible in the catalog; it just
			// stayed stopped and can be started from there. The data is intact.
			fail(fmt.Errorf("item was added but did not start: %w", err))
			return
		}
		if _, err := s.qb.waitStarted(current.hash, s.startTimeout(), s.pollInterval); err != nil {
			fail(fmt.Errorf("item was added but did not start: %w", err))
			return
		}
	}
	s.transitionDraft(id, []string{draftConfirming}, func(d *draft) {
		d.status = draftConfirmed
		d.category = target
		d.item = s.itemIDForHash(current.hash)
		for i := range d.files {
			d.files[i].wanted = wanted[d.files[i].id]
		}
	})
	s.poke()
}

// itemIDForHash finds the id under which the torrent appeared in the catalog.
// The client needs it to open the item's screen right after adding.
func (s *server) itemIDForHash(hash string) string {
	cat := s.buildCatalog()
	s.catalog.Store(cat)
	for _, item := range cat.items {
		if item.hash == hash {
			return item.id
		}
	}
	return ""
}

// discardDraft cancels a draft: the torrent is removed together with whatever
// got downloaded while the user was choosing. The handler has already moved
// the draft to discarding, which also freezes its hash: the resolver can no
// longer record one, and removes its torrent itself if it gets that far.
func (s *server) discardDraft(id string) {
	current, ok := s.draftStore().get(id)
	if !ok || current.status != draftDiscarding {
		return
	}
	if current.hash != "" {
		if err := s.discardTorrent(current.tag, current.hash); err != nil {
			s.failDraft(id, draftDiscarding, err)
			return
		}
	}
	s.transitionDraft(id, []string{draftDiscarding}, func(d *draft) { d.status = draftDiscarded })
	s.poke()
}

// discardTorrent deletes a draft's torrent together with its data, but only
// after making sure it is still OUR draft. The check is mandatory: the hash
// may now belong to a torrent that was confirmed in the meantime or isn't ours
// at all, and deleting it with its data would then be irreversible. The check
// and the delete run under the torrent's lock, the one a confirm holds while
// it turns the draft into an item.
func (s *server) discardTorrent(tag, hash string) error {
	unlock := s.lockTorrent(hash)
	defer unlock()
	t, err := s.qb.torrent(hash)
	if err != nil {
		if errors.Is(err, errTorrentNotFound) {
			return nil // already gone, nothing to cancel
		}
		return err
	}
	if !t.hasTag(tag) {
		return fmt.Errorf("torrent %s is no longer a draft; refusing to delete its data", hash)
	}
	return s.qb.deleteTorrent(hash, true)
}

// sweepDrafts cleans up whatever was abandoned: the window was closed with the
// Home button, the client lost its connection, torrmedia was restarted.
// Without the sweep such a torrent would keep fetching metadata and taking up
// space without ever showing up in the catalog.
func (s *server) sweepDrafts() {
	if s.qb == nil {
		return
	}
	now := time.Now()
	known := map[string]bool{}
	for _, d := range s.draftStore().list() {
		known[d.tag] = true
		switch {
		case d.finished():
			if now.Sub(d.settled) > 5*time.Minute {
				s.draftStore().forget(d.id)
			}
		case now.Sub(d.created) > s.draftTTL:
			s.expireDraft(d.id)
		}
	}

	// Torrents with the draft tag that aren't among ours are left over from a
	// previous run. The deadline is the same: if torrmedia was restarted right
	// in the middle of choosing, the user still has time to confirm.
	list, err := s.qb.torrentsWithTag(draftTag)
	if err != nil {
		return
	}
	for i := range list {
		t := &list[i]
		if t.AddedOn <= 0 || now.Sub(time.Unix(t.AddedOn, 0)) <= s.draftTTL {
			continue
		}
		orphan := true
		for tag := range t.tagSet() {
			if known[tag] {
				orphan = false
				break
			}
		}
		if !orphan {
			continue
		}
		log.Printf("draft sweep: removing orphaned draft torrent %s", t.Hash)
		if err := s.qb.deleteTorrent(t.Hash, true); err != nil {
			log.Printf("draft sweep: %v", err)
		}
	}
}

// expireDraft removes a draft that outlived its TTL, together with its
// torrent. Only a draft nobody is acting on is expired, and it is claimed
// (moved to discarding) before anything is deleted: a confirm or a discard
// in progress owns its draft, and a confirming draft's torrent is about to
// become an item.
func (s *server) expireDraft(id string) {
	claimed, ok := s.transitionDraft(id, draftOpen, func(d *draft) { d.status = draftDiscarding })
	if !ok {
		return
	}
	log.Printf("draft %s: expired after %s", id, s.draftTTL)
	if claimed.hash != "" {
		if err := s.discardTorrent(claimed.tag, claimed.hash); err != nil {
			log.Printf("draft %s: discard: %v", id, err)
		}
	}
	s.transitionDraft(id, []string{draftDiscarding}, func(d *draft) {
		d.status = draftFailed
		d.failure = "draft expired"
	})
}

// --- handlers ---

func (s *server) handleDraftCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Magnet   string `json:"magnet"`
		Category string `json:"category"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	if s.qb == nil {
		writeErr(w, http.StatusBadGateway, codeQbitUnavailable, "qbittorrent is not configured")
		return
	}
	if err := checkAddLink(body.Magnet); err != nil {
		writeErr(w, http.StatusForbidden, codeRefused, "%v", err)
		return
	}
	if err := checkCategory(body.Category); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	// A duplicate is caught before adding: otherwise qBittorrent would silently
	// attach our tags to the existing torrent, the user would see a picker for
	// something already downloaded, and cancelling would take its files away.
	if hash := magnetInfoHash(body.Magnet); hash != "" {
		if existing, err := s.qb.torrent(hash); err == nil && existing != nil {
			writeErr(w, http.StatusConflict, codeAmbiguous, "this torrent is already in the library")
			return
		} else if err != nil && !errors.Is(err, errTorrentNotFound) {
			writeErr(w, http.StatusBadGateway, codeQbitUnavailable, "qbittorrent: %v", err)
			return
		}
	}

	d := s.newDraft(body.Category)
	if !s.draftStore().add(d) {
		writeErr(w, http.StatusConflict, codeBusy,
			"too many drafts are open at once; finish or cancel one first")
		return
	}
	magnet := body.Magnet
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		s.resolveDraft(d.id, magnet)
	}()

	w.Header().Set("Location", "/drafts/"+d.id)
	writeJSON(w, http.StatusAccepted, s.renderDraft(d))
}

func (s *server) resolveDraftRequest(w http.ResponseWriter, r *http.Request) (*draft, bool) {
	id := r.PathValue("draft")
	d, ok := s.draftStore().get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such draft")
		return nil, false
	}
	return d, true
}

func (s *server) handleDraft(w http.ResponseWriter, r *http.Request) {
	d, ok := s.resolveDraftRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.renderDraft(d))
}

func (s *server) handleDraftEvents(w http.ResponseWriter, r *http.Request) {
	d, ok := s.resolveDraftRequest(w, r)
	if !ok {
		return
	}
	// The snapshot goes into the hub before subscribing: the client must see the
	// current state right away rather than wait for the next change.
	s.publishDraft(d)
	s.serveEvents(w, r, d.id)
}

func (s *server) handleDraftConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := s.resolveDraftRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Files []string `json:"files"`
		// Category replaces the one given at creation; omitted or null, it
		// keeps that one. An empty string means no category, as on POST
		// /drafts.
		Category *string `json:"category"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	// An early answer for the obvious case; the transition below is what
	// actually decides.
	if d.status != draftReady {
		writeErr(w, http.StatusConflict, codeBusy, "draft is %s", d.status)
		return
	}
	if body.Category != nil {
		if err := checkCategory(*body.Category); err != nil {
			writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
			return
		}
	}
	known := map[string]bool{}
	for _, f := range d.files {
		known[f.id] = true
	}
	// An empty list means "add, but don't download yet": the item is created
	// with all priorities cleared and stays stopped. The wanted episodes are
	// enabled later from the item's own screen.
	wanted := map[string]bool{}
	for _, id := range body.Files {
		if !known[id] {
			writeErr(w, http.StatusNotFound, codeNotFound, "no such file in this draft")
			return
		}
		wanted[id] = true
	}

	// The status read above may be stale by now: a DELETE or the sweeper may
	// have taken the draft over. Only the one that moves it out of ready acts.
	// The file list validated above is the one that came with ready and does
	// not change after it.
	updated, ok := s.transitionDraft(d.id, []string{draftReady}, func(item *draft) {
		item.status = draftConfirming
	})
	if updated == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such draft")
		return
	}
	if !ok {
		writeErr(w, http.StatusConflict, codeBusy, "draft is %s", updated.status)
		return
	}
	category := body.Category
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		s.confirmDraft(d.id, wanted, category)
	}()
	writeJSON(w, http.StatusAccepted, s.renderDraft(updated))
}

func (s *server) handleDraftDelete(w http.ResponseWriter, r *http.Request) {
	// The check and the switch to discarding are one step: a confirm that got
	// there first keeps its torrent, and of two DELETEs only one discards.
	updated, ok := s.transitionDraft(r.PathValue("draft"), draftOpen, func(item *draft) {
		item.status = draftDiscarding
	})
	if updated == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such draft")
		return
	}
	if !ok {
		switch updated.status {
		case draftConfirming:
			writeErr(w, http.StatusConflict, codeBusy, "draft is being confirmed")
		case draftConfirmed:
			// The item has already been created and lives its own life: it must
			// be removed as an item, with all the checks (files being played,
			// ambiguity and so on).
			writeErr(w, http.StatusConflict, codeBadRequest,
				"draft was already confirmed; remove the item instead")
		default: // discarding, discarded, failed: nothing is left to do
			writeJSON(w, http.StatusAccepted, s.renderDraft(updated))
		}
		return
	}
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		s.discardDraft(updated.id)
	}()
	writeJSON(w, http.StatusAccepted, s.renderDraft(updated))
}

func (s *server) handleStorage(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.storageInfo())
}

// --- output ---

func (s *server) storageInfo() apiStorage {
	info := apiStorage{Reserve: s.enableReserve}
	if free, err := s.availableBytes(s.root); err == nil {
		info.Free = &free
	} else {
		log.Printf("storage: free space: %v", err)
	}
	if total, err := s.totalBytes(s.root); err == nil {
		info.Total = &total
	}
	return info
}

func (s *server) renderDraft(d *draft) apiDraft {
	files := make([]apiDraftFile, 0, len(d.files))
	for _, f := range d.files {
		files = append(files, apiDraftFile{
			ID: f.id, Name: f.name, Path: f.rel, Size: f.size, Wanted: f.wanted,
		})
	}
	return apiDraft{
		ID: d.id, Status: d.status, Title: d.title, Size: d.size,
		Category: nilIfEmpty(d.category), Item: nilIfEmpty(d.item),
		Error: nilIfEmpty(d.failure),
		Files: files, Storage: s.storageInfo(),
	}
}

func (s *server) publishDraft(d *draft) {
	data, err := json.Marshal(s.renderDraft(d))
	if err != nil {
		log.Printf("draft %s: marshal: %v", d.id, err)
		return
	}
	s.hub().publishState(d.id, data)
}

// --- magnet ---

// magnetInfoHash extracts the infohash from a magnet link: 40 hex characters
// or 32 base32 characters. An empty string means "couldn't parse", e.g. for a
// v2 torrent; the duplicate is then caught by the added time instead.
func magnetInfoHash(magnet string) string {
	parsed, err := url.Parse(strings.TrimSpace(magnet))
	if err != nil {
		return ""
	}
	for _, xt := range parsed.Query()["xt"] {
		value, found := strings.CutPrefix(strings.ToLower(xt), "urn:btih:")
		if !found {
			continue
		}
		switch len(value) {
		case 40:
			if _, err := hex.DecodeString(value); err == nil {
				return value
			}
		case 32:
			raw, err := base32.StdEncoding.DecodeString(strings.ToUpper(value))
			if err == nil && len(raw) == 20 {
				return hex.EncodeToString(raw)
			}
		}
	}
	return ""
}
