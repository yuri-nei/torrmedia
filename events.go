package main

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

	topicList = ""
)

type sseFrame struct {
	event string
	data  []byte
}

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
	last map[string][]byte
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

func (h *eventHub) snapshotFor(topic string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, ok := h.last[topic]
	return data, ok
}

func (s *server) runStatePump(done <-chan struct{}) {
	ticker := time.NewTicker(s.statePollInterval)
	defer ticker.Stop()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	s.refreshState()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s.refreshState()
		case <-s.statePoke:
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
		item := cat.item(topic)
		if item == nil {
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
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := s.hub().subscribe(topic)
	defer s.hub().unsubscribe(sub)

	if data, ok := s.hub().snapshotFor(topic); ok {
		writeSSE(w, sseFrame{event: eventState, data: data})
		flusher.Flush()
	} else {
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

func writeSSE(w http.ResponseWriter, frame sseFrame) {
	w.Write([]byte("event: " + frame.event + "\ndata: "))
	w.Write(frame.data)
	w.Write([]byte("\n\n"))
}

func (s *server) publishOperation(op operation) {
	data, err := json.Marshal(op)
	if err != nil {
		log.Printf("operation %s: marshal: %v", op.ID, err)
		return
	}
	s.hub().publishOperationFrame(op.Item, data)
	s.poke()
}
