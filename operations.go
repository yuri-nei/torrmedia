package main

// operations.go implements asynchronous operations.
//
// Everything that touches qBittorrent goes through them. The reason is
// simple: deleting an episode may internally wait for the torrent to stop,
// for priority changes and for a full recheck, which takes minutes. While
// such a request was synchronous, the client's timeout was part of the
// protocol, and any mismatch between the two looked like an error on a
// successful operation.
//
// Now the request answers at once, and progress and result arrive over the
// event stream. The client has nothing to time out.

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync"
	"time"
)

const (
	opQueued  = "queued"
	opRunning = "running"
	opDone    = "done"
	opFailed  = "failed"
)

type operation struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Item       string         `json:"item"`
	File       *string        `json:"file"`
	State      string         `json:"state"`
	Phase      *string        `json:"phase"`
	StartedAt  int64          `json:"started_at"`
	FinishedAt *int64         `json:"finished_at"`
	Error      *string        `json:"error"`
	Warnings   []string       `json:"warnings"`
	Result     map[string]any `json:"result"`

	// doomed holds the item's files that this operation deletes. It is not
	// sent to the client: all it needs is that play disappears for these files.
	doomed []string
}

// Operation kinds referenced by the rules (enableQueuesBehind)
const (
	opFileDelete        = "file.delete"
	opItemDeleteWatched = "item." + actionDeleteWatched
	opFileEnable        = "file.enable"
)

// operations stores the operations and knows whether an item is busy.
//
// Finished ones are not removed right away: a client that reconnects after a
// dropped connection must learn the outcome of what it started before.
type operations struct {
	mu     sync.Mutex
	byID   map[string]*operation
	order  []string
	limit  int
	active map[string]int // item id -> number of unfinished operations

	// doomed: item id -> file id -> number of unfinished operations deleting
	// it. Tracked separately rather than as "item is busy": the other ready
	// episodes can be watched while their neighbours are being deleted, since
	// nobody touches their data.
	doomed map[string]map[string]int

	// notify is called outside the mutex with a copy of the operation.
	notify func(operation)
}

func newOperations(limit int) *operations {
	if limit <= 0 {
		limit = 200
	}
	return &operations{
		byID: map[string]*operation{}, active: map[string]int{}, limit: limit,
		doomed: map[string]map[string]int{},
		notify: func(operation) {},
	}
}

// newRandomHex is the shared id generator for operations and drafts.
func newRandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// The only entropy source is unavailable, which is a hardware failure,
		// and handing out predictable ids instead of an error is not acceptable.
		panic("random id: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func newOperationID() string { return "op_" + newRandomHex(6) }

// start registers an operation. doomed holds the ids of the item's files it
// deletes: play is not offered for them until it finishes.
func (o *operations) start(kind, itemID string, fileID *string, doomed ...string) *operation {
	op := &operation{
		ID: newOperationID(), Kind: kind, Item: itemID, File: fileID,
		State: opQueued, StartedAt: time.Now().Unix(), Warnings: []string{},
		doomed: append([]string(nil), doomed...),
	}
	o.mu.Lock()
	o.byID[op.ID] = op
	o.order = append(o.order, op.ID)
	o.active[itemID]++
	if len(op.doomed) > 0 {
		files := o.doomed[itemID]
		if files == nil {
			files = map[string]int{}
			o.doomed[itemID] = files
		}
		for _, id := range op.doomed {
			files[id]++
		}
	}
	o.evictLocked()
	snapshot := *op
	o.mu.Unlock()

	o.notify(snapshot)
	return op
}

// evictLocked evicts the oldest FINISHED operations. An active one is never
// evicted: otherwise the client would lose track of it in the middle of work.
func (o *operations) evictLocked() {
	for len(o.order) > o.limit {
		var kept []string
		removed := false
		for _, id := range o.order {
			op := o.byID[id]
			if !removed && op != nil && op.finished() {
				delete(o.byID, id)
				removed = true
				continue
			}
			kept = append(kept, id)
		}
		o.order = kept
		if !removed {
			return // nothing to evict, all operations are still running
		}
	}
}

func (o *operations) update(id string, mutate func(*operation)) {
	o.mu.Lock()
	op := o.byID[id]
	if op == nil {
		o.mu.Unlock()
		return
	}
	wasFinished := op.finished()
	mutate(op)
	if !wasFinished && op.finished() {
		op.FinishedAt = ptr(time.Now().Unix())
		if o.active[op.Item] > 0 {
			o.active[op.Item]--
		}
		if o.active[op.Item] == 0 {
			delete(o.active, op.Item)
		}
		if files := o.doomed[op.Item]; files != nil {
			for _, id := range op.doomed {
				if files[id]--; files[id] <= 0 {
					delete(files, id)
				}
			}
			if len(files) == 0 {
				delete(o.doomed, op.Item)
			}
		}
	}
	snapshot := *op
	o.mu.Unlock()

	o.notify(snapshot)
}

func (o *operations) phase(id, phase string) {
	o.update(id, func(op *operation) {
		op.State = opRunning
		op.Phase = ptr(phase)
	})
}

func (o *operations) succeed(id string, warnings []string, result map[string]any) {
	o.update(id, func(op *operation) {
		op.State = opDone
		op.Phase = nil
		op.Warnings = append([]string{}, warnings...)
		op.Result = result
	})
}

func (o *operations) fail(id string, err error, warnings []string) {
	o.update(id, func(op *operation) {
		op.State = opFailed
		op.Phase = nil
		op.Error = ptr(err.Error())
		op.Warnings = append([]string{}, warnings...)
	})
}

func (o *operation) finished() bool {
	return o.State == opDone || o.State == opFailed
}

func (o *operations) get(id string) (operation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	op, ok := o.byID[id]
	if !ok {
		return operation{}, false
	}
	return *op, true
}

// busy reports whether the item has an unfinished operation. The client
// learns this not from a separate flag but from an empty actions list:
// there is simply nothing to offer.
func (o *operations) busy(itemID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.active[itemID] > 0
}

// busyBeyond reports whether the item has an unfinished operation whose kind
// is not in allowed. It is needed where an action may queue up behind certain
// operations but not behind any: enabling an episode waits behind deletes of
// neighbouring ones, but not behind a stop, a recheck or removing the item.
func (o *operations) busyBeyond(itemID string, allowed ...string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.active[itemID] == 0 {
		return false
	}
	for _, op := range o.byID {
		if op.Item != itemID || op.finished() {
			continue
		}
		if !slices.Contains(allowed, op.Kind) {
			return true
		}
	}
	return false
}

// dooms reports whether one of the item's unfinished operations deletes the
// file.
func (o *operations) dooms(itemID, fileID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.doomed[itemID][fileID] > 0
}

func ptr[T any](v T) *T { return &v }
