package board

import (
	"net/url"
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
