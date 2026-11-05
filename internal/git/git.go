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

// Clean reports whether nothing is staged, modified or untracked. Ignored files don't count.
func (c Checkout) Clean(ctx context.Context) (bool, error) {
	out, err := run(ctx, c.Dir, "status", "--porcelain", "--untracked-files=normal")
	return out == "", err
}

// Fetch updates origin's branches. Its error holds git's message.
func (c Checkout) Fetch(ctx context.Context) error {
	_, err := run(ctx, c.Dir, "fetch", "origin")
	return err
}

// Head is the full commit HEAD points at.
func (c Checkout) Head(ctx context.Context) (string, error) {
	return run(ctx, c.Dir, "rev-parse", "HEAD")
}

// OriginMain is the full commit origin/main was at the last fetch.
func (c Checkout) OriginMain(ctx context.Context) (string, error) {
	return run(ctx, c.Dir, "rev-parse", "refs/remotes/origin/main")
}

// ParseRepository takes a remote URL's last two path segments, without .git.
func ParseRepository(remote string) (string, bool) {
	remote = strings.TrimRight(strings.TrimSpace(remote), "/")
	remote = strings.TrimSuffix(remote, ".git")
	parts := strings.FieldsFunc(remote, func(r rune) bool { return r == '/' || r == ':' })
	if len(parts) < 2 {
		return "", false
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if strings.ContainsRune(owner, '@') {
		return "", false
	}
	return owner + "/" + name, true
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
