package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

func TestEventsPublishOnlyOnChange(t *testing.T) {
	env := newTestEnv(t)
	env.refreshState()

	sub := env.hub().subscribe(topicList)
	defer env.hub().unsubscribe(sub)

	env.refreshState()
	select {
	case frame := <-sub.ch:
		t.Fatalf("unchanged state was published: %s %s", frame.event, frame.data)
	case <-time.After(50 * time.Millisecond):
	}

	env.fake.mu.Lock()
	env.fake.torrents[0].state = "uploading"
	env.fake.mu.Unlock()
	env.refreshState()
	select {
	case frame := <-sub.ch:
		if frame.event != eventState || !strings.Contains(string(frame.data), statusSeeding) {
			t.Fatalf("frame=%s %s", frame.event, frame.data)
		}
	case <-time.After(time.Second):
		t.Fatal("changed state was not published")
	}
}

func TestItemEventsCarryOperations(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	env.refreshState()

	sub := env.hub().subscribe(item.id)
	defer env.hub().unsubscribe(sub)
	env.refreshState()
	<-sub.ch

	op := env.ops().start("test.kind", item.id, nil)
	env.ops().succeed(op.ID, []string{"careful"}, nil)

	deadline := time.After(2 * time.Second)
	seen := false
	for !seen {
		select {
		case frame := <-sub.ch:
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
		case <-deadline:
			t.Fatal("operation event never arrived")
		}
	}
}

func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe(topicList)
	defer hub.unsubscribe(sub)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			hub.publish(topicList, sseFrame{event: eventState, data: []byte("{}")})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a subscriber that never reads")
	}
}

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
