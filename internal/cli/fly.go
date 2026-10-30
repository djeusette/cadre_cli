package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/djeusette/cadre_cli/internal/api"
)

const msgNoToken = "No token given. Pipe it in, or type it when asked."

// runFly dispatches `fly org add <org>` and `fly org remove <org>`; any other form is a usage error.
func runFly(env Env, asJSON bool, args []string) int {
	if len(args) < 2 || args[0] != "org" {
		return usageError(env)
	}
	rest := args[2:]
	switch args[1] {
	case "add":
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			return addFlyOrg(ctx, s, a[0])
		})
	case "remove":
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			return removeFlyOrg(ctx, s, a[0])
		})
	default:
		return usageError(env)
	}
}

func addFlyOrg(ctx context.Context, s *session, org string) int {
	if !s.requireKey() {
		return 1
	}

	token, err := s.readToken(org)
	if err != nil {
		fmt.Fprintf(s.env.Stderr, "Could not read the token: %v\n", err)
		return 1
	}
	if token == "" {
		fmt.Fprintln(s.env.Stderr, msgNoToken)
		return 1
	}

	added, err := s.client.AddAccount(ctx, "fly", org, token)
	if err != nil {
		return s.flyFailure(err, org)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(added.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Connected the Fly org %s: its token reads %s.\n", org, apps(added.Apps))
	return 0
}

func removeFlyOrg(ctx context.Context, s *session, org string) int {
	if !s.requireKey() {
		return 1
	}

	removed, err := s.client.RemoveAccount(ctx, "fly", org)
	if err != nil {
		return s.flyFailure(err, org)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(removed.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Removed the Fly org %s.\n", org)
	return 0
}

func apps(n int) string {
	if n == 1 {
		return "1 app"
	}
	return fmt.Sprintf("%d apps", n)
}

// flyFailure prints a failed fly org call to stderr and returns exit code 1. The token is never part of it.
func (s *session) flyFailure(err error, org string) int {
	var inUse *api.AccountInUseError
	switch {
	case errors.Is(err, api.ErrTokenRefused):
		fmt.Fprintf(s.env.Stderr, "Fly refused this token for %s.\n", org)
	case errors.Is(err, api.ErrOrgNotFound):
		fmt.Fprintf(s.env.Stderr, "Fly has no org %s that this token can read.\n", org)
	case errors.Is(err, api.ErrAccountNotFound):
		fmt.Fprintf(s.env.Stderr, "No Fly org %s on %s.\n", org, s.baseURL)
	case errors.Is(err, api.ErrPlatformNotFound):
		fmt.Fprintf(s.env.Stderr, "Fly isn't a platform on %s.\n", s.baseURL)
	case errors.Is(err, api.ErrPlatformUnreachable):
		fmt.Fprintln(s.env.Stderr, "Cadre couldn't reach Fly. Try again in a minute.")
	case errors.As(err, &inUse):
		fmt.Fprintf(s.env.Stderr, "%s still reads %s. Unset or move those targets first.\n", org, strings.Join(inUse.Repos, ", "))
	default:
		return s.failAPI(err)
	}
	return 1
}

// readToken is the token for org, trimmed. When stdin is piped it is read whole. On a terminal the person is
// asked on stderr and the answer is read with echo off.
func (s *session) readToken(org string) (string, error) {
	if !s.env.stdinIsTerminal() {
		raw, err := io.ReadAll(s.env.stdin())
		return strings.TrimSpace(string(raw)), err
	}

	fmt.Fprintf(s.env.Stderr, "Token for %s: ", org)
	read := s.env.ReadSecret
	if read == nil {
		read = readSecretFromTTY(s.env.Stderr)
	}
	token, err := read()
	return strings.TrimSpace(token), err
}

func (e Env) stdin() io.Reader {
	if e.Stdin == nil {
		return os.Stdin
	}
	return e.Stdin
}

func (e Env) stdinIsTerminal() bool {
	if e.Terminal != nil {
		return e.Terminal()
	}
	if e.Stdin != nil {
		return false
	}
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// readSecretFromTTY reads one line from the terminal with echo off, restoring echo on every path, and ends the
// prompt's line on out.
func readSecretFromTTY(out io.Writer) func() (string, error) {
	return func() (string, error) {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return "", err
		}
		defer tty.Close()

		if err := stty(tty, "-echo"); err != nil {
			return "", err
		}
		defer func() {
			_ = stty(tty, "echo")
			fmt.Fprintln(out)
		}()

		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && line != "") {
			return "", err
		}
		return line, nil
	}
}

func stty(tty *os.File, mode string) error {
	cmd := exec.Command("stty", mode)
	cmd.Stdin = tty
	return cmd.Run()
}
