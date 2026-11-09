package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// LogLine is one line of a run's logs. TS is the HH:MM:SS stamp of the line, or of the stamped
// line before it in the same source (gate output has none of its own).
type LogLine struct {
	TS, Source, Text string
}

// ErrInvalidCursor is a log cursor that did not come from Cursor.Encode.
var ErrInvalidCursor = errors.New("invalid log cursor")

// Cursor is where a LogReader stands in each logs/<source>.log: the bytes read, the trailing
// line not yet terminated and the stamp of the last stamped line. Encode makes it an opaque
// string the remote API hands to its client and takes back.
type Cursor struct {
	Offsets map[string]int64  `json:"o,omitempty"`
	Partial map[string][]byte `json:"p,omitempty"`
	LastTS  map[string]string `json:"t,omitempty"`
}

// Encode is the cursor as an opaque string; an empty cursor is "".
func (c Cursor) Encode() string {
	if len(c.Offsets) == 0 && len(c.Partial) == 0 && len(c.LastTS) == 0 {
		return ""
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ParseCursor reads what Encode wrote; "" is the start of the logs.
func ParseCursor(s string) (Cursor, error) {
	var c Cursor
	if s == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: %w", ErrInvalidCursor, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Cursor{}, fmt.Errorf("%w: %w", ErrInvalidCursor, err)
	}
	for source, off := range c.Offsets {
		if off < 0 || !validSource(source) {
			return Cursor{}, fmt.Errorf("%w: offset of %q", ErrInvalidCursor, source)
		}
	}
	for source := range c.Partial {
		if !validSource(source) {
			return Cursor{}, fmt.Errorf("%w: partial of %q", ErrInvalidCursor, source)
		}
	}
	for source, ts := range c.LastTS {
		if !validSource(source) || !stampRe.MatchString(ts+" ") {
			return Cursor{}, fmt.Errorf("%w: stamp of %q", ErrInvalidCursor, source)
		}
	}
	return c, nil
}

// sourceRe is a log source name: a file name under logs/, never a path.
var sourceRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func validSource(s string) bool { return sourceRe.MatchString(s) && s != "." && s != ".." }

// ValidSource reports whether roles names a log source: letters, digits, dot, dash, underscore.
func ValidSource(s string) bool { return validSource(s) }

// LogReader reads a run's logs/<source>.log files incrementally: each Read returns the lines
// appended since the last one, every source interleaved by its stamp.
type LogReader struct {
	dir     string
	roles   []string          // only these sources; nil means all
	offsets map[string]int64  // source -> bytes already read
	partial map[string][]byte // source -> trailing line not yet terminated
	lastTS  map[string]string // source -> time of its last stamped line
	maxRead int64             // bytes one Read takes of each file; 0 means all
	behind  bool              // the last Read left bytes unread in some file
}

var stampRe = regexp.MustCompile(`^\d\d:\d\d:\d\d `)

// NewLogReader reads the logs under dir, restricted to roles when given.
func NewLogReader(dir string, roles []string) *LogReader {
	return &LogReader{dir: dir, roles: roles, offsets: map[string]int64{}, partial: map[string][]byte{}, lastTS: map[string]string{}}
}

// LimitRead bounds what each Read takes of each file to maxBytes, so a Read of a long log stays
// bounded: the rest comes with the next calls, and Behind reports that it is there.
func (r *LogReader) LimitRead(maxBytes int64) { r.maxRead = maxBytes }

// Behind reports whether the last Read left bytes unread in some file because of LimitRead.
func (r *LogReader) Behind() bool { return r.behind }

// Dir is the logs directory the reader reads.
func (r *LogReader) Dir() string { return r.dir }

// Cursor is where the reader stands, to Seek back to later.
func (r *LogReader) Cursor() Cursor {
	c := Cursor{Offsets: map[string]int64{}, Partial: map[string][]byte{}, LastTS: map[string]string{}}
	for k, v := range r.offsets {
		c.Offsets[k] = v
	}
	for k, v := range r.partial {
		if len(v) > 0 {
			c.Partial[k] = slices.Clone(v)
		}
	}
	for k, v := range r.lastTS {
		c.LastTS[k] = v
	}
	return c
}

// Seek puts the reader where c stands.
func (r *LogReader) Seek(c Cursor) {
	r.offsets, r.partial, r.lastTS = map[string]int64{}, map[string][]byte{}, map[string]string{}
	for k, v := range c.Offsets {
		r.offsets[k] = v
	}
	for k, v := range c.Partial {
		r.partial[k] = slices.Clone(v)
	}
	for k, v := range c.LastTS {
		r.lastTS[k] = v
	}
}

// Read returns the lines appended since the last call, ordered by their time stamp. flush also
// returns unterminated last lines (the run has ended, nothing more will complete them) of the
// files read to their end.
func (r *LogReader) Read(flush bool) ([]LogLine, error) {
	paths, err := filepath.Glob(filepath.Join(r.dir, "*.log"))
	if err != nil {
		return nil, err
	}
	r.behind = false
	var out []LogLine
	for _, p := range paths {
		source := strings.TrimSuffix(filepath.Base(p), ".log")
		if len(r.roles) > 0 && !slices.Contains(r.roles, source) {
			continue
		}
		chunk, atEnd, err := r.readFrom(p, source)
		if err != nil {
			return nil, err
		}
		r.behind = r.behind || !atEnd
		data := append(r.partial[source], chunk...)
		cut := bytes.LastIndexByte(data, '\n') + 1
		if flush && atEnd {
			cut = len(data)
		}
		r.partial[source] = slices.Clone(data[cut:])
		if cut == 0 {
			continue
		}
		for _, text := range strings.Split(strings.TrimSuffix(string(data[:cut]), "\n"), "\n") {
			if stampRe.MatchString(text) {
				r.lastTS[source] = text[:8]
			}
			// A line without a stamp (gate output) belongs with the stamped line before it.
			out = append(out, LogLine{TS: r.lastTS[source], Source: source, Text: text})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

// readFrom is what source's file holds past its offset, up to maxRead, and whether that reached
// the end the file had when it was opened.
func (r *LogReader) readFrom(path, source string) ([]byte, bool, error) {
	f, err := os.Open(path) //nolint:gosec // a log of the run dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() < r.offsets[source] { // truncated or recreated: start over
		r.offsets[source], r.partial[source] = 0, nil
	}
	if _, err := f.Seek(r.offsets[source], io.SeekStart); err != nil {
		return nil, false, err
	}
	var src io.Reader = f
	if r.maxRead > 0 {
		src = io.LimitReader(f, r.maxRead)
	}
	data, err := io.ReadAll(src)
	r.offsets[source] += int64(len(data))
	return data, r.offsets[source] >= info.Size(), err
}

// SkipBacklog moves the reader past what exceeds maxBytes in each file, so its next Read stays
// bounded, and returns how many lines it skipped; the line it lands in is read from there on.
func (r *LogReader) SkipBacklog(maxBytes int64) (int, error) {
	paths, err := filepath.Glob(filepath.Join(r.dir, "*.log"))
	if err != nil {
		return 0, err
	}
	skipped := 0
	for _, path := range paths {
		source := strings.TrimSuffix(filepath.Base(path), ".log")
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		from := r.offsets[source]
		if info.Size() < from { // truncated: read starts over
			from = 0
		}
		to := info.Size() - maxBytes
		if to <= from {
			continue
		}
		n, err := countLines(path, from, to)
		if err != nil {
			return 0, err
		}
		skipped += n
		r.offsets[source], r.partial[source] = to, nil
	}
	return skipped, nil
}

// countLines counts the line breaks between the offsets from and to of path.
func countLines(path string, from, to int64) (int, error) {
	f, err := os.Open(path) //nolint:gosec // a log of the run dir
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	n, buf := 0, make([]byte, 32<<10)
	sr := io.NewSectionReader(f, from, to-from)
	for {
		k, err := sr.Read(buf)
		n += bytes.Count(buf[:k], []byte{'\n'})
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// CapPartial keeps at most maxBytes of each unterminated line, as a bounded view keeps of a line.
func (r *LogReader) CapPartial(maxBytes int) {
	for source, b := range r.partial {
		if len(b) > maxBytes {
			r.partial[source] = bytes.Clone(b[:maxBytes])
		}
	}
}
