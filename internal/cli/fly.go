package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/djeusette/cadre_cli/internal/api"
)

const msgNoToken = "No token given. Pipe it in, or type it when asked."

// runFly dispatches `fly org add <org>` and `fly org remove <org>`.
func runFly(env Env, asJSON bool, args []string) int {
	switch {
	case len(args) == 0:
		return showHelp(env, []string{"fly"})
	case args[0] != "org":
		return unknownCommand(env, "fly", args[0])
	case len(args) == 1:
		return showHelp(env, []string{"fly", "org"})
	}
	rest := args[2:]
	switch args[1] {
	case "add":
		return runCommand(env, asJSON, "fly org add", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return addAccount(ctx, s, "fly", "org", a[0])
		})
	case "remove":
		return runCommand(env, asJSON, "fly org remove", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return removeAccount(ctx, s, "fly", "org", a[0])
		})
	default:
		return unknownCommand(env, "fly org", args[1])
	}
}

// addAccount connects the platform's account name with a token read from stdin or the terminal. noun is what the
// platform calls the account: org or team.
func addAccount(ctx context.Context, s *session, platform, noun, name string) int {
	if !s.requireKey() {
		return 1
	}

	token, err := s.readToken(name)
	if err != nil {
		fmt.Fprintf(s.env.Stderr, "Could not read the token: %v\n", err)
		return 1
	}
	if token == "" {
		fmt.Fprintln(s.env.Stderr, msgNoToken)
		return 1
	}

	return sendAccount(ctx, s, platform, noun, name, token, func(added api.Account) string {
		return fmt.Sprintf("Connected the %s %s %s: its token reads %s.", displayName(platform), noun, added.Name, apps(added.Apps))
	})
}

// sendAccount sends the token for the platform's account name and prints connected's line, or the answer as JSON.
func sendAccount(ctx context.Context, s *session, platform, noun, name, token string, connected func(api.Account) string) int {
	added, err := s.client.AddAccount(ctx, platform, name, token)
	if err != nil {
		return s.accountFailure(err, platform, noun, name)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(added.Raw))
		return 0
	}
	fmt.Fprintln(s.env.Stdout, connected(added))
	return 0
}

func removeAccount(ctx context.Context, s *session, platform, noun, name string) int {
	if !s.requireKey() {
		return 1
	}

	removed, err := s.client.RemoveAccount(ctx, platform, name)
	if err != nil {
		return s.accountFailure(err, platform, noun, name)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(removed.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Removed the %s %s %s.\n", displayName(platform), noun, removed.Name)
	return 0
}

func apps(n int) string {
	if n == 1 {
		return "1 app"
	}
	return fmt.Sprintf("%d apps", n)
}

// accountFailure prints a failed account call to stderr and returns exit code 1. The token is never part of it.
func (s *session) accountFailure(err error, platform, noun, name string) int {
	display := displayName(platform)
	var inUse *api.AccountInUseError
	switch {
	case errors.Is(err, api.ErrTokenRefused):
		fmt.Fprint(s.env.Stderr, tokenRefused(platform, name))
	case errors.Is(err, api.ErrOrgNotFound):
		fmt.Fprintf(s.env.Stderr, "%s has no %s %s that this token can read.\n", display, noun, name)
	case errors.Is(err, api.ErrAccountNotFound):
		fmt.Fprintf(s.env.Stderr, "No %s %s %s on %s.\n", display, noun, name, s.baseURL)
	case errors.Is(err, api.ErrPlatformNotFound):
		fmt.Fprintf(s.env.Stderr, "%s isn't a platform on %s.\n", display, s.baseURL)
	case errors.Is(err, api.ErrPlatformUnreachable):
		fmt.Fprintf(s.env.Stderr, "Cadre couldn't reach %s. Try again in a minute.\n", display)
	case errors.As(err, &inUse):
		fmt.Fprintf(s.env.Stderr, "%s still reads %s. %s\n", name, strings.Join(inUse.Repos, ", "), inUseAdvice(platform))
	default:
		return s.failAPI(err)
	}
	return 1
}

// inUseAdvice says what to do about an account that apps still read. A store account is replaced by another one, and
// each app is set again to read it.
func inUseAdvice(platform string) string {
	switch platform {
	case "app_store":
		return "Add the key that replaces it, set each of those apps again with cadre repo set, then remove this one."
	case "google_play":
		return "Add the account that replaces it, set each of those apps again with cadre repo set, then remove this one."
	default:
		return "Unset or move those targets first."
	}
}

// tokenRefused says a platform refused the credentials of an account.
func tokenRefused(platform, name string) string {
	switch platform {
	case "app_store":
		return fmt.Sprintf("Apple refused the key %s. Check its key ID, its issuer ID and its .p8 file.\n", name)
	case "google_play":
		return "Google refused this service account's key.\n"
	}
	return fmt.Sprintf("%s refused this token for %s.\n", displayName(platform), name)
}

// readToken is the token for the named account, trimmed. When stdin is piped it is read whole. On a terminal the
// person is asked on stderr and the answer is read with echo off.
func (s *session) readToken(name string) (string, error) {
	if !s.env.stdinIsTerminal() {
		raw, err := io.ReadAll(s.env.stdin())
		return strings.TrimSpace(string(raw)), err
	}

	fmt.Fprintf(s.env.Stderr, "Token for %s: ", name)
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

// exitProcess ends the process on an interrupt at the token prompt; tests replace it.
var exitProcess = os.Exit

// exitInterrupted is the shell's code for a process ended by Ctrl-C.
const exitInterrupted = 130

// guardInterrupt waits for a signal on sigs. On one it runs restore, ends the prompt's line on out and calls
// exit(130). It returns when done is closed.
func guardInterrupt(sigs <-chan os.Signal, done <-chan struct{}, restore func(), out io.Writer, exit func(int)) {
	select {
	case <-sigs:
		restore()
		fmt.Fprintln(out)
		exit(exitInterrupted)
	case <-done:
	}
}

// readSecretFromTTY reads one line from the terminal with echo off, restoring echo on every path, and ends the
// prompt's line on out. An interrupt or termination while it reads restores echo too, and exits with 130.
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
		restore := func() { _ = stty(tty, "echo") }
		defer func() {
			restore()
			fmt.Fprintln(out)
		}()

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		done := make(chan struct{})
		defer func() {
			signal.Stop(sigs)
			close(done)
		}()
		go guardInterrupt(sigs, done, restore, out, exitProcess)

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
