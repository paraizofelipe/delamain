package notify

import (
	"testing"

	"github.com/paraizo/delamain/internal/config"
)

func TestSampleShouldCoverEveryNotifyEvent(t *testing.T) {
	for _, e := range config.NotifyEvents {
		m, err := Sample(Event(e))
		if err != nil {
			t.Errorf("%s: %v", e, err)
			continue
		}
		if m.Event != Event(e) || m.Title == "" || len(m.Actions) != 0 {
			t.Errorf("%s: the sample is its event's toast, without actions: %+v", e, m)
		}
	}
}

func TestSampleShouldShowTheIterationExample(t *testing.T) {
	m, err := Sample(EventIteration)
	if err != nil {
		t.Fatal(err)
	}
	want := "Mostrar HARNESS e INTERACTIONS no status\nrun 42 · 1.2M tokens · US$ 0.67\nreview: 3 blocking findings (spec, security)"
	if m.Title != "🔁 Iteration 2/12 · delamain #12" || m.Body != want {
		t.Fatalf("sample = %q / %q, want body %q", m.Title, m.Body, want)
	}
}
