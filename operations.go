package main

import (
	"crypto/rand"
	"encoding/hex"
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
}

type operations struct {
	mu     sync.Mutex
	byID   map[string]*operation
	order  []string
	limit  int
	active map[string]int

	notify func(operation)
}

func newOperations(limit int) *operations {
	if limit <= 0 {
		limit = 200
	}
	return &operations{
		byID: map[string]*operation{}, active: map[string]int{}, limit: limit,
		notify: func(operation) {},
	}
}

func newRandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("random id: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func newOperationID() string { return "op_" + newRandomHex(6) }

func (o *operations) start(kind, itemID string, fileID *string) *operation {
	op := &operation{
		ID: newOperationID(), Kind: kind, Item: itemID, File: fileID,
		State: opQueued, StartedAt: time.Now().Unix(), Warnings: []string{},
	}
	o.mu.Lock()
	o.byID[op.ID] = op
	o.order = append(o.order, op.ID)
	o.active[itemID]++
	o.evictLocked()
	snapshot := *op
	o.mu.Unlock()

	o.notify(snapshot)
	return op
}

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
			return
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

func (o *operations) busy(itemID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.active[itemID] > 0
}

func ptr[T any](v T) *T { return &v }
