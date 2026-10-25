package board

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// files reads the artifacts of the runs, reparsing a file only when its size or mtime changed:
// the board reloads every run on every tick.
type files struct {
	mu sync.Mutex
	m  map[string]parsed
}

type parsed struct {
	mod  time.Time
	size int64
	v    any
	err  error
}

// maxCached bounds the cache: the runs that left the board leave their entries behind.
const maxCached = 4096

func newFiles() *files { return &files{m: map[string]parsed{}} }

// get returns the file at path decoded, false when it does not exist, or the error that kept it
// from being read or decoded (an agent may be halfway through writing it).
func (f *files) get(path string, decode func([]byte) (any, error)) (any, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	f.mu.Lock()
	p, hit := f.m[path]
	f.mu.Unlock()
	if hit && p.mod.Equal(info.ModTime()) && p.size == info.Size() {
		return p.v, true, p.err
	}
	data, err := os.ReadFile(path) //nolint:gosec // inside a run directory taken from the state database
	p = parsed{mod: info.ModTime(), size: info.Size(), err: err}
	if err == nil {
		p.v, p.err = decode(data)
	}
	f.mu.Lock()
	if len(f.m) >= maxCached {
		clear(f.m)
	}
	f.m[path] = p
	f.mu.Unlock()
	return p.v, true, p.err
}

// readJSON reads an artifact. It decodes leniently: the board shows what an agent wrote even
// when the orchestrator later rejects it, and a field it does not know is no reason to hide it.
func readJSON[T any](f *files, path string) (*T, bool, error) {
	v, ok, err := f.get(path, func(data []byte) (any, error) {
		t := new(T)
		return t, json.Unmarshal(data, t)
	})
	if !ok || err != nil {
		return nil, ok, err
	}
	return v.(*T), true, nil
}

// Gate is one check the orchestrator ran in an iteration.
type Gate struct {
	Name string `json:"name"`
	Exit int    `json:"exit"`
	Tail string `json:"tail,omitempty"` // the end of its log, when it failed
}

// runLog is what the orchestrator's log of a run says about each iteration: the states it went
// through and the gates it ran.
type runLog struct {
	states map[int]map[string]bool
	gates  map[int][]Gate
}

// reviewed reports whether iteration iter got past its gates to the review.
func (l runLog) reviewed(iter int) bool { return l.states[iter]["review"] }

// readRunLog parses logs/delamain.log, whose lines are "15:04:05 [LEVEL] msg k=v ...".
func readRunLog(f *files, dir string) runLog {
	v, ok, err := f.get(filepath.Join(dir, "logs", "delamain.log"), func(data []byte) (any, error) {
		return parseRunLog(data), nil
	})
	if !ok || err != nil {
		return runLog{}
	}
	return v.(runLog)
}

func parseRunLog(data []byte) runLog {
	l := runLog{states: map[int]map[string]bool{}, gates: map[int][]Gate{}}
	iter := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		msg, kv := parseLine(sc.Text())
		switch msg {
		case "state":
			if n, err := strconv.Atoi(kv["iteration"]); err == nil {
				iter = n
			}
			if l.states[iter] == nil {
				l.states[iter] = map[string]bool{}
			}
			l.states[iter][kv["state"]] = true
		case "gate":
			code, err := strconv.Atoi(kv["exit"])
			if err != nil || iter == 0 {
				continue
			}
			gates := l.gates[iter]
			for i, g := range gates {
				if g.Name == kv["name"] {
					gates = append(gates[:i], gates[i+1:]...)
					break
				}
			}
			l.gates[iter] = append(gates, Gate{Name: kv["name"], Exit: code})
		}
	}
	return l
}

func parseLine(line string) (string, map[string]string) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil
	}
	fields = fields[1:] // the time
	switch fields[0] {
	case "DEBUG", "WARN", "ERROR":
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return "", nil
	}
	kv := map[string]string{}
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		}
		kv[k] = v
	}
	return fields[0], kv
}

// tail is the end of a log, at most lines lines and maxBytes bytes.
func tail(path string, lines, maxBytes int) string {
	b, err := os.ReadFile(path) //nolint:gosec // inside a run directory taken from the state database
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	out := strings.Join(all, "\n")
	if len(out) > maxBytes {
		out = "…" + out[len(out)-maxBytes:]
	}
	return out
}
