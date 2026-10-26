package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"

	"github.com/djeusette/cadre_cli/internal/release"
)

// installCommand installs the latest release in ~/.local/bin, for when upgrade can't.
const installCommand = `gh release download -R djeusette/cadre_cli --pattern "cadre_$(uname -s)_$(uname -m).tar.gz" -O - | tar -xz -C ~/.local/bin cadre`

// releaseVersion is MAJOR.MINOR.PATCH, the version GoReleaser stamps from a v* tag.
// Anything else, such as "dev" or a snapshot, is a development build.
var releaseVersion = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// upgraded is upgrade's --json answer. Path is the binary it replaced, or would have.
type upgraded struct {
	Version  string `json:"version"`
	Latest   string `json:"latest"`
	Upgraded bool   `json:"upgraded"`
	Path     string `json:"path"`
}

// runUpgrade replaces the running binary with the latest release once its checksum matches.
// It talks to GitHub only, never to a Cadre server, so it needs no sign-in.
func runUpgrade(env Env, asJSON bool, args []string) int {
	if len(args) != 0 {
		return usageError(env)
	}
	current, ok := parseVersion(env.Version)
	if !ok {
		fmt.Fprintf(env.Stderr, "This cadre is a development build (%s), so it can't tell which release it is. Install a release with:\n%s\n", env.Version, installCommand)
		return 1
	}

	ctx := context.Background()
	gh := &release.Client{BaseURL: env.GitHubURL, UserAgent: "cadre-cli/" + env.Version, HTTP: env.HTTP}
	latest, err := gh.Latest(ctx)
	if err != nil {
		return failGitHub(env, err)
	}
	target, ok := parseVersion(latest.Version)
	if !ok {
		fmt.Fprintf(env.Stderr, "The latest release has a version cadre can't read: %q.\n", latest.Version)
		return 1
	}
	path, err := executable(env)
	if err != nil {
		return fail(env, fmt.Errorf("cannot find the running cadre: %w", err))
	}

	result := upgraded{Version: env.Version, Latest: latest.Version, Path: path}
	if slices.Compare(target, current) > 0 {
		binary, ok := fetchBinary(ctx, env, gh, latest)
		if !ok {
			return 1
		}
		if err := replace(path, binary); err != nil {
			return failReplace(env, path, err)
		}
		result.Upgraded = true
	}
	return printUpgrade(env, asJSON, result)
}

// parseVersion splits a release version into its numbers, so 0.10.0 sorts after 0.9.0.
func parseVersion(v string) ([]int, bool) {
	m := releaseVersion.FindStringSubmatch(v)
	if m == nil {
		return nil, false
	}
	numbers := make([]int, 3)
	for i, s := range m[1:] {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, false
		}
		numbers[i] = n
	}
	return numbers, true
}

// executable is the file the running cadre was started from, symlinks resolved,
// so upgrade replaces the binary and not a link to it.
func executable(env Env) (string, error) {
	path, err := env.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// fetchBinary downloads this platform's archive and the checksums file, checks one against the other
// and unpacks the binary. It says why on stderr when it can't.
func fetchBinary(ctx context.Context, env Env, gh *release.Client, latest release.Release) ([]byte, bool) {
	name := release.AssetName(runtime.GOOS, runtime.GOARCH)
	sumsName := release.ChecksumsName(latest.Version)
	noChecksum := func() ([]byte, bool) {
		fmt.Fprintf(env.Stderr, "The latest release, %s, has no checksum for %s, so cadre was not upgraded.\n", latest.Version, name)
		return nil, false
	}

	archiveURL, ok := latest.Assets[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "The latest release, %s, has no %s for %s/%s.\n", latest.Version, name, runtime.GOOS, runtime.GOARCH)
		return nil, false
	}
	sumsURL, ok := latest.Assets[sumsName]
	if !ok {
		return noChecksum()
	}
	archive, err := gh.Download(ctx, archiveURL)
	if err != nil {
		fmt.Fprintf(env.Stderr, "Could not download %s: %v\n", name, err)
		return nil, false
	}
	sums, err := gh.Download(ctx, sumsURL)
	if err != nil {
		fmt.Fprintf(env.Stderr, "Could not download %s: %v\n", sumsName, err)
		return nil, false
	}

	switch err := release.Verify(archive, name, sums); {
	case errors.Is(err, release.ErrNoChecksum):
		return noChecksum()
	case err != nil:
		fmt.Fprintf(env.Stderr, "The download of %s does not match its checksum, so cadre was not upgraded. Try again.\n", name)
		return nil, false
	}
	binary, err := release.Binary(archive)
	if err != nil {
		fmt.Fprintf(env.Stderr, "Could not unpack %s: %v\n", name, err)
		return nil, false
	}
	return binary, true
}

// replace writes binary to a temp file next to path, then renames it over path: the old binary stays
// whole until the new one is complete, and a cadre that is running keeps the file it opened.
func replace(path string, binary []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cadre-upgrade-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // already gone once the rename succeeds

	if err := writeExecutable(tmp, binary); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func writeExecutable(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return errors.Join(err, f.Chmod(0o755), f.Sync(), f.Close())
}

func failReplace(env Env, path string, err error) int {
	if errors.Is(err, fs.ErrPermission) {
		fmt.Fprintf(env.Stderr, "Cannot replace %s: permission denied. Install cadre in a folder you can write, such as ~/.local/bin:\n%s\n", path, installCommand)
		return 1
	}
	fmt.Fprintf(env.Stderr, "Cannot replace %s: %v\n", path, err)
	return 1
}

func failGitHub(env Env, err error) int {
	var limited *release.RateLimitedError
	if errors.As(err, &limited) {
		fmt.Fprintf(env.Stderr, "GitHub's limit on requests from this address is used up. %s\n", tryAgain(limited.RetryAfter))
		return 1
	}
	fmt.Fprintf(env.Stderr, "Could not find the latest cadre on GitHub: %v\n", err)
	return 1
}

func printUpgrade(env Env, asJSON bool, result upgraded) int {
	switch {
	case asJSON:
		data, err := json.Marshal(result)
		if err != nil {
			return fail(env, err)
		}
		fmt.Fprintln(env.Stdout, string(data))
	case result.Upgraded:
		fmt.Fprintf(env.Stdout, "Upgraded cadre from %s to %s.\n", result.Version, result.Latest)
	default:
		fmt.Fprintf(env.Stdout, "cadre %s is already the latest version.\n", result.Version)
	}
	return 0
}
