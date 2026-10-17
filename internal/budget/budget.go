// Package budget turns omp session usage into a per-issue and daily spend and decides when a
// run must wrap up (soft limit) or stop (hard limit).
package budget

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/omp"
	"github.com/paraizo/delamain/internal/store"
)

type Level int

const (
	OK   Level = iota
	Soft       // past soft_ratio of a limit: agents are told to finish
	Hard       // a limit is reached: the run stops
)

func (l Level) String() string { return [...]string{"ok", "soft", "hard"}[l] }

// Status is the spend of one run and of the current day, in the configured mode's unit.
type Status struct {
	Mode            config.BudgetMode
	Usage           store.Usage // the run's raw usage, cacheRead included
	Run, Day        float64
	PerIssue, Daily float64
	Level           Level
	Reason          string
}

// Meter reads session files into the store ledger and evaluates the limits.
type Meter struct {
	Store *store.Store
	Cfg   config.Budget
	Now   func() time.Time
}

// Check records the current usage of every session under sessionsDir (layout <role>/...jsonl)
// and returns the run's status. Safe to call repeatedly: the ledger keeps only deltas.
func (m Meter) Check(ctx context.Context, runID int64, sessionsDir string) (Status, error) {
	files, err := omp.SessionFiles(sessionsDir)
	if err != nil {
		return Status{}, err
	}
	for _, f := range files {
		u, err := omp.SumUsage(f)
		if err != nil {
			return Status{}, err
		}
		role, _, _ := strings.Cut(relSlash(sessionsDir, f), "/")
		if _, err := m.Store.RecordUsage(ctx, runID, role, f, store.Usage(u)); err != nil {
			return Status{}, err
		}
	}
	run, err := m.Store.RunUsage(ctx, runID)
	if err != nil {
		return Status{}, err
	}
	s, err := m.Today(ctx)
	if err != nil {
		return Status{}, err
	}
	s.Usage, s.Run = run, m.spent(run)
	s.evaluate(m.Cfg.SoftRatio)
	return s, nil
}

// Today returns the day's spend over all runs, used to refuse new work once the cap is hit.
func (m Meter) Today(ctx context.Context) (Status, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	t := now()
	day, err := m.Store.UsageSince(ctx, time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()))
	if err != nil {
		return Status{}, err
	}
	s := Status{Mode: m.Cfg.Mode, Day: m.spent(day), PerIssue: m.Cfg.PerIssue, Daily: m.Cfg.Daily}
	s.evaluate(m.Cfg.SoftRatio)
	return s, nil
}

func (m Meter) spent(u store.Usage) float64 {
	if m.Cfg.Mode == config.BudgetUSD {
		return u.CostUSD
	}
	return float64(u.BudgetTokens())
}

func (s *Status) evaluate(soft float64) {
	s.Level, s.Reason = OK, ""
	for _, l := range []struct {
		name         string
		spent, limit float64
	}{{"da issue", s.Run, s.PerIssue}, {"diário", s.Day, s.Daily}} {
		if l.limit <= 0 {
			continue
		}
		switch {
		case l.spent >= l.limit:
			s.Level = Hard
			s.Reason = fmt.Sprintf("budget %s esgotado: %s de %s", l.name, Format(l.spent, s.Mode), Format(l.limit, s.Mode))
			return
		case l.spent >= soft*l.limit && s.Level < Soft:
			s.Level = Soft
			s.Reason = fmt.Sprintf("budget %s em %.0f%%", l.name, 100*l.spent/l.limit)
		}
	}
}

// Left is the budget line agents see in their stage prompt.
func (s Status) Left() string {
	if s.PerIssue <= 0 {
		return "sem limite por issue (consumo atual: " + Format(s.Run, s.Mode) + ")"
	}
	left := max(s.PerIssue-s.Run, 0)
	msg := fmt.Sprintf("restam %s de %s nesta issue (%.0f%% usado)", Format(left, s.Mode), Format(s.PerIssue, s.Mode), 100*s.Run/s.PerIssue)
	if s.Level >= Soft {
		msg += ". ATENÇÃO: budget quase no fim — finalize a fatia atual, escreva o artefato e não abra frentes novas"
	}
	return msg
}

// Summary is the one-line spend for reports, PRs and the herdr sidebar.
func (s Status) Summary() string {
	out := Format(s.Run, s.Mode)
	if s.PerIssue > 0 {
		out += " de " + Format(s.PerIssue, s.Mode)
	}
	if s.Mode != config.BudgetUSD {
		out += fmt.Sprintf(" tokens · US$ %.2f", s.Usage.CostUSD)
	}
	return out
}

// Format renders a spend compactly: 167K, 1.2M tokens or US$ 0.67.
func Format(v float64, mode config.BudgetMode) string {
	if mode == config.BudgetUSD {
		return fmt.Sprintf("US$ %.2f", v)
	}
	switch {
	case v >= 1e6:
		return trim(v/1e6) + "M"
	case v >= 1e3:
		return trim(v/1e3) + "K"
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// trim keeps one decimal below 100 (1.5K, 1.2M) and drops a trailing .0.
func trim(v float64) string {
	if v >= 100 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	v = float64(int64(v*10)) / 10 // truncate: never overstate what is left
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0")
}

func relSlash(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}
