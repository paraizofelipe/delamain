package control

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// OpKind is what an Operation does.
type OpKind string

const (
	OpStartRun    OpKind = "start_run"
	OpStartReview OpKind = "start_review"
	OpRecover     OpKind = "recover"
)

// OpStatus is where an Operation stands.
type OpStatus string

const (
	OpRunning   OpStatus = "running"
	OpSucceeded OpStatus = "succeeded"
	OpFailed    OpStatus = "failed"
)

// Operation is an action that outlives the call that asked for it: a run or review started as a
// process of its own, or the recovery of an orphan. The remote API answers with it and reports
// its changes over the event stream.
type Operation struct {
	ID     string
	Kind   OpKind
	Status OpStatus
	Repo   string
	Issue  int   // for OpStartRun
	PR     int   // for OpStartReview
	RunID  int64 // the run it started or recovered, once known
	// ExitCode is the process's exit code, once it ended; nil while running or for OpRecover.
	ExitCode *int
	// Error says why it failed; Result how a recover ended (e.g. "draft PR").
	Error, Result string
	// LogPath is the launch log; LogTail its last bytes once the process ended.
	LogPath, LogTail string
	StartedAt        time.Time
	EndedAt          *time.Time
}

// opRing bounds how many operations are kept.
const opRing = 200

// Operations keeps the recent operations and tells its subscribers of every change.
type Operations struct {
	mu   sync.Mutex
	ops  []*Operation // oldest first
	subs map[chan Operation]struct{}
	seq  int
	now  func() time.Time
}

func NewOperations() *Operations {
	return &Operations{subs: map[chan Operation]struct{}{}, now: time.Now}
}

// Get returns operation id.
func (o *Operations) Get(id string) (Operation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, op := range o.ops {
		if op.ID == id {
			return *op, true
		}
	}
	return Operation{}, false
}

// List returns up to limit operations, newest first; limit <= 0 means every one kept.
func (o *Operations) List(limit int) []Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Operation, 0, len(o.ops))
	for i := len(o.ops) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, *o.ops[i])
	}
	return out
}

// Subscribe delivers every change to a channel until unsubscribe is called. A subscriber that
// does not read loses the changes it did not take: the operations themselves stay in List.
func (o *Operations) Subscribe() (<-chan Operation, func()) {
	ch := make(chan Operation, 64)
	o.mu.Lock()
	o.subs[ch] = struct{}{}
	o.mu.Unlock()
	return ch, func() {
		o.mu.Lock()
		delete(o.subs, ch)
		o.mu.Unlock()
	}
}

// add records op as running and tells the subscribers.
func (o *Operations) add(op Operation) Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	op.ID = fmt.Sprintf("op-%d-%d", o.now().Unix(), o.seq)
	op.Status, op.StartedAt = OpRunning, o.now()
	o.ops = append(o.ops, &op)
	if len(o.ops) > opRing {
		o.ops = slices.Delete(o.ops, 0, len(o.ops)-opRing)
	}
	o.publish(op)
	return op
}

// update changes operation id and tells the subscribers.
func (o *Operations) update(id string, mutate func(*Operation)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, op := range o.ops {
		if op.ID == id {
			mutate(op)
			o.publish(*op)
			return
		}
	}
}

// finish ends operation id with status and tells the subscribers.
func (o *Operations) finish(id string, status OpStatus, mutate func(*Operation)) {
	o.update(id, func(op *Operation) {
		mutate(op)
		ended := o.now()
		op.Status, op.EndedAt = status, &ended
	})
}

func (o *Operations) publish(op Operation) {
	for ch := range o.subs {
		select {
		case ch <- op:
		default:
		}
	}
}
