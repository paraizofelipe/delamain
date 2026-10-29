package board

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseReportQueryShouldDefaultTo30Days(t *testing.T) {
	q, err := ParseReportQuery(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if q != (ReportQuery{Period: "30d"}) {
		t.Fatalf("query = %+v", q)
	}
}

func TestParseReportQueryShouldAcceptEveryKnownPeriod(t *testing.T) {
	for _, period := range []string{"7d", "30d", "90d", "all"} {
		v := url.Values{"period": {period}, "repo": {"/r/app"}, "kind": {"review"}, "offset": {"20"}}
		q, err := ParseReportQuery(v)
		if err != nil {
			t.Fatalf("%s: %v", period, err)
		}
		if want := (ReportQuery{Period: period, Repo: "/r/app", Kind: "review", Offset: 20}); q != want {
			t.Errorf("%s: query = %+v, want %+v", period, q, want)
		}
	}
}

func TestParseReportQueryShouldRejectAnUnknownPeriod(t *testing.T) {
	for _, period := range []string{"1d", "30D", "<b>x</b>"} {
		_, err := ParseReportQuery(url.Values{"period": {period}})
		if err == nil {
			t.Errorf("%q: no error", period)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "7d, 30d, 90d") || strings.Contains(msg, period) {
			t.Errorf("%q: error %q", period, msg)
		}
	}
}

func TestParseReportQueryShouldRejectANegativeOffset(t *testing.T) {
	_, err := ParseReportQuery(url.Values{"offset": {"-20"}})
	if err == nil || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseReportQueryShouldRejectANonNumericOffset(t *testing.T) {
	for _, offset := range []string{"abc", "1.5", "99999999999999999999"} {
		_, err := ParseReportQuery(url.Values{"offset": {offset}})
		if err == nil {
			t.Errorf("%q: no error", offset)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "offset") || strings.Contains(msg, offset) {
			t.Errorf("%q: error %q", offset, msg)
		}
	}
}
