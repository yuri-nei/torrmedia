package main

// events_test.go covers the state stream and the operation registry.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readFrames parses SSE the same way the client will have to.
func readFrames(t *testing.T, body *bufio.Reader, want int, timeout time.Duration) []sseFrame {
	t.Helper()
	type parsed struct {
		frames []sseFrame
		err    error
	}
	ch := make(chan parsed, 1)
	go func() {
		frames := []sseFrame{}
		var event string
		for len(frames) < want {
			line, err := body.ReadString('\n')
			if err != nil {
				ch <- parsed{frames, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				frames = append(frames, sseFrame{
					event: event, data: []byte(strings.TrimPrefix(line, "data: ")),
				})
			}
		}
		ch <- parsed{frames, nil}
	}()
	select {
	case got := <-ch:
		if got.err != nil && len(got.frames) < want {
			t.Fatalf("read frames: %v (got %d of %d)", got.err, len(got.frames), want)
		}
		return got.frames
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %d frames", want)
		return nil
	}
}

// A client that connects gets the picture immediately; it should not have
// to wait for the next tick.
func TestEventsSendSnapshotOnConnect(t *testing.T) {
	env := newTestEnv(t)
	env.refreshState()

	srv := httptest.NewServer(env.routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q", ct)
	}

	frames := readFrames(t, bufio.NewReader(resp.Body), 1, 3*time.Second)
	if frames[0].event != eventState {
		t.Fatalf("first frame is %q", frames[0].event)
	}
	var list apiItemList
	if err := json.Unmarshal(frames[0].data, &list); err != nil {
		t.Fatalf("snapshot is not a list: %s", frames[0].data)
	}
	if len(list.Items) != 1 {
		t.Fatalf("snapshot=%+v", list)
	}
}

// The item screen gets the item's details right on connect, from the
// catalog the list was drawn from, not after an out-of-turn recompute. In
// tests the poller is not running, so poke by itself would produce no frame
// at all.
func TestItemEventsSendSnapshotOnConnect(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	env.refreshState() // catalog exists, item details don't: nobody is subscribed

	srv := httptest.NewServer(env.routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/items/" + item.id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	frames := readFrames(t, bufio.NewReader(resp.Body), 1, 3*time.Second)
	if frames[0].event != eventState {
		t.Fatalf("first frame is %q", frames[0].event)
	}
	var detail apiItemDetail
	if err := json.Unmarshal(frames[0].data, &detail); err != nil {
		t.Fatalf("snapshot is not an item: %s", frames[0].data)
	}
	if detail.ID != item.id || len(detail.Files) == 0 {
		t.Fatalf("snapshot=%+v", detail)
	}
}

// Stopping the service releases the stream at once instead of holding
// http.Shutdown until its deadline: a permanently connected client keeps its
// stream open for hours.
func TestEventsCloseAllReleasesStream(t *testing.T) {
	env := newTestEnv(t)
	env.refreshState()

	srv := httptest.NewServer(env.routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	readFrames(t, reader, 1, 3*time.Second) // got the snapshot: subscription is live

	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(reader)
		done <- err
	}()
	env.hub().closeAll()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close after closeAll")
	}
}

// A snapshot is sent only when the state has changed: otherwise the stream
// would turn into the same polling, just inside out.
func TestEventsPublishOnlyOnChange(t *testing.T) {
	env := newTestEnv(t)
	env.refreshState()

	sub := env.hub().subscribe(topicList)
	defer env.hub().unsubscribe(sub)

	env.refreshState() // nothing changed
	if frames := sub.drain(); len(frames) != 0 {
		t.Fatalf("unchanged state was published: %+v", frames)
	}

	env.fake.mu.Lock()
	env.fake.torrents[0].state = "uploading"
	env.fake.mu.Unlock()
	env.refreshState()
	frames := sub.drain()
	if len(frames) != 1 || frames[0].event != eventState ||
		!strings.Contains(string(frames[0].data), statusSeeding) {
		t.Fatalf("frames=%+v", frames)
	}
}

// P1: a state frame must never be lost. It used to be dropped when the
// buffer overflowed while "last sent" was still updated, so the subscriber
// was stuck with a stale picture until the state changed again.
func TestStateFrameIsNeverLost(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe(topicList)
	defer hub.unsubscribe(sub)

	// The subscriber does not read, while the state changes many times in a
	// row.
	for i := 0; i < 1000; i++ {
		hub.publishState(topicList, []byte(`{"n":`+strconv.Itoa(i)+`}`))
	}
	frames := sub.drain()
	if len(frames) != 1 {
		t.Fatalf("state frames=%d, want exactly the newest one", len(frames))
	}
	if got := string(frames[0].data); got != `{"n":999}` {
		t.Fatalf("state=%s, want the newest", got)
	}

	// And once it has read, the next snapshot arrives as usual.
	hub.publishState(topicList, []byte(`{"n":1000}`))
	frames = sub.drain()
	if len(frames) != 1 || string(frames[0].data) != `{"n":1000}` {
		t.Fatalf("frames=%+v", frames)
	}
}

// An operation's outcome must not be lost: the queue accumulates them
// instead of replacing them.
func TestOperationFramesAreQueued(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe("item-1")
	defer hub.unsubscribe(sub)

	for i := 0; i < 5; i++ {
		hub.publishOperationFrame("item-1", []byte(`{"i":`+strconv.Itoa(i)+`}`))
	}
	frames := sub.drain()
	if len(frames) != 5 {
		t.Fatalf("operation frames=%d, want 5", len(frames))
	}
	if string(frames[4].data) != `{"i":4}` {
		t.Fatalf("last frame=%s", frames[4].data)
	}
}

// An item's stream carries both its details and the events of its
// operations.
func TestItemEventsCarryOperations(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	env.refreshState()

	sub := env.hub().subscribe(item.id)
	defer env.hub().unsubscribe(sub)
	env.refreshState() // the first detail snapshot of this topic
	sub.drain()

	op := env.ops().start("test.kind", item.id, nil)
	env.ops().succeed(op.ID, []string{"careful"}, nil)

	seen := false
	for _, frame := range sub.drain() {
		if frame.event != eventOperation {
			continue
		}
		var got operation
		if err := json.Unmarshal(frame.data, &got); err != nil {
			t.Fatalf("operation frame: %v", err)
		}
		if got.ID == op.ID && got.State == opDone && len(got.Warnings) == 1 {
			seen = true
		}
	}
	if !seen {
		t.Fatal("terminal operation event never arrived")
	}
}

// A slow client must not hold up the poller: its frames are dropped, and
// the next snapshot is complete anyway.
func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe(topicList)
	defer hub.unsubscribe(sub)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			hub.publishState(topicList, []byte(`{"n":`+strconv.Itoa(i)+`}`))
			hub.publishOperationFrame(topicList, []byte("{}"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a subscriber that never reads")
	}
}

// --- operation registry ---

func TestOperationsTrackBusyAndEvict(t *testing.T) {
	ops := newOperations(3)
	ops.notify = func(operation) {}

	running := ops.start("k", "item-a", nil)
	if !ops.busy("item-a") {
		t.Fatal("item is not busy while its operation runs")
	}
	for i := 0; i < 5; i++ {
		finished := ops.start("k", "item-b", nil)
		ops.succeed(finished.ID, nil, nil)
	}
	if ops.busy("item-b") {
		t.Fatal("item-b is still busy after all its operations finished")
	}
	// An active operation is never evicted: otherwise the client would lose
	// sight of it mid-work.
	if _, ok := ops.get(running.ID); !ok {
		t.Fatal("running operation was evicted")
	}
	ops.fail(running.ID, errTestFailure, []string{"partial"})
	got, _ := ops.get(running.ID)
	if got.State != opFailed || got.Error == nil || len(got.Warnings) != 1 {
		t.Fatalf("failed operation=%+v", got)
	}
	if got.FinishedAt == nil {
		t.Fatal("failed operation has no finish time")
	}
	if ops.busy("item-a") {
		t.Fatal("item-a is still busy after its operation failed")
	}
}

var errTestFailure = testError("boom")

type testError string

func (e testError) Error() string { return string(e) }
