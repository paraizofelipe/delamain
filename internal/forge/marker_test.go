package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestMarkerShouldRoundTripThroughParse(t *testing.T) {
	m := Marker{RunID: 12, Kind: "reply", Key: "ab12cd"}
	if s := m.String(); s != "<!-- delamain:run=12 kind=reply key=ab12cd -->" {
		t.Fatalf("String() = %q", s)
	}
	for _, body := range []string{
		m.String(),
		"Looks good.\n\n" + m.String(),
		m.String() + "\nfirst line\nsecond line",
		"before " + m.String() + " after",
	} {
		got, ok := ParseMarker(body)
		if !ok || got != m {
			t.Errorf("ParseMarker(%q) = %+v %v, want %+v", body, got, ok, m)
		}
	}
}

func TestParseMarkerShouldIgnoreBodiesWithoutMarker(t *testing.T) {
	for _, body := range []string{"", "plain comment", "<!-- a normal html comment -->", "delamain:run=12 kind=reply key=ab12cd"} {
		if m, ok := ParseMarker(body); ok || m != (Marker{}) {
			t.Errorf("ParseMarker(%q) = %+v %v, want no marker", body, m, ok)
		}
	}
}

func TestParseMarkerShouldIgnoreMalformedMarker(t *testing.T) {
	for _, body := range []string{
		"<!-- delamain:run=abc kind=reply key=ab12cd -->",                  // run is not a number
		"<!-- delamain:run=12 kind=reply key=XYZ -->",                      // key is not hex
		"<!-- delamain:run=12 key=ab12cd -->",                              // kind missing
		"<!-- delamain:run=12 kind=reply -->",                              // key missing
		"<!-- delamain:run=12 kind=reply key=ab12cd",                       // not closed
		"<!-- delamain:run=12 kind=re ply key=ab12cd -->",                  // kind with a space
		"<!-- delamain:kind=reply run=12 key=ab12cd -->",                   // fields out of order
		"<!-- delamain:run=99999999999999999999 kind=reply key=ab12cd -->", // run overflows int64
	} {
		if m, ok := ParseMarker(body); ok || m != (Marker{}) {
			t.Errorf("ParseMarker(%q) = %+v %v, want rejected", body, m, ok)
		}
	}
}

func TestKeyShouldBeStableAndDifferForDifferentParts(t *testing.T) {
	sum := sha256.Sum256([]byte("a.go\x0012\x00nil deref"))
	want := hex.EncodeToString(sum[:])[:12]
	if got := Key("a.go", "12", "nil deref"); got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
	if Key("a.go", "12", "nil deref") != Key("a.go", "12", "nil deref") {
		t.Fatal("Key must be stable")
	}
	if Key("ab", "c") == Key("a", "bc") {
		t.Fatal("the separator must keep different parts apart")
	}
	if Key("a.go", "12") == Key("a.go", "13") {
		t.Fatal("different parts must give different keys")
	}
}
