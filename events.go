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
	ch    chan sseFrame
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
	sub := &eventSub{topic: topic, ch: make(chan sseFrame, 8)}
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
	close(sub.ch)
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

func (h *eventHub) publish(topic string, frame sseFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.topic != topic {
			continue
		}
		select {
		case sub.ch <- frame:
		default:
		}
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
		h.publish(topic, sseFrame{event: eventState, data: data})
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
			for _, topic := range s.events.topics() {
				s.events.publish(topic, sseFrame{event: eventPing, data: []byte("{}")})
			}
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
	s.events.publishState(topicList, list)

	for _, topic := range s.events.topics() {
		if topic == topicList {
			continue
		}
		item := cat.item(topic)
		if item == nil {
			s.events.publishState(topic, []byte(`null`))
			continue
		}
		detail, err := json.Marshal(s.renderDetail(cat, item))
		if err != nil {
			log.Printf("state: marshal item %s: %v", topic, err)
			continue
		}
		s.events.publishState(topic, detail)
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

	sub := s.events.subscribe(topic)
	defer s.events.unsubscribe(sub)

	if data, ok := s.events.snapshotFor(topic); ok {
		writeSSE(w, sseFrame{event: eventState, data: data})
		flusher.Flush()
	} else {
		s.poke()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-sub.ch:
			if !open {
				return
			}
			writeSSE(w, frame)
			flusher.Flush()
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
	s.events.publish(op.Item, sseFrame{event: eventOperation, data: data})
	s.poke()
}
