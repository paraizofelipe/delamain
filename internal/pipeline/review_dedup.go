package pipeline

import (
	"context"

	"github.com/paraizo/delamain/internal/artifacts"
)

// seenBlocker is a blocking finding already raised in the run, kept with its text so a reworded
// one can be told apart from a new problem.
type seenBlocker struct {
	Axis, File, Category, Message string
	Line                          int
}

// key is the blocker's findingKey identity.
func (b seenBlocker) key() string {
	return findingKey(b.Axis, artifacts.Finding{File: b.File, Category: b.Category})
}

// seenBlockers holds every blocker raised in the run: keys by findingKey, list in arrival order.
type seenBlockers struct {
	keys map[string]bool
	list []seenBlocker
}

func newSeenBlockers() *seenBlockers { return &seenBlockers{keys: map[string]bool{}} }

// novelBlockers records the review's blocking findings in seen and reports whether every one of
// them is new to the run. Findings are compared by findingKey, not by message.
func (r *Runner) novelBlockers(_ context.Context, _ *run, _ int, seen *seenBlockers, review artifacts.Review) bool {
	allNew := true
	for _, a := range review.Axes {
		for _, f := range a.Findings {
			if !f.Blocking() {
				continue
			}
			b := seenBlocker{Axis: a.Axis, File: f.File, Category: f.Category, Message: f.Message, Line: f.Line}
			k := b.key()
			if seen.keys[k] {
				allNew = false
			}
			seen.keys[k] = true
			seen.list = append(seen.list, b)
		}
	}
	return allNew
}
