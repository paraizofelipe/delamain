package forge

import (
	"strings"
	"testing"
)

func TestDetectShouldRecognizeForgeFromRemoteURL(t *testing.T) {
	cases := []struct {
		url, kind string
		want      Repo
	}{
		{"https://github.com/acme/api.git", "auto", Repo{GitHub, "github.com", "acme/api"}},
		{"git@github.com:acme/api.git", "auto", Repo{GitHub, "github.com", "acme/api"}},
		{"ssh://git@gitlab.com:2222/grp/sub/api.git", "auto", Repo{GitLab, "gitlab.com", "grp/sub/api"}},
		{"git@gitlab.luizalabs.com:team/api.git", "auto", Repo{GitLab, "gitlab.luizalabs.com", "team/api"}},
		{"https://git.corp.example/team/api", "gitlab", Repo{GitLab, "git.corp.example", "team/api"}},
		{"https://ghe.corp.example/team/api.git", "github", Repo{GitHub, "ghe.corp.example", "team/api"}},
	}
	for _, c := range cases {
		got, err := Detect(c.url, c.kind)
		if err != nil || got != c.want {
			t.Errorf("Detect(%q, %q) = %+v, %v; want %+v", c.url, c.kind, got, err, c.want)
		}
	}
}

func TestDetectShouldNeverKeepCredentialsFromTheURL(t *testing.T) {
	got, err := Detect("https://oauth2:glpat-SECRET@gitlab.com/grp/api.git", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.String(), "SECRET") || strings.Contains(got.String(), "oauth2") {
		t.Fatalf("credentials leaked into %q", got.String())
	}
}

func TestDetectShouldRejectUnknownHostsAndMalformedURLs(t *testing.T) {
	for _, u := range []string{"https://bitbucket.org/acme/api.git", "/srv/git/api.git", "", "https://github.com/", "git@github.com:"} {
		if _, err := Detect(u, "auto"); err == nil {
			t.Errorf("Detect(%q) must fail: unknown forge or no repository path", u)
		}
	}
}

func TestPRNumberShouldParseGitHubAndGitLabURLs(t *testing.T) {
	for url, want := range map[string]int{
		"https://github.com/acme/api/pull/42":                     42,
		"https://gitlab.com/grp/sub/api/-/merge_requests/7":       7,
		"https://git.corp.example/grp/api/-/merge_requests/1300/": 1300,
		"https://github.com/acme/api/issues/42":                   0,
		"https://github.com/acme/api/pull/":                       0,
		"https://github.com/acme/api/pull/abc":                    0,
		"https://github.com/acme/api/pull/0":                      0,
		"":                                                        0,
	} {
		got, ok := PRNumber(url)
		if got != want || ok != (want > 0) {
			t.Errorf("PRNumber(%q) = %d, %v; want %d", url, got, ok, want)
		}
	}
}
