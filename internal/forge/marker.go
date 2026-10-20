package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Marker tags every comment delamain posts so it can skip its own comments and never post twice.
type Marker struct {
	RunID int64
	Kind  string // review | reply | summary
	Key   string // stable hash identifying what the comment is about
}

// String is a hidden HTML comment, invisible in the rendered body.
func (m Marker) String() string {
	return fmt.Sprintf("<!-- delamain:run=%d kind=%s key=%s -->", m.RunID, m.Kind, m.Key)
}

var markerPattern = regexp.MustCompile(`<!-- delamain:run=(\d+) kind=(\w+) key=([0-9a-f]+) -->`)

// ParseMarker finds the marker anywhere in body; a malformed one is no marker.
func ParseMarker(body string) (Marker, bool) {
	m := markerPattern.FindStringSubmatch(body)
	if m == nil {
		return Marker{}, false
	}
	run, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return Marker{}, false
	}
	return Marker{RunID: run, Kind: m[2], Key: m[3]}, true
}

// Key is the first 12 hex characters of the sha256 of parts joined by "\x00".
func Key(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}
