// Package omp reads omp session files (JSONL, format v3).
package omp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Assistant is the part of an assistant message the orchestrator needs.
type Assistant struct {
	StopReason   string
	ErrorMessage string
	Model        string
}

// ErrNoAssistant means the session has no assistant reply yet.
var ErrNoAssistant = errors.New("omp session has no assistant message")

type entry struct {
	Type    string `json:"type"`
	Message struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
		Model        string `json:"model"`
	} `json:"message"`
}

// LastAssistant returns the last assistant message of a session file.
// herdr reports `done` even when the provider failed (M0), so callers check StopReason.
func LastAssistant(path string) (Assistant, error) {
	f, err := os.Open(path) //nolint:gosec // session path comes from herdr or the run's own session dir
	if err != nil {
		return Assistant{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var last *Assistant
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "message" || e.Message.Role != "assistant" {
			continue
		}
		last = &Assistant{StopReason: e.Message.StopReason, ErrorMessage: e.Message.ErrorMessage, Model: e.Message.Model}
	}
	if err := sc.Err(); err != nil {
		return Assistant{}, fmt.Errorf("read %s: %w", path, err)
	}
	if last == nil {
		return Assistant{}, ErrNoAssistant
	}
	return *last, nil
}

// Failed reports a provider or runtime failure that ended the turn.
func (a Assistant) Failed() bool { return a.StopReason == "error" }

// LatestSession returns the most recently modified top-level *.jsonl in dir.
// Subagent sessions live in subdirectories and are skipped.
func LatestSession(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var best string
	var bestInfo fs.FileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if bestInfo == nil || info.ModTime().After(bestInfo.ModTime()) {
			best, bestInfo = filepath.Join(dir, e.Name()), info
		}
	}
	if best == "" {
		return "", fmt.Errorf("no omp session in %s", dir)
	}
	return best, nil
}
