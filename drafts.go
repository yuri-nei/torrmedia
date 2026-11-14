package main

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
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	draftResolving  = "resolving"
	draftReady      = "ready"
	draftConfirming = "confirming"
	draftConfirmed  = "confirmed"
	draftDiscarding = "discarding"
	draftDiscarded  = "discarded"
	draftFailed     = "failed"
)

const (
	draftTag      = "torrmedia-draft"
	draftIDPrefix = "dr_"
	draftLimit    = 8
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
	item     string
	files    []draftFile
	created  time.Time
	settled  time.Time
}

func (d *draft) finished() bool {
	return d.status == draftConfirmed || d.status == draftDiscarded || d.status == draftFailed
}

func (d *draft) clone() *draft {
	copied := *d
	copied.files = append([]draftFile(nil), d.files...)
	return &copied
}

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

func (d *drafts) update(id string, mutate func(*draft)) (*draft, bool) {
	d.mu.Lock()
	item, ok := d.m[id]
	if !ok {
		d.mu.Unlock()
		return nil, false
	}
	wasFinished := item.finished()
	mutate(item)
	if !wasFinished && item.finished() {
		item.settled = time.Now()
	}
	copied := item.clone()
	d.mu.Unlock()
	return copied, true
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

func (s *server) newDraft(category string) *draft {
	id := draftIDPrefix + newRandomHex(6)
	return &draft{
		id: id, tag: draftTag + "-" + strings.TrimPrefix(id, draftIDPrefix),
		category: category, status: draftResolving, created: time.Now(),
	}
}

func (s *server) setDraft(id string, mutate func(*draft)) *draft {
	updated, ok := s.draftStore().update(id, mutate)
	if !ok {
		return nil
	}
	s.publishDraft(updated)
	return updated
}

func (s *server) failDraft(id string, err error) {
	log.Printf("draft %s: %v", id, err)
	s.setDraft(id, func(d *draft) {
		d.status = draftFailed
		d.failure = err.Error()
	})
}

func (s *server) resolveDraft(id, magnet string) {
	current, ok := s.draftStore().get(id)
	if !ok {
		return
	}
	addedAfter := time.Now().Unix() - 30

	form := map[string]string{
		"urls":          magnet,
		"tags":          draftTag + "," + current.tag,
		"stopCondition": "MetadataReceived",
	}
	if current.category != "" {
		form["category"] = current.category
	}
	if err := s.qb.addForm(form); err != nil {
		s.failDraft(id, err)
		return
	}

	torrent, err := s.awaitDraftTorrent(current.tag, addedAfter)
	if err != nil {
		s.failDraft(id, err)
		return
	}
	s.setDraft(id, func(d *draft) {
		d.hash = torrent.Hash
		if d.title == "" {
			d.title = torrent.Name
		}
	})

	files, err := s.awaitDraftMetadata(torrent.Hash)
	if err != nil {
		s.discardTorrent(current.tag, torrent.Hash)
		s.failDraft(id, err)
		return
	}

	if err := s.stopDraftTorrent(torrent.Hash); err != nil {
		s.discardTorrent(current.tag, torrent.Hash)
		s.failDraft(id, fmt.Errorf("draft torrent could not be stopped: %w", err))
		return
	}

	fresh, err := s.qb.torrent(torrent.Hash)
	if err != nil {
		s.failDraft(id, err)
		return
	}
	s.setDraft(id, func(d *draft) {
		d.status = draftReady
		d.title = torrentTitle(fresh, len(files) == 1)
		d.files = draftFilesOf(fresh, files)
		d.size = 0
		for _, f := range d.files {
			d.size += f.size
		}
	})
}

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
			wanted: true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return fileOrderLess(out[i].rel, out[j].rel) })
	return out
}

func (s *server) confirmDraft(id string, wanted map[string]bool, category string) {
	current, ok := s.draftStore().get(id)
	if !ok {
		return
	}
	skip := []int{}
	for _, f := range current.files {
		if !wanted[f.id] {
			skip = append(skip, f.qbitIndex)
		}
	}

	unlock, err := s.guardTorrent(current.hash)
	if err != nil {
		s.failDraft(id, err)
		return
	}
	defer unlock()

	if len(skip) > 0 {
		if err := s.qb.setFilePrios(current.hash, skip, 0); err != nil {
			s.failDraft(id, err)
			return
		}
		if err := s.qb.waitFilePriorities(current.hash, skip, 0, s.operationTimeout, s.pollInterval); err != nil {
			s.failDraft(id, err)
			return
		}
	}
	if category != current.category {
		if err := s.qb.setCategory(current.hash, category); err != nil {
			s.failDraft(id, err)
			return
		}
	}
	if err := s.qb.removeTags(current.hash, []string{draftTag, current.tag}); err != nil {
		s.failDraft(id, err)
		return
	}
	s.poke()

	if len(wanted) > 0 {
		if err := s.qb.switchTorrent(current.hash, "start"); err != nil {
			s.failDraft(id, fmt.Errorf("item was added but did not start: %w", err))
			return
		}
		if _, err := s.qb.waitStarted(current.hash, s.startTimeout(), s.pollInterval); err != nil {
			s.failDraft(id, fmt.Errorf("item was added but did not start: %w", err))
			return
		}
	}
	s.setDraft(id, func(d *draft) {
		d.status = draftConfirmed
		d.category = category
		d.item = s.itemIDForHash(current.hash)
		for i := range d.files {
			d.files[i].wanted = wanted[d.files[i].id]
		}
	})
	s.poke()
}

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

func (s *server) discardDraft(id string) {
	current, ok := s.draftStore().get(id)
	if !ok {
		return
	}
	if current.hash != "" {
		if err := s.discardTorrent(current.tag, current.hash); err != nil {
			s.failDraft(id, err)
			return
		}
	}
	s.setDraft(id, func(d *draft) { d.status = draftDiscarded })
	s.poke()
}

func (s *server) discardTorrent(tag, hash string) error {
	t, err := s.qb.torrent(hash)
	if err != nil {
		if errors.Is(err, errTorrentNotFound) {
			return nil
		}
		return err
	}
	if !t.hasTag(tag) {
		return fmt.Errorf("torrent %s is no longer a draft; refusing to delete its data", hash)
	}
	return s.qb.deleteTorrent(hash, true)
}

func (s *server) sweepDrafts() {
	if s.qb == nil {
		return
	}
	now := time.Now()
	known := map[string]bool{}
	for _, d := range s.draftStore().list() {
		known[d.tag] = true
		switch {
		case !d.finished() && now.Sub(d.created) > s.draftTTL:
			log.Printf("draft %s: expired after %s", d.id, s.draftTTL)
			if d.hash != "" {
				if err := s.discardTorrent(d.tag, d.hash); err != nil {
					log.Printf("draft %s: discard: %v", d.id, err)
				}
			}
			s.setDraft(d.id, func(item *draft) {
				item.status = draftFailed
				item.failure = "draft expired"
			})
		case d.finished() && now.Sub(d.settled) > 5*time.Minute:
			s.draftStore().forget(d.id)
		}
	}

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
	s.publishDraft(d)
	s.serveEvents(w, r, d.id)
}

func (s *server) handleDraftConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := s.resolveDraftRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Files    []string `json:"files"`
		Category string   `json:"category"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	if d.status != draftReady {
		writeErr(w, http.StatusConflict, codeBusy, "draft is %s", d.status)
		return
	}
	if err := checkCategory(body.Category); err != nil {
		writeErr(w, http.StatusBadRequest, codeBadRequest, "%v", err)
		return
	}
	known := map[string]bool{}
	for _, f := range d.files {
		known[f.id] = true
	}
	wanted := map[string]bool{}
	for _, id := range body.Files {
		if !known[id] {
			writeErr(w, http.StatusNotFound, codeNotFound, "no such file in this draft")
			return
		}
		wanted[id] = true
	}

	updated := s.setDraft(d.id, func(item *draft) { item.status = draftConfirming })
	if updated == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such draft")
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
	d, ok := s.resolveDraftRequest(w, r)
	if !ok {
		return
	}
	switch d.status {
	case draftConfirming:
		writeErr(w, http.StatusConflict, codeBusy, "draft is being confirmed")
		return
	case draftConfirmed:
		writeErr(w, http.StatusConflict, codeBadRequest,
			"draft was already confirmed; remove the item instead")
		return
	case draftDiscarding, draftDiscarded, draftFailed:
		writeJSON(w, http.StatusAccepted, s.renderDraft(d))
		return
	}

	updated := s.setDraft(d.id, func(item *draft) { item.status = draftDiscarding })
	if updated == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such draft")
		return
	}
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		s.discardDraft(d.id)
	}()
	writeJSON(w, http.StatusAccepted, s.renderDraft(updated))
}

func (s *server) handleStorage(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.storageInfo())
}

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
