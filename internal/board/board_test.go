package board

import "testing"

func TestColumnOf(t *testing.T) {
	cases := []struct {
		name string
		run  Run
		want string
	}{
		{"claimed", Run{State: "claimed"}, ColSetup},
		{"setup", Run{State: "setup"}, ColSetup},
		{"plan", Run{State: "plan"}, ColPlan},
		{"implement", Run{State: "implement"}, ColImplement},
		{"verify", Run{State: "verify"}, ColVerify},
		{"review", Run{State: "review"}, ColReview},
		{"PR review", Run{State: "review", Kind: "review"}, ColReview},
		{"deliver", Run{State: "deliver"}, ColDeliver},
		{"deliver draft", Run{State: "deliver_draft"}, ColDeliver},
		{"approved", Run{State: "recorded", Reason: "approved"}, ColDone},
		{"approved with why", Run{State: "recorded", Reason: "approved: PR aberto"}, ColDone},
		{"reviewed", Run{State: "recorded", Reason: "reviewed: 3 comentários", Kind: "review"}, ColDone},
		{"answered", Run{State: "recorded", Reason: "answered: 2 respostas", Kind: "address"}, ColDone},
		{"skipped", Run{State: "recorded", Reason: "skipped: o PR/MR #3 não está aberto"}, ColDone},
		{"exhausted", Run{State: "recorded", Reason: "exhausted: 3 iterações"}, ColHuman},
		{"halted", Run{State: "recorded", Reason: "halted: parada pedida"}, ColHuman},
		{"halted on the way to recorded", Run{State: "halted"}, ColHuman},
		{"needs info", Run{State: "needs_info"}, ColHuman},
		{"recorded without reason", Run{State: "recorded"}, ColHuman},
		{"orphaned", Run{State: "implement", Orphaned: true}, ColHuman},
		{"orphaned is ignored once recorded", Run{State: "recorded", Reason: "approved", Orphaned: true}, ColDone},
		{"unknown state", Run{State: "preflight"}, ColSetup},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := columnOf(c.run); got != c.want {
				t.Fatalf("columnOf(%+v) = %s, want %s", c.run, got, c.want)
			}
		})
	}
}

func TestParseRunLog(t *testing.T) {
	log := `10:00:00 state run=7 state=setup iteration=0
10:00:01 gate run=7 name=baseline exit=0
10:00:02 state run=7 state=plan iteration=0
10:01:00 state run=7 state=implement iteration=1
10:02:00 state run=7 state=verify iteration=1
10:02:01 gate run=7 name=test exit=1
10:02:02 gate run=7 name=lint exit=0
10:03:00 state run=7 state=implement iteration=2
10:04:00 state run=7 state=verify iteration=2
10:04:01 gate run=7 name=test exit=0
10:04:02 WARN gate run=7 name=lint exit=0
10:04:03 state run=7 state=review iteration=2
garbage
`
	l := parseRunLog([]byte(log))
	if got := l.gates[1]; len(got) != 2 || got[0] != (Gate{Name: "test", Exit: 1}) || got[1] != (Gate{Name: "lint"}) {
		t.Fatalf("gates of iteration 1 = %+v", got)
	}
	if got := l.gates[2]; len(got) != 2 {
		t.Fatalf("gates of iteration 2 = %+v", got)
	}
	if len(l.gates[0]) != 0 {
		t.Fatalf("gates before the first iteration count: %+v", l.gates[0])
	}
	if l.reviewed(1) || !l.reviewed(2) {
		t.Fatalf("reviewed: iteration 1 %v, iteration 2 %v", l.reviewed(1), l.reviewed(2))
	}
}
