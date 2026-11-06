package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/decide"
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

// dedupItem is one blocker as the review_dedup decision reads it.
type dedupItem struct {
	ID       string `json:"id"`
	Axis     string `json:"axis"`
	File     string `json:"file"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

// dedupMessageBytes caps each message sent to the review_dedup decision.
const dedupMessageBytes = 500

// dedupPrevious caps the previous blockers sent: with nenhum, one choice holds at most 255 options.
const dedupPrevious = 254

// dedupItems numbers bs as <prefix>_<i>, in order, each message cut to dedupMessageBytes.
func dedupItems(prefix string, bs []seenBlocker) []dedupItem {
	items := make([]dedupItem, len(bs))
	for i, b := range bs {
		items[i] = dedupItem{
			ID: fmt.Sprintf("%s_%d", prefix, i), Axis: b.Axis, File: b.File, Category: b.Category,
			Message: clipBytes(b.Message, dedupMessageBytes),
		}
	}
	return items
}

// dedupState is the state of the review_dedup decision: the fresh blockers and the most recent
// dedupPrevious of previous, dropping the oldest until the JSON — measured encoded, since
// json.Marshal escapes <, > and & — fits under checkStateBytes. It returns the previous it kept.
func dedupState(previous, fresh []seenBlocker) (map[string]any, []seenBlocker) {
	previous = previous[max(0, len(previous)-dedupPrevious):]
	news := dedupItems("new", fresh)
	state := func(prev []seenBlocker) map[string]any {
		return map[string]any{"previous": dedupItems("prev", prev), "new": news}
	}
	// Dropping the oldest only shrinks the JSON, so the first start that fits is found by halving.
	start := sort.Search(len(previous), func(i int) bool {
		b, err := json.Marshal(state(previous[i:]))
		return err != nil || len(b) < checkStateBytes
	})
	return state(previous[start:]), previous[start:]
}

// novelBlockers records the review's blocking findings in seen and reports whether every one of
// them is new to the run. Findings are compared by findingKey, not by message; with the
// review_dedup point on, a blocker new by key that the model matches to a previous one counts as
// seen, so a reworded blocker does not pass for a new layer of review.
func (r *Runner) novelBlockers(ctx context.Context, ru *run, iter int, seen *seenBlockers, review artifacts.Review) bool {
	allNew := true
	keys := map[string]bool{}
	var all, fresh []seenBlocker
	for _, a := range review.Axes {
		for _, f := range a.Findings {
			if !f.Blocking() {
				continue
			}
			b := seenBlocker{Axis: a.Axis, File: f.File, Category: f.Category, Message: f.Message, Line: f.Line}
			if k := b.key(); seen.keys[k] || keys[k] {
				allNew = false
			} else {
				keys[k] = true
				fresh = append(fresh, b)
			}
			all = append(all, b)
		}
	}
	if len(fresh) > 0 && len(seen.list) > 0 && r.matchBlockers(ctx, ru, iter, seen.list, fresh) {
		allNew = false
	}
	for _, b := range all {
		seen.keys[b.key()] = true
	}
	seen.list = append(seen.list, all...)
	return allNew
}

// matchBlockers asks, in one request, whether each blocker of fresh is the same problem as one of
// previous; true means at least one is, with its probability at the threshold or above. Shadow
// only logs the decision. No previous fitting the state skips the question, and a missing answer
// leaves the whole review to the keys.
func (r *Runner) matchBlockers(ctx context.Context, ru *run, iter int, previous, fresh []seenBlocker) bool {
	state, previous := dedupState(previous, fresh)
	if len(previous) == 0 {
		return false
	}
	options := map[string]string{"nenhum": "É um problema diferente de todos os anteriores."}
	byID := map[string]seenBlocker{}
	for j, it := range dedupItems("prev", previous) {
		options[it.ID] = fmt.Sprintf("Mesmo problema que %s: %s %s — %s", it.ID, it.Axis, it.File, it.Message)
		byID[it.ID] = previous[j]
	}
	req := decide.Request{State: state, Choices: map[string]decide.Choice{}}
	for i := range fresh {
		id := fmt.Sprintf("new_%d", i)
		req.Choices[id] = decide.Choice{
			Instructions: "O bloqueante " + id + " descreve o mesmo problema que algum bloqueante anterior?",
			Options:      options,
		}
	}
	attrs := []any{"run", ru.rec.ID, "iteration", iter, "new_blockers", len(fresh)}
	p := r.Cfg.Decisions.ReviewDedup
	res, ok := r.ask(ctx, ru.log, "review_dedup", p, req, attrs...)
	if !ok || p.Mode != config.DecisionOn {
		return false
	}
	type match struct {
		b, prev seenBlocker
		p       float64
	}
	var matches []match
	for i, b := range fresh {
		key := fmt.Sprintf("new_%d", i)
		a, answered := res.Answers[key]
		if !answered {
			ru.log.Warn("decision skipped", append([]any{"point", "review_dedup", "reason", "no answer to " + key}, attrs...)...)
			return false
		}
		if pb, known := byID[a.Choice]; known && a.Probabilities[a.Choice] >= p.Threshold {
			matches = append(matches, match{b, pb, a.Probabilities[a.Choice]})
		}
	}
	for _, m := range matches {
		ru.log.Info("review blocker matched", "run", ru.rec.ID, "new", m.b.key(), "previous", m.prev.key(), "p", m.p)
	}
	return len(matches) > 0
}
