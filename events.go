package main

// events.go is the state stream (SSE).
//
// A single poller queries qBittorrent for all clients, instead of each client
// polling on its own. This is not an optimization but the removal of a whole
// class of bugs: while the client assembled its picture of the world on a
// timer, it had races between concurrent updates, stale snapshots, and
// optimistic states that drifted away from reality.
//
// Whole snapshots are sent, not deltas. With dozens of items the difference
// in traffic is negligible, and everything that usually comes with deltas
// goes away: sequence numbers, replaying what was missed, reconciliation and
// divergence after a disconnect. Reconnect and you get the current picture,
// that is all.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	eventState     = "state"
	eventOperation = "operation"
	eventPing      = "ping"

	// topicList is the stream with the item list; every other topic is an item id.
	topicList = ""
)

type sseFrame struct {
	event string
	data  []byte
}

// eventSub is a subscriber. State and operations are stored differently on
// purpose.
//
// State is ONE slot that gets replaced. It must not be lost: a frame used to
// be dropped when the buffer overflowed while h.last was still updated, so
// the subscriber was stuck with the old picture until the state changed
// again. A slot that always holds the latest value makes this impossible.
//
// Operations are a queue: an operation's outcome must not be lost, and "the
// latest" supersedes nothing here. The queue is bounded, so after a reconnect
// the client also checks unfinished operations via GET /operations/{id}.
type eventSub struct {
	topic string

	mu     sync.Mutex
	state  []byte
	ops    [][]byte
	pings  int
	closed bool
	wake   chan struct{}
	maxOps int
}

func newEventSub(topic string) *eventSub {
	return &eventSub{topic: topic, wake: make(chan struct{}, 1), maxOps: 256}
}

func (s *eventSub) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *eventSub) pushState(data []byte) {
	s.mu.Lock()
	if !s.closed {
		s.state = data
	}
	s.mu.Unlock()
	s.signal()
}

func (s *eventSub) pushOperation(data []byte) {
	s.mu.Lock()
	if !s.closed {
		if len(s.ops) >= s.maxOps {
			s.ops = s.ops[1:]
		}
		s.ops = append(s.ops, data)
	}
	s.mu.Unlock()
	s.signal()
}

func (s *eventSub) pushPing() {
	s.mu.Lock()
	if !s.closed {
		s.pings++
	}
	s.mu.Unlock()
	s.signal()
}

// drain takes everything accumulated in one go.
func (s *eventSub) drain() (frames []sseFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != nil {
		frames = append(frames, sseFrame{event: eventState, data: s.state})
		s.state = nil
	}
	for _, op := range s.ops {
		frames = append(frames, sseFrame{event: eventOperation, data: op})
	}
	s.ops = nil
	if s.pings > 0 {
		frames = append(frames, sseFrame{event: eventPing, data: []byte("{}")})
		s.pings = 0
	}
	return frames
}

func (s *eventSub) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.signal()
}

type eventHub struct {
	mu   sync.Mutex
	subs map[*eventSub]struct{}
	last map[string][]byte // topic -> last computed snapshot
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[*eventSub]struct{}{}, last: map[string][]byte{}}
}

func (h *eventHub) subscribe(topic string) *eventSub {
	sub := newEventSub(topic)
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *eventHub) unsubscribe(sub *eventSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	if !h.hasTopicLocked(sub.topic) {
		delete(h.last, sub.topic)
	}
	h.mu.Unlock()
	sub.close()
}

func (h *eventHub) hasTopicLocked(topic string) bool {
	for sub := range h.subs {
		if sub.topic == topic {
			return true
		}
	}
	return false
}

// topics returns the snapshots that need computing at all. Details of an
// item nobody is subscribed to are never assembled.
func (h *eventHub) topics() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{topicList: true}
	out := []string{topicList}
	for sub := range h.subs {
		if !seen[sub.topic] {
			seen[sub.topic] = true
			out = append(out, sub.topic)
		}
	}
	return out
}

func (h *eventHub) forEach(topic string, fn func(*eventSub)) {
	h.mu.Lock()
	subs := make([]*eventSub, 0, len(h.subs))
	for sub := range h.subs {
		if sub.topic == topic {
			subs = append(subs, sub)
		}
	}
	h.mu.Unlock()
	for _, sub := range subs {
		fn(sub)
	}
}

// seedState stores a snapshot only if nothing has been computed for the topic
// yet: the poller may already have published a fresher one, and it must not
// be overwritten with a cached one.
func (h *eventHub) seedState(topic string, data []byte) {
	h.mu.Lock()
	_, seen := h.last[topic]
	if !seen {
		h.last[topic] = data
	}
	h.mu.Unlock()
	if !seen {
		h.forEach(topic, func(sub *eventSub) { sub.pushState(data) })
	}
}

// publishState delivers a snapshot to the subscribers if it has changed.
func (h *eventHub) publishState(topic string, data []byte) {
	h.mu.Lock()
	previous, seen := h.last[topic]
	changed := !seen || !bytes.Equal(previous, data)
	if changed {
		h.last[topic] = data
	}
	h.mu.Unlock()
	if changed {
		h.forEach(topic, func(sub *eventSub) { sub.pushState(data) })
	}
}

func (h *eventHub) publishOperationFrame(topic string, data []byte) {
	h.forEach(topic, func(sub *eventSub) { sub.pushOperation(data) })
}

// closeAll releases all subscribers. It is called on shutdown, after the
// started operations have been carried through: an event stream lives for
// hours, and without this http.Shutdown would dutifully wait for it until its
// deadline, while the client would get disconnected anyway, only 15 seconds
// later and by systemd.
func (h *eventHub) closeAll() {
	h.mu.Lock()
	subs := make([]*eventSub, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		sub.close()
	}
}

func (h *eventHub) publishPing() {
	h.mu.Lock()
	subs := make([]*eventSub, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		sub.pushPing()
	}
}

// snapshotFor returns the topic's last known snapshot, so a newly connected
// client gets the picture immediately instead of waiting for the next tick.
func (h *eventHub) snapshotFor(topic string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, ok := h.last[topic]
	return data, ok
}

// --- poller ---

// runStatePump builds the catalog on a timer and after every mutation, splits
// it into topics and publishes the snapshots that changed.
func (s *server) runStatePump(done <-chan struct{}) {
	ticker := time.NewTicker(s.statePollInterval)
	defer ticker.Stop()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	// The draft sweep runs on its own, infrequent timer: it calls into
	// qBittorrent and has nothing to do with how often the catalog refreshes.
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()

	s.refreshState()
	s.sweepDrafts()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s.refreshState()
		case <-sweep.C:
			s.sweepDrafts()
		case <-s.statePoke:
			// A short delay coalesces a burst of mutations into one recompute.
			time.Sleep(150 * time.Millisecond)
			drain(s.statePoke)
			s.refreshState()
		case <-ping.C:
			s.hub().publishPing()
		}
	}
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// poke asks for the state to be recomputed right now: something changed
// because of us, and there is no point in waiting for the next tick.
func (s *server) poke() {
	select {
	case s.statePoke <- struct{}{}:
	default:
	}
}

func (s *server) refreshState() {
	cat := s.buildCatalog()
	s.catalog.Store(cat)

	list, err := json.Marshal(s.renderList(cat))
	if err != nil {
		log.Printf("state: marshal list: %v", err)
		return
	}
	s.hub().publishState(topicList, list)

	for _, topic := range s.hub().topics() {
		if topic == topicList {
			continue
		}
		if isDraftTopic(topic) {
			// A draft is not in the catalog and must not be: drafts.go
			// publishes its state at the moment it changes.
			continue
		}
		item := cat.item(topic)
		if item == nil {
			// The item is gone: the client will see that in the list and leave
			// the screen.
			s.hub().publishState(topic, []byte(`null`))
			continue
		}
		detail, err := json.Marshal(s.renderDetail(cat, item))
		if err != nil {
			log.Printf("state: marshal item %s: %v", topic, err)
			continue
		}
		s.hub().publishState(topic, detail)
	}
}

// --- serving the stream ---

func (s *server) serveEvents(w http.ResponseWriter, r *http.Request, topic string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, codeInternal, "streaming is not supported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Disables buffering in nginx, in case it ever ends up in front.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := s.hub().subscribe(topic)
	defer s.hub().unsubscribe(sub)

	if data, ok := s.hub().snapshotFor(topic); ok {
		writeSSE(w, sseFrame{event: eventState, data: data})
		flusher.Flush()
	} else {
		// The first frame comes from the already built catalog (it goes out on the
		// first pass of the loop below); a fresh one follows an out-of-turn
		// recompute if anything has changed.
		s.seedTopic(topic)
		s.poke()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.wake:
			frames := sub.drain()
			for _, frame := range frames {
				writeSSE(w, frame)
			}
			if len(frames) > 0 {
				flusher.Flush()
			}
			sub.mu.Lock()
			closed := sub.closed
			sub.mu.Unlock()
			if closed {
				return
			}
		}
	}
}

// seedTopic gives a new subscriber of an item that item's snapshot from the
// already built catalog, the same one the list was rendered from. The poller
// does not compute details of items nobody subscribes to, so without this the
// first frame waited for poke, which sleeps 150 ms to coalesce mutations and
// then calls qBittorrent, and the item screen sat without files just as long.
// Drafts are published by drafts.go, and "no such item" must come from a
// fresh recompute rather than the cache, so that case is left to poke.
func (s *server) seedTopic(topic string) {
	if topic == topicList || isDraftTopic(topic) {
		return
	}
	cat := s.catalog.Load()
	if cat == nil {
		return
	}
	item := cat.item(topic)
	if item == nil {
		return
	}
	detail, err := json.Marshal(s.renderDetail(cat, item))
	if err != nil {
		log.Printf("state: marshal item %s: %v", topic, err)
		return
	}
	s.hub().seedState(topic, detail)
}

func writeSSE(w http.ResponseWriter, frame sseFrame) {
	// The data is always single-line JSON, so there is no need to split on \n.
	w.Write([]byte("event: " + frame.event + "\ndata: "))
	w.Write(frame.data)
	w.Write([]byte("\n\n"))
}

// publishOperation sends an operation update to its item's stream.
func (s *server) publishOperation(op operation) {
	data, err := json.Marshal(op)
	if err != nil {
		log.Printf("operation %s: marshal: %v", op.ID, err)
		return
	}
	s.hub().publishOperationFrame(op.Item, data)
	// The set of actions depends on whether an operation is running; recompute.
	s.poke()
}
