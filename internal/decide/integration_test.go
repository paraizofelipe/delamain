package decide

import (
	"context"
	"os"
	"testing"
	"time"
)

// Contract tests against the real OpenRouter Decisions API. They spend a fraction of a cent:
//
//	DELAMAIN_IT_DECISIONS_MODEL=typesafe/jev-1.14 DELAMAIN_DECISIONS_API_KEY=... go test ./internal/decide/ -run Integration
func integrationClient(t *testing.T) *Client {
	t.Helper()
	model, key := os.Getenv("DELAMAIN_IT_DECISIONS_MODEL"), os.Getenv("DELAMAIN_DECISIONS_API_KEY")
	if model == "" || key == "" {
		t.Skip("set DELAMAIN_IT_DECISIONS_MODEL and DELAMAIN_DECISIONS_API_KEY to run decisions integration tests")
	}
	return New(Options{BaseURL: "https://openrouter.ai/api", APIKey: key, Model: model, Timeout: 30 * time.Second})
}

func TestIntegrationDecideShouldAnswerNoulAndChoiceFromOpenRouter(t *testing.T) {
	c := integrationClient(t)
	res, err := c.Decide(context.Background(), bugTriage())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Answers["is_bug"]; !ok {
		t.Fatalf("no answer to the noul: %+v", res.Answers)
	}
	team, ok := res.Answers["team"]
	if !ok {
		t.Fatalf("no answer to the choice: %+v", res.Answers)
	}
	if _, ok := bugTriage().Choices["team"].Options[team.Choice]; !ok {
		t.Fatalf("choice %q is not one of the options", team.Choice)
	}
	if len(team.Probabilities) == 0 {
		t.Fatalf("choice has no probabilities: %+v", team)
	}
	if res.Model == "" || res.Latency <= 0 {
		t.Fatalf("result lacks model or latency: %+v", res)
	}
}
