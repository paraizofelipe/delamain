package pipeline

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/paraizo/delamain/internal/omp/rpc"
)

// Agents starts omp sessions. The pipeline drives them directly; herdr only shows their logs.
type Agents interface {
	Start(ctx context.Context, o AgentOptions) (Agent, error)
}

// Agent is one running omp session.
type Agent interface {
	// Prompt runs one turn to its end; on timeout the turn is aborted and rpc.ErrTimeout returned.
	Prompt(ctx context.Context, text string, timeout time.Duration) (rpc.Turn, error)
	// Abort stops the turn in flight, which then ends with StopReason "aborted".
	Abort() error
	Close() error
}

type AgentOptions struct {
	Name    string // e.g. implementer-r7, for logs
	Dir     string
	Args    []string
	Env     []string // wins over the inherited environment
	LogPath string   // human-readable event log, shown in the role's herdr tab
	ErrPath string   // omp's stderr
}

// RPCAgents runs each session as `omp --mode rpc`.
type RPCAgents struct{ Binary string }

func (a RPCAgents) Start(ctx context.Context, o AgentOptions) (Agent, error) {
	logf, err := os.OpenFile(o.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	errf, err := os.OpenFile(o.ErrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = logf.Close()
		return nil, err
	}
	c, err := rpc.Start(ctx, rpc.Options{
		Binary: a.Binary, Dir: o.Dir, Args: o.Args, Env: o.Env,
		Log: logf, Stderr: errf, ReadyTimeout: startTimeout,
	})
	if err != nil {
		_, _ = logf.Close(), errf.Close()
		return nil, err
	}
	return &rpcAgent{Client: c, files: []*os.File{logf, errf}}, nil
}

type rpcAgent struct {
	*rpc.Client
	files []*os.File
}

func (a *rpcAgent) Close() error {
	err := a.Client.Close()
	for _, f := range a.files {
		err = errors.Join(err, f.Close())
	}
	return err
}
