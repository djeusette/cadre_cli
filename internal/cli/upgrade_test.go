package cli_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djeusette/cadre_cli/internal/cli"
	"github.com/djeusette/cadre_cli/internal/release"
)

const (
	latestPath = "/repos/djeusette/cadre_cli/releases/latest"
	installCmd = `gh release download -R djeusette/cadre_cli --pattern "cadre_$(uname -s)_$(uname -m).tar.gz" -O - | tar -xz -C ~/.local/bin cadre`
	oldBinary  = "old cadre"
	newBinary  = "new cadre"
)

// fakeRelease is what the fake GitHub serves: a tag and its files by name.
type fakeRelease struct {
	tag    string
	assets map[string][]byte
}

// hostAsset is the archive name for the platform the tests run on.
func hostAsset() string { return release.AssetName(runtime.GOOS, runtime.GOARCH) }

// otherAsset is an archive name for another platform, so picking the wrong file shows.
func otherAsset() string {
	if name := release.AssetName("linux", "arm64"); name != hostAsset() {
		return name
	}
	return release.AssetName("darwin", "arm64")
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func checksumLine(name string, data []byte) string {
	return fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
}

// newRelease is a release as GoReleaser publishes it: an archive per platform and the checksums file.
func newRelease(t *testing.T, tag string) fakeRelease {
	t.Helper()
	archive := tarGz(t, map[string]string{"README.md": "# cadre\n", "cadre": newBinary})
	other := tarGz(t, map[string]string{"README.md": "# cadre\n", "cadre": "cadre for another platform"})
	version := strings.TrimPrefix(tag, "v")
	return fakeRelease{tag: tag, assets: map[string][]byte{
		hostAsset():                    archive,
		otherAsset():                   other,
		release.ChecksumsName(version): []byte(checksumLine(otherAsset(), other) + checksumLine(hostAsset(), archive)),
	}}
}

// fakeGitHub serves rel at GitHub's latest-release path and its files under /download/,
// and records the path of every request.
type fakeGitHub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newFakeGitHub(t *testing.T, rel fakeRelease) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{}
	gh.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.mu.Lock()
		gh.paths = append(gh.paths, r.URL.Path)
		gh.mu.Unlock()
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "cadre-cli/") {
			t.Errorf("User-Agent = %q", ua)
		}
		switch {
		case r.URL.Path == latestPath:
			assets := []map[string]string{}
			for name := range rel.assets {
				assets = append(assets, map[string]string{"name": name, "browser_download_url": gh.srv.URL + "/download/" + name})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": rel.tag, "assets": assets})
		case strings.HasPrefix(r.URL.Path, "/download/"):
			data, ok := rel.assets[strings.TrimPrefix(r.URL.Path, "/download/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.srv.Close)
	return gh
}

func (gh *fakeGitHub) requests() []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return append([]string(nil), gh.paths...)
}

type upgradeHarness struct {
	env  cli.Env
	out  *strings.Builder
	errb *strings.Builder
	dir  string
	exe  string
}

// newUpgradeHarness installs an old cadre in a temp folder and points the CLI at srv as GitHub.
// Getenv is left nil: upgrade never reads the config.
func newUpgradeHarness(t *testing.T, srv *httptest.Server, version string) *upgradeHarness {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "cadre")
	if err := os.WriteFile(exe, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &upgradeHarness{out: &strings.Builder{}, errb: &strings.Builder{}, dir: dir, exe: exe}
	h.env = cli.Env{
		Stdout:     h.out,
		Stderr:     h.errb,
		HTTP:       srv.Client(),
		GitHubURL:  srv.URL,
		Executable: func() (string, error) { return h.exe, nil },
		Version:    version,
	}
	return h
}

func (h *upgradeHarness) binary(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// assertOnlyCadre checks that the folder holds the binary and nothing else, such as a leftover temp file.
func assertOnlyCadre(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cadre" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("folder holds %v, want only cadre", names)
	}
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestUpgradeReplacesTheBinaryWithTheLatestRelease(t *testing.T) {
	gh := newFakeGitHub(t, newRelease(t, "v0.10.0"))
	h := newUpgradeHarness(t, gh.srv, "0.4.0")

	if code := cli.Run([]string{"upgrade"}, h.env); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if got := h.out.String(); got != "Upgraded cadre from 0.4.0 to 0.10.0.\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := h.binary(t); got != newBinary {
		t.Errorf("binary = %q, want %q", got, newBinary)
	}
	info, err := os.Stat(h.exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	assertOnlyCadre(t, h.dir)
}

func TestUpgradePrintsJSON(t *testing.T) {
	gh := newFakeGitHub(t, newRelease(t, "v0.5.0"))
	h := newUpgradeHarness(t, gh.srv, "0.4.0")

	if code := cli.Run([]string{"upgrade", "--json"}, h.env); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(h.out.String()), &got); err != nil {
		t.Fatalf("stdout %q: %v", h.out, err)
	}
	want := map[string]any{"version": "0.4.0", "latest": "0.5.0", "upgraded": true, "path": resolved(t, h.exe)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("json = %v, want %v", got, want)
	}
}

func TestUpgradeLeavesTheLatestVersionAlone(t *testing.T) {
	for _, version := range []string{"0.5.0", "0.6.0"} {
		t.Run(version, func(t *testing.T) {
			gh := newFakeGitHub(t, newRelease(t, "v0.5.0"))
			h := newUpgradeHarness(t, gh.srv, version)

			if code := cli.Run([]string{"upgrade"}, h.env); code != 0 {
				t.Fatalf("code = %d, stderr %q", code, h.errb)
			}
			if got, want := h.out.String(), "cadre "+version+" is already the latest version.\n"; got != want {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			if got := h.binary(t); got != oldBinary {
				t.Errorf("binary = %q, want it unchanged", got)
			}
			if got := gh.requests(); len(got) != 1 || got[0] != latestPath {
				t.Errorf("requests = %v, want only the latest release", got)
			}
		})
	}
}

func TestUpgradePrintsJSONWhenAlreadyLatest(t *testing.T) {
	gh := newFakeGitHub(t, newRelease(t, "v0.5.0"))
	h := newUpgradeHarness(t, gh.srv, "0.5.0")

	if code := cli.Run([]string{"upgrade", "--json"}, h.env); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(h.out.String()), &got); err != nil {
		t.Fatalf("stdout %q: %v", h.out, err)
	}
	want := map[string]any{"version": "0.5.0", "latest": "0.5.0", "upgraded": false, "path": resolved(t, h.exe)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("json = %v, want %v", got, want)
	}
}

func TestUpgradeRefusesADevelopmentBuild(t *testing.T) {
	for _, version := range []string{"dev", "0.5.0-SNAPSHOT-abc1234"} {
		t.Run(version, func(t *testing.T) {
			gh := newFakeGitHub(t, newRelease(t, "v0.5.0"))
			h := newUpgradeHarness(t, gh.srv, version)

			if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
				t.Fatalf("code = %d, want 1", code)
			}
			want := "This cadre is a development build (" + version + "), so it can't tell which release it is. Install a release with:\n" + installCmd + "\n"
			if got := h.errb.String(); got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
			if got := gh.requests(); len(got) != 0 {
				t.Errorf("requests = %v, want none", got)
			}
			if got := h.binary(t); got != oldBinary {
				t.Errorf("binary = %q, want it unchanged", got)
			}
		})
	}
}

func TestUpgradeRefusesADownloadThatDoesNotMatchItsChecksum(t *testing.T) {
	rel := newRelease(t, "v0.5.0")
	rel.assets[release.ChecksumsName("0.5.0")] = []byte(checksumLine(hostAsset(), []byte("something else")))
	gh := newFakeGitHub(t, rel)
	h := newUpgradeHarness(t, gh.srv, "0.4.0")

	if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	want := "The download of " + hostAsset() + " does not match its checksum, so cadre was not upgraded. Try again.\n"
	if got := h.errb.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := h.binary(t); got != oldBinary {
		t.Errorf("binary = %q, want it unchanged", got)
	}
	assertOnlyCadre(t, h.dir)
}

func TestUpgradeRefusesAReleaseWithoutChecksums(t *testing.T) {
	rel := newRelease(t, "v0.5.0")
	delete(rel.assets, release.ChecksumsName("0.5.0"))
	gh := newFakeGitHub(t, rel)
	h := newUpgradeHarness(t, gh.srv, "0.4.0")

	if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	want := "The latest release, 0.5.0, has no checksum for " + hostAsset() + ", so cadre was not upgraded.\n"
	if got := h.errb.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := h.binary(t); got != oldBinary {
		t.Errorf("binary = %q, want it unchanged", got)
	}
}

func TestUpgradeRefusesAReleaseWithoutThisPlatform(t *testing.T) {
	rel := newRelease(t, "v0.5.0")
	delete(rel.assets, hostAsset())
	gh := newFakeGitHub(t, rel)
	h := newUpgradeHarness(t, gh.srv, "0.4.0")

	if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	want := "The latest release, 0.5.0, has no " + hostAsset() + " for " + runtime.GOOS + "/" + runtime.GOARCH + ".\n"
	if got := h.errb.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestUpgradeRefusesAnArchiveWithoutTheBinary(t *testing.T) {
	rel := newRelease(t, "v0.5.0")
	archive := tarGz(t, map[string]string{"README.md": "# cadre\n"})
	rel.assets[hostAsset()] = archive
	rel.assets[release.ChecksumsName("0.5.0")] = []byte(checksumLine(hostAsset(), archive))
	gh := newFakeGitHub(t, rel)
	h := newUpgradeHarness(t, gh.srv, "0.4.0")

	if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	want := "Could not unpack " + hostAsset() + ": the archive has no cadre binary\n"
	if got := h.errb.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := h.binary(t); got != oldBinary {
		t.Errorf("binary = %q, want it unchanged", got)
	}
}

func TestUpgradeReplacesTheFileASymlinkPointsTo(t *testing.T) {
	gh := newFakeGitHub(t, newRelease(t, "v0.5.0"))
	h := newUpgradeHarness(t, gh.srv, "0.4.0")
	real := h.exe
	h.exe = filepath.Join(t.TempDir(), "cadre")
	if err := os.Symlink(real, h.exe); err != nil {
		t.Fatal(err)
	}

	if code := cli.Run([]string{"upgrade"}, h.env); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if info, err := os.Lstat(h.exe); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v, %v", info, err)
	}
	if got := h.binary(t); got != newBinary {
		t.Errorf("binary = %q, want %q", got, newBinary)
	}
	assertOnlyCadre(t, h.dir)
}

func TestUpgradeExplainsAFolderItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only folder")
	}
	gh := newFakeGitHub(t, newRelease(t, "v0.5.0"))
	h := newUpgradeHarness(t, gh.srv, "0.4.0")
	if err := os.Chmod(h.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.dir, 0o755) })

	if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	want := "Cannot replace " + resolved(t, h.exe) + ": permission denied. Install cadre in a folder you can write, such as ~/.local/bin:\n" + installCmd + "\n"
	if got := h.errb.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := h.binary(t); got != oldBinary {
		t.Errorf("binary = %q, want it unchanged", got)
	}
}

func TestUpgradeSaysWhenGitHubsRateLimitIsUsedUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10))
		writeJSON(w, http.StatusForbidden, `{"message":"API rate limit exceeded"}`)
	}))
	t.Cleanup(srv.Close)
	h := newUpgradeHarness(t, srv, "0.4.0")

	if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	want := "GitHub's limit on requests from this address is used up. Try again in 10m0s.\n"
	if got := h.errb.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestUpgradeReportsAnUnexpectedAnswerFromGitHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{}`)
	}))
	t.Cleanup(srv.Close)
	h := newUpgradeHarness(t, srv, "0.4.0")

	if code := cli.Run([]string{"upgrade"}, h.env); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	want := "Could not find the latest cadre on GitHub: GitHub answered with an unexpected status 500\n"
	if got := h.errb.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestUpgradeTakesNoArguments(t *testing.T) {
	gh := newFakeGitHub(t, newRelease(t, "v0.5.0"))
	h := newUpgradeHarness(t, gh.srv, "0.4.0")

	if code := cli.Run([]string{"upgrade", "now"}, h.env); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(h.errb.String(), "upgrade") {
		t.Errorf("stderr = %q, want the usage naming upgrade", h.errb)
	}
	if got := gh.requests(); len(got) != 0 {
		t.Errorf("requests = %v, want none", got)
	}
}
