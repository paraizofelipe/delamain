package omp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Usage is the provider-reported consumption of a session.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
	CostUSD                              float64
}

type usageEntry struct {
	Type    string `json:"type"`
	Message struct {
		Role  string `json:"role"`
		Usage *struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
			Cost       struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

// SumUsage adds the usage of every assistant turn in a session file.
func SumUsage(path string) (Usage, error) {
	f, err := os.Open(path) //nolint:gosec // session files live in the run's own session dir
	if err != nil {
		return Usage{}, err
	}
	defer f.Close()
	var u Usage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var e usageEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "message" || e.Message.Role != "assistant" || e.Message.Usage == nil {
			continue
		}
		m := e.Message.Usage
		u.Input += m.Input
		u.Output += m.Output
		u.CacheRead += m.CacheRead
		u.CacheWrite += m.CacheWrite
		u.CostUSD += m.Cost.Total
	}
	if err := sc.Err(); err != nil {
		return u, fmt.Errorf("read %s: %w", path, err)
	}
	return u, nil
}

// SessionFiles lists every *.jsonl under dir, subagent sessions included, sorted.
// A missing dir has no sessions yet.
func SessionFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	sort.Strings(out)
	return out, err
}
