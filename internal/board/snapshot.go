package board

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/artifacts"
)

// Snapshot is the whole board at one moment.
type Snapshot struct {
	Status  Status   `json:"status"`
	Columns []Column `json:"columns"`
}

// Card is a run, or an issue of the queue, on the board.
type Card struct {
	Key           string    `json:"key"` // run-<id> or queue-<position>
	ID            int64     `json:"id,omitempty"`
	Kind          string    `json:"kind"` // issue | review | address | queued
	Repo          string    `json:"repo"`
	Issue         string    `json:"issue"`
	Title         string    `json:"title"`
	State         string    `json:"state,omitempty"`
	Iteration     int       `json:"iteration,omitempty"`
	MaxIterations int       `json:"max_iterations,omitempty"`
	Complexity    string    `json:"complexity,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	PR            string    `json:"pr,omitempty"`
	Live          bool      `json:"live"` // an agent or the orchestrator is working on it
	Outcome       string    `json:"outcome,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Spend         string    `json:"spend,omitempty"`
	Hint          string    `json:"hint,omitempty"` // the command a person runs next
	Rounds        []Round   `json:"rounds,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Round is what the gates and the reviewers said about one iteration (R1, R2…), or about the PR.
type Round struct {
	Label   string `json:"label"`
	Verdict string `json:"verdict,omitempty"`
	Chips   []Chip `json:"chips"`
}

// Chip is one gate or reviewer of a round.
type Chip struct {
	Label string `json:"label"`
	// Tone is pass (no blocking finding, gates green), changes (blocking findings), fail (a gate
	// failed), wait (still running), bad (missing or unreadable) or idle (informative).
	Tone  string `json:"tone"`
	Title string `json:"title"`
}

// Detail is everything the drawer of a card shows: the decisions of each agent.
type Detail struct {
	Card       Card                 `json:"card"`
	Column     string               `json:"column"`
	RunDir     string               `json:"run_dir"`
	Plan       *artifacts.Plan      `json:"plan,omitempty"`
	PlanError  string               `json:"plan_error,omitempty"`
	Iterations []IterationDetail    `json:"iterations"`
	PRReview   *artifacts.PRReview  `json:"pr_review,omitempty"`
	Responses  *artifacts.Responses `json:"responses,omitempty"`
	// ArtifactError says why the PR review or the responses could not be read.
	ArtifactError string  `json:"artifact_error,omitempty"`
	Report        string  `json:"report,omitempty"`
	Agents        []Agent `json:"agents"`
}

// IterationDetail is one implement → verify → review loop.
type IterationDetail struct {
	N           int               `json:"n"`
	Impl        *artifacts.Impl   `json:"impl,omitempty"`
	ImplError   string            `json:"impl_error,omitempty"`
	Gates       []Gate            `json:"gates"`
	Review      *artifacts.Review `json:"review,omitempty"`
	ReviewError string            `json:"review_error,omitempty"`
	Verdict     string            `json:"verdict,omitempty"`
}

// reportLimit bounds the report.md the drawer shows.
const reportLimit = 256 << 10

type builder struct {
	src   Source
	files *files
	limit int // finished runs on the board
}

func (b *builder) snapshot(ctx context.Context) (Snapshot, error) {
	status, err := b.src.Status(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	queue, err := b.src.Queue(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	runs, err := b.src.Runs(ctx, b.limit)
	if err != nil {
		return Snapshot{}, err
	}
	cols := columns()
	at := map[string]int{}
	for i, c := range cols {
		at[c.Key] = i
		cols[i].Cards = []Card{}
	}
	for _, q := range queue {
		c := &cols[at[ColQueue]]
		c.Cards = append(c.Cards, Card{
			Key: fmt.Sprintf("queue-%d", q.Position), Kind: "queued", Repo: filepath.Base(q.Repo),
			Issue: fmt.Sprintf("#%d", q.Issue), Title: q.Title, UpdatedAt: q.AddedAt,
			Hint: fmt.Sprintf("posição %d", q.Position),
		})
	}
	for _, r := range runs {
		c := &cols[at[columnOf(r)]]
		c.Cards = append(c.Cards, b.card(r))
	}
	return Snapshot{Status: status, Columns: cols}, nil
}

func (b *builder) card(r Run) Card {
	c := Card{
		Key: fmt.Sprintf("run-%d", r.ID), ID: r.ID, Kind: r.Kind, Repo: filepath.Base(r.Repo), Issue: r.Issue,
		Title: r.Title, State: r.State, Iteration: r.Iteration, MaxIterations: r.MaxIterations,
		Complexity: r.Complexity, Branch: r.Branch, PR: r.PR, Spend: r.Spend, UpdatedAt: r.UpdatedAt,
		Live: !terminal(r.State) && !r.Orphaned,
	}
	if c.Kind == "" {
		c.Kind = "issue"
	}
	switch {
	case terminal(r.State):
		c.Outcome, c.Reason = outcome(r.Reason), detail(r.Reason)
		if c.Outcome == "" {
			c.Outcome = r.State
		}
		if columnOf(r) == ColHuman {
			c.Hint = fmt.Sprintf("delamain logs %d", r.ID)
		}
	case r.Orphaned:
		c.Outcome, c.Reason, c.Hint = "orphaned", "o processo dono da run morreu", "delamain recover"
	}
	c.Rounds = b.rounds(r)
	return c
}

// Short names of the review axes, the way the chips show them.
var axisLabel = map[string]string{"spec": "spec", "standards": "std", "security": "sec"}

func (b *builder) rounds(r Run) []Round {
	switch r.Kind {
	case "review":
		return b.prRound(r)
	case "address":
		return b.responsesRound(r)
	}
	lg := readRunLog(b.files, r.Dir)
	var out []Round
	for n := 1; n <= r.Iteration; n++ {
		round := Round{Label: fmt.Sprintf("R%d", n)}
		if gates := lg.gates[n]; len(gates) > 0 {
			round.Chips = append(round.Chips, gatesChip(gates))
		}
		rv, ok, err := readJSON[artifacts.Review](b.files, filepath.Join(r.Dir, fmt.Sprintf("iter-%d", n), "review.json"))
		switch {
		case err != nil:
			round.Chips = append(round.Chips, missingAxes("review.json ilegível: "+err.Error())...)
		case ok:
			round.Verdict = rv.EffectiveVerdict()
			round.Chips = append(round.Chips, axisChips(rv.Axes)...)
		case n == r.Iteration && r.State == "review" && !r.Orphaned:
			round.Chips = append(round.Chips, waitingAxes()...)
		case lg.reviewed(n):
			round.Chips = append(round.Chips, missingAxes("a revisão não deixou review.json")...)
		}
		if len(round.Chips) > 0 {
			out = append(out, round)
		}
	}
	return out
}

func (b *builder) prRound(r Run) []Round {
	round := Round{Label: "PR"}
	rv, ok, err := readJSON[artifacts.PRReview](b.files, filepath.Join(r.Dir, "review.json"))
	switch {
	case err != nil:
		round.Chips = missingAxes("review.json ilegível: " + err.Error())
	case ok:
		round.Verdict = rv.Adherence
		round.Chips = axisChips(rv.Axes)
	case r.State == "review" && !r.Orphaned:
		round.Chips = waitingAxes()
	default:
		return nil
	}
	return []Round{round}
}

func (b *builder) responsesRound(r Run) []Round {
	rs, ok, err := readJSON[artifacts.Responses](b.files, filepath.Join(r.Dir, "responses.json"))
	if err != nil {
		return []Round{{Label: "PR", Chips: []Chip{{Label: "respostas?", Tone: "bad", Title: err.Error()}}}}
	}
	if !ok {
		return nil
	}
	count := map[string]int{}
	for _, a := range rs.Responses {
		count[a.Verdict]++
	}
	tones := []struct{ verdict, tone string }{{"valid", "pass"}, {"unclear", "changes"}, {"invalid", "idle"}}
	round := Round{Label: "PR"}
	for _, t := range tones {
		if count[t.verdict] > 0 {
			round.Chips = append(round.Chips, Chip{
				Label: fmt.Sprintf("%s %d", t.verdict, count[t.verdict]), Tone: t.tone,
				Title: fmt.Sprintf("%d discussões julgadas %s", count[t.verdict], t.verdict),
			})
		}
	}
	return []Round{round}
}

func gatesChip(gates []Gate) Chip {
	var failed, all []string
	for _, g := range gates {
		all = append(all, fmt.Sprintf("%s: exit %d", g.Name, g.Exit))
		if g.Exit != 0 {
			failed = append(failed, g.Name)
		}
	}
	if len(failed) > 0 {
		return Chip{Label: "gates ✗", Tone: "fail", Title: strings.Join(all, " · ")}
	}
	return Chip{Label: "gates", Tone: "pass", Title: strings.Join(all, " · ")}
}

// axisChips has one chip per required axis: a reviewer approves its axis when it found nothing
// blocking (critical or major).
func axisChips(axes []artifacts.Axis) []Chip {
	var chips []Chip
	for _, name := range artifacts.RequiredAxes {
		label := axisLabel[name]
		var ax *artifacts.Axis
		for i := range axes {
			if axes[i].Axis == name {
				ax = &axes[i]
			}
		}
		if ax == nil || !ax.Executed {
			chips = append(chips, Chip{Label: label + "?", Tone: "bad", Title: name + ": não executado"})
			continue
		}
		blocking := 0
		for _, f := range ax.Findings {
			if f.Blocking() {
				blocking++
			}
		}
		minor := len(ax.Findings) - blocking
		if blocking > 0 {
			chips = append(chips, Chip{
				Label: fmt.Sprintf("%s %d", label, blocking), Tone: "changes",
				Title: fmt.Sprintf("%s: %d bloqueantes, %d menores", name, blocking, minor),
			})
			continue
		}
		chips = append(chips, Chip{Label: label, Tone: "pass", Title: fmt.Sprintf("%s: aprovado (%d menores)", name, minor)})
	}
	return chips
}

func waitingAxes() []Chip {
	var chips []Chip
	for _, name := range artifacts.RequiredAxes {
		chips = append(chips, Chip{Label: axisLabel[name], Tone: "wait", Title: name + ": revisando"})
	}
	return chips
}

func missingAxes(why string) []Chip {
	var chips []Chip
	for _, name := range artifacts.RequiredAxes {
		chips = append(chips, Chip{Label: axisLabel[name] + "?", Tone: "bad", Title: name + ": " + why})
	}
	return chips
}

func (b *builder) detail(ctx context.Context, id int64) (Detail, error) {
	r, err := b.src.Run(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Card: b.card(r), Column: columnOf(r), RunDir: r.Dir, Agents: r.Agents, Iterations: []IterationDetail{}}
	if d.Agents == nil {
		d.Agents = []Agent{}
	}
	switch r.Kind {
	case "review":
		rv, _, err := readJSON[artifacts.PRReview](b.files, filepath.Join(r.Dir, "review.json"))
		d.PRReview, d.ArtifactError = rv, errText(err)
	case "address":
		rs, _, err := readJSON[artifacts.Responses](b.files, filepath.Join(r.Dir, "responses.json"))
		d.Responses, d.ArtifactError = rs, errText(err)
	default:
		plan, _, err := readJSON[artifacts.Plan](b.files, filepath.Join(r.Dir, "plan.json"))
		d.Plan, d.PlanError = plan, errText(err)
		lg := readRunLog(b.files, r.Dir)
		for n := 1; n <= r.Iteration; n++ {
			d.Iterations = append(d.Iterations, b.iteration(r.Dir, n, lg.gates[n]))
		}
	}
	report, _, _ := b.files.get(filepath.Join(r.Dir, "report.md"), func(data []byte) (any, error) {
		if len(data) > reportLimit {
			data = append(data[:reportLimit:reportLimit], "\n…"...)
		}
		return string(data), nil
	})
	d.Report, _ = report.(string)
	return d, nil
}

func (b *builder) iteration(dir string, n int, gates []Gate) IterationDetail {
	iterDir := filepath.Join(dir, fmt.Sprintf("iter-%d", n))
	it := IterationDetail{N: n, Gates: []Gate{}}
	impl, _, err := readJSON[artifacts.Impl](b.files, filepath.Join(iterDir, "impl.json"))
	it.Impl, it.ImplError = impl, errText(err)
	for _, g := range gates {
		// The name comes from the log: never let it walk out of the iteration directory.
		if g.Exit != 0 && g.Name == filepath.Base(g.Name) && !strings.HasPrefix(g.Name, ".") {
			g.Tail = tail(filepath.Join(iterDir, "verify-"+g.Name+".log"), 40, 4000)
		}
		it.Gates = append(it.Gates, g)
	}
	rv, _, err := readJSON[artifacts.Review](b.files, filepath.Join(iterDir, "review.json"))
	it.Review, it.ReviewError = rv, errText(err)
	if rv != nil {
		it.Verdict = rv.EffectiveVerdict()
	}
	return it
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
