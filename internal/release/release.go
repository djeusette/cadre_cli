// Package release finds cadre's latest release on GitHub and reads its files.
package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitHubURL is GitHub's REST API, which lists the releases of djeusette/cadre_cli.
const GitHubURL = "https://api.github.com"

// latestPath is the newest release that is neither a draft nor a prerelease.
const latestPath = "/repos/djeusette/cadre_cli/releases/latest"

const (
	maxAnswerBytes  = 1 << 20
	maxArchiveBytes = 64 << 20
	maxBinaryBytes  = 128 << 20
	defaultTimeout  = 5 * time.Minute
)

var (
	// ErrNoChecksum means the checksums file does not list the archive.
	ErrNoChecksum = errors.New("no checksum for the archive")
	// ErrChecksumMismatch means the archive's SHA-256 is not the one the checksums file lists.
	ErrChecksumMismatch = errors.New("the archive does not match its checksum")
	// ErrNoBinary means the archive holds no cadre binary.
	ErrNoBinary = errors.New("the archive has no cadre binary")
)

// StatusError is an HTTP status from GitHub other than 200.
type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("GitHub answered with an unexpected status %d", e.Status)
}

// RateLimitedError is GitHub refusing a request because this address used up its rate limit.
// RetryAfter is how long until the limit resets, or zero when GitHub did not say.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "GitHub's rate limit is used up" }

// Release is a published release: its version, without the tag's leading "v", and the download URL
// of each of its files by name.
type Release struct {
	Version string
	Assets  map[string]string
}

// Client reads cadre's releases from the GitHub API at BaseURL. It sends no credentials.
type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client
}

// AssetName is the release archive for the platform goos/goarch, named as .goreleaser.yaml names it,
// which is also `cadre_$(uname -s)_$(uname -m).tar.gz`.
func AssetName(goos, goarch string) string {
	arch := goarch
	switch {
	case goarch == "amd64":
		arch = "x86_64"
	case goos == "linux" && goarch == "arm64":
		arch = "aarch64"
	}
	return "cadre_" + strings.ToUpper(goos[:1]) + goos[1:] + "_" + arch + ".tar.gz"
}

// ChecksumsName is the release file, GoReleaser's default, that lists every archive's SHA-256.
func ChecksumsName(version string) string {
	return "cadre_" + version + "_checksums.txt"
}

// Latest returns the newest release.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	body, err := c.get(ctx, strings.TrimRight(c.BaseURL, "/")+latestPath, "application/vnd.github+json", maxAnswerBytes)
	if err != nil {
		return Release{}, err
	}
	var answer struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return Release{}, fmt.Errorf("unexpected answer from GitHub: %w", err)
	}
	rel := Release{Version: strings.TrimPrefix(answer.TagName, "v"), Assets: make(map[string]string, len(answer.Assets))}
	for _, a := range answer.Assets {
		rel.Assets[a.Name] = a.URL
	}
	return rel, nil
}

// Download returns the release file at url.
func (c *Client) Download(ctx context.Context, url string) ([]byte, error) {
	return c.get(ctx, url, "application/octet-stream", maxArchiveBytes)
}

// Verify checks that archive, the release file name, has the SHA-256 that checksums lists for it.
func Verify(archive []byte, name string, checksums []byte) error {
	sum := sha256.Sum256(archive)
	for line := range strings.Lines(string(checksums)) {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		if !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
			return ErrChecksumMismatch
		}
		return nil
	}
	return ErrNoChecksum
}

// Binary returns the cadre binary inside a .tar.gz release archive.
func Binary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	files := tar.NewReader(gz)
	for {
		hdr, err := files.Next()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoBinary
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != "cadre" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > maxBinaryBytes {
			return nil, fmt.Errorf("the cadre binary is larger than %d bytes", maxBinaryBytes)
		}
		return io.ReadAll(files)
	}
}

// get sends one GET and returns the body, refusing one larger than limit.
func (c *Client) get(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := statusError(resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(body)) > limit {
		err = fmt.Errorf("the answer is larger than %d bytes", limit)
	}
	return body, err
}

// statusError is nil for a 200, a *RateLimitedError when GitHub's rate limit is used up,
// and a *StatusError otherwise.
func statusError(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0":
		return &RateLimitedError{RetryAfter: untilReset(resp.Header.Get("X-RateLimit-Reset"))}
	default:
		return &StatusError{Status: resp.StatusCode}
	}
}

// untilReset reads X-RateLimit-Reset, a Unix time, as the minutes left until then. Any other form is zero.
func untilReset(header string) time.Duration {
	reset, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64)
	if err != nil {
		return 0
	}
	return max(time.Until(time.Unix(reset, 0)).Round(time.Minute), 0)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}
