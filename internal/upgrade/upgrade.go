// Package upgrade replaces the installed delamain binary with the latest GitHub release.
package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultAPI is the GitHub API root of the repository that publishes the releases.
const DefaultAPI = "https://api.github.com/repos/paraizofelipe/delamain"

// ChecksumsAsset lists the sha256 of every binary of a release, in sha256sum format.
const ChecksumsAsset = "checksums.txt"

const defaultMaxSize = 200 << 20

// ErrNoRelease means the repository has not published any release yet.
var ErrNoRelease = errors.New("nenhuma release publicada")

// Release is a published version and its downloadable assets (name -> URL).
type Release struct {
	Tag    string
	Assets map[string]string
}

type Upgrader struct {
	API  string
	HTTP *http.Client
	// Token, when set, authenticates API calls (higher rate limit); asset downloads never carry it.
	Token    string
	OS, Arch string
	// MaxSize bounds each download; zero means 200 MiB.
	MaxSize int64
}

// AssetName is the release asset built for this platform, as `make dist` names it.
func (u Upgrader) AssetName() string { return "delamain_" + u.OS + "_" + u.Arch }

// Latest returns the newest published release.
func (u Upgrader) Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(u.API, "/")+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Release{}, ErrNoRelease
	case resp.StatusCode != http.StatusOK:
		return Release{}, fmt.Errorf("última release: HTTP %d", resp.StatusCode)
	}
	var v struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return Release{}, fmt.Errorf("última release: %w", err)
	}
	if v.Tag == "" {
		return Release{}, errors.New("última release sem tag")
	}
	rel := Release{Tag: v.Tag, Assets: map[string]string{}}
	for _, a := range v.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel, nil
}

// Install downloads this platform's binary of rel, checks its sha256 and that it reports rel.Tag,
// then atomically renames it over target. On any failure target is left untouched.
func (u Upgrader) Install(ctx context.Context, rel Release, target string) error {
	name := u.AssetName()
	binURL, ok := rel.Assets[name]
	if !ok {
		return fmt.Errorf("a release %s não tem binário para %s/%s (%s)", rel.Tag, u.OS, u.Arch, name)
	}
	sumsURL, ok := rel.Assets[ChecksumsAsset]
	if !ok {
		return fmt.Errorf("a release %s não tem %s", rel.Tag, ChecksumsAsset)
	}
	bin, err := u.download(ctx, binURL)
	if err != nil {
		return err
	}
	sums, err := u.download(ctx, sumsURL)
	if err != nil {
		return err
	}
	want, err := checksumOf(sums, name)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(bin); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s: sha256 não confere com %s", name, ChecksumsAsset)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".delamain-upgrade-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // an executable binary
		return err
	}
	if err := reportsVersion(ctx, tmp.Name(), rel.Tag); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

func (u Upgrader) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	limit := u.MaxSize
	if limit == 0 {
		limit = defaultMaxSize
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: maior que o limite de %d bytes", url, limit)
	}
	return b, nil
}

// checksumOf finds name's sha256 in a sha256sum listing.
func checksumOf(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == sha256.Size*2 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s não lista %s", ChecksumsAsset, name)
}

// reportsVersion runs `<bin> version`: it catches a binary for the wrong platform or release.
func reportsVersion(ctx context.Context, bin, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output() //nolint:gosec // the verified download
	if err != nil {
		return fmt.Errorf("o binário baixado não executa: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != tag {
		return fmt.Errorf("o binário baixado informa %q, esperado %q", got, tag)
	}
	return nil
}
