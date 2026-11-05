// Package git reads the checkout cadre deploy runs in.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrNotACheckout means the working directory is outside a checkout, or the checkout has no origin.
var ErrNotACheckout = errors.New("not a git checkout")

// ErrMarkedFiles means a tracked file is marked skip-worktree or assume-unchanged. Its text is printed as it is.
var ErrMarkedFiles = errors.New("Files marked skip-worktree or assume-unchanged can hide changes. Clear the marks, listed by git ls-files -v.")

// Checkout is a checkout's top level.
type Checkout struct{ Dir string }

// Open finds the checkout that holds the working directory.
func Open(ctx context.Context) (Checkout, error) {
	dir, err := run(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return Checkout{}, notACheckout(err)
	}
	return Checkout{Dir: dir}, nil
}

// Repository is origin's "owner/name".
func (c Checkout) Repository(ctx context.Context) (string, error) {
	remote, err := run(ctx, c.Dir, "remote", "get-url", "origin")
	if err != nil {
		return "", notACheckout(err)
	}
	repository, ok := ParseRepository(remote)
	if !ok {
		return "", ErrNotACheckout
	}
	return repository, nil
}

// Branch is the current branch, or "" on a detached HEAD.
func (c Checkout) Branch(ctx context.Context) (string, error) {
	return run(ctx, c.Dir, "branch", "--show-current")
}

// Clean reports whether nothing is staged, modified or untracked, and git status can see every tracked file. Ignored
// files don't count. When git fails it is never clean. A tree that only has marked files is ErrMarkedFiles, not plain
// false, as committing or stashing can't help with those.
func (c Checkout) Clean(ctx context.Context) (bool, error) {
	out, err := run(ctx, c.Dir, "status", "--porcelain", "--untracked-files=normal")
	if err != nil || out != "" {
		return false, err
	}
	return c.statusSeesEveryFile(ctx)
}

// statusSeesEveryFile is ErrMarkedFiles when a tracked file is marked skip-worktree or assume-unchanged, which hide its
// changes from git status. ls-files -v tags every other tracked file H.
func (c Checkout) statusSeesEveryFile(ctx context.Context) (bool, error) {
	out, err := run(ctx, c.Dir, "ls-files", "-v")
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(out) {
		if !strings.HasPrefix(line, "H") {
			return false, ErrMarkedFiles
		}
	}
	return true, nil
}

// Tracked reports whether file, relative to the top level, is exactly one tracked file, so a directory is false.
func (c Checkout) Tracked(ctx context.Context, file string) (bool, error) {
	out, err := run(ctx, c.Dir, "ls-files", "--full-name", "--", ":(literal)"+file)
	if err != nil {
		return false, err
	}
	return out == file, nil
}

// Fetch updates origin/main and nothing else. Naming the ref makes a main deleted on origin an error instead of a stale
// origin/main, without pruning the person's other refs. Its error holds git's message.
func (c Checkout) Fetch(ctx context.Context) error {
	_, err := run(ctx, c.Dir, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
	return err
}

// Head is the full commit HEAD points at.
func (c Checkout) Head(ctx context.Context) (string, error) {
	return run(ctx, c.Dir, "rev-parse", "HEAD")
}

// OriginMain is the full commit origin/main was at the last fetch.
func (c Checkout) OriginMain(ctx context.Context) (string, error) {
	return run(ctx, c.Dir, "rev-parse", "--verify", "refs/remotes/origin/main")
}

// ParseRepository takes a remote's last two path segments, without .git. A host is never one of them.
func ParseRepository(remote string) (string, bool) {
	remote = strings.TrimRight(strings.TrimSpace(remote), "/")
	remote = strings.TrimSuffix(remote, ".git")
	parts := strings.FieldsFunc(remotePath(remote), func(r rune) bool { return r == '/' })
	if len(parts) < 2 {
		return "", false
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if strings.ContainsRune(owner, '@') {
		return "", false
	}
	return owner + "/" + name, true
}

// remotePath is what follows the host of a URL or an scp form. As in git, a colon before any slash makes the scp form,
// and anything else is a local path, which has no host.
func remotePath(remote string) string {
	if _, url, ok := strings.Cut(remote, "://"); ok {
		_, path, _ := strings.Cut(url, "/")
		return path
	}
	if host, path, ok := strings.Cut(remote, ":"); ok && !strings.Contains(host, "/") {
		return path
	}
	return remote
}

// notACheckout maps git's own refusals to ErrNotACheckout, and leaves other failures, such as a missing git, as they are.
func notACheckout(err error) error {
	var exit *exitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%w: %s", ErrNotACheckout, exit.message)
	}
	return err
}

// exitError is git exiting non-zero, with its trimmed stderr.
type exitError struct{ message string }

func (e *exitError) Error() string { return e.message }

// run runs git in dir ("" is the working directory) and returns its trimmed stdout, or an error holding its trimmed stderr.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return "", &exitError{message: message}
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
