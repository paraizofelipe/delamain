// Package decide asks a System One decision model (Jev via OpenRouter) typed questions.
package decide

import (
	"context"
	"errors"
	"time"
)

// Noul is a yes/no question: the answer is the probability of True.
type Noul struct{ Instructions, True, False string }

// Choice is a question answered with one of its options.
type Choice struct {
	Instructions string
	Options      map[string]string // option key -> criterion
}

// Request is the state the model reads and the questions it answers, keyed by name.
type Request struct {
	State   map[string]any
	Nouls   map[string]Noul
	Choices map[string]Choice
}

// Answer is the model's answer to one question.
type Answer struct {
	Noul          float64            // only for nouls
	Choice        string             // only for choices
	Confidence    float64            // only for choices
	Probabilities map[string]float64 // only for choices
}

// Result holds the answers to every question of a Request.
type Result struct {
	Answers map[string]Answer
	Model   string        // the snapshot that answered (response "model")
	CostUSD float64       // response usage.cost
	Latency time.Duration // measured by the client
}

// Decider answers a Request. Callers treat any error as "no decision" and fall back.
type Decider interface {
	Decide(ctx context.Context, req Request) (Result, error)
}

var (
	ErrStateTooLarge = errors.New("decision state exceeds 32000 bytes")
	ErrNoQuestions   = errors.New("decision request has no questions")
)
