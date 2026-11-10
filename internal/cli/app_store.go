package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/djeusette/cadre_cli/internal/api"
)

const msgNoKey = "No key given. Pipe it in, or give the path of its file when asked."

// runAppStore dispatches `app-store key add <key-id> --issuer <issuer-id>` and `app-store key remove <key-id>`.
func runAppStore(env Env, asJSON bool, args []string) int {
	switch {
	case len(args) == 0:
		return showHelp(env, []string{"app-store"})
	case args[0] != "key":
		return unknownCommand(env, "app-store", args[0])
	case len(args) == 1:
		return showHelp(env, []string{"app-store", "key"})
	}
	rest := args[2:]
	switch args[1] {
	case "add":
		issuer, rest, ok := takeFlag(rest, "--issuer")
		if !ok || issuer == "" {
			return usageError(env, "app-store key add")
		}
		return runCommand(env, asJSON, "app-store key add", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return addAppStoreKey(ctx, s, strings.ToUpper(strings.TrimSpace(a[0])), issuer)
		})
	case "remove":
		return runCommand(env, asJSON, "app-store key remove", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return removeAccount(ctx, s, "app_store", "Connect key", strings.ToUpper(strings.TrimSpace(a[0])))
		})
	default:
		return unknownCommand(env, "app-store key", args[1])
	}
}

// addAppStoreKey connects the App Store Connect key keyID, whose .p8 file is read from stdin or at a path asked for.
// The token the server gets is a JSON object with the issuer and the key's text.
func addAppStoreKey(ctx context.Context, s *session, keyID, issuer string) int {
	if !s.requireKey() {
		return 1
	}
	pem, ok := s.keyFileText("Path to AuthKey_" + keyID + ".p8: ")
	if !ok {
		return 1
	}
	token, err := json.Marshal(map[string]string{"issuer_id": issuer, "private_key": pem})
	if err != nil {
		return s.failAPI(err)
	}
	return sendAccount(ctx, s, "app_store", "Connect key", keyID, string(token), func(added api.Account) string {
		return fmt.Sprintf("Connected the App Store Connect key %s: it reads %s.", added.Name, apps(added.Apps))
	})
}

// keyFileText is the text of the key file, trimmed, or false after saying why there is none. The key is never
// printed.
func (s *session) keyFileText(prompt string) (string, bool) {
	text, err := s.readKeyFile(prompt)
	if err != nil {
		fmt.Fprintf(s.env.Stderr, "Could not read the key file: %v\n", err)
		return "", false
	}
	if text == "" {
		fmt.Fprintln(s.env.Stderr, msgNoKey)
		return "", false
	}
	return text, true
}

// readKeyFile is a key file's text, trimmed. When stdin is piped, stdin is the key. On a terminal the person is asked
// on stderr for the file's path, which is not a secret and so is echoed, and the file is read.
func (s *session) readKeyFile(prompt string) (string, error) {
	if !s.env.stdinIsTerminal() {
		raw, err := io.ReadAll(s.env.stdin())
		return strings.TrimSpace(string(raw)), err
	}

	fmt.Fprint(s.env.Stderr, prompt)
	read := s.env.ReadPath
	if read == nil {
		read = readLineFromTTY
	}
	path, err := read()
	if err != nil {
		return "", err
	}
	path, err = expandHome(strings.TrimSpace(path))
	if err != nil || path == "" {
		return "", err
	}
	raw, err := os.ReadFile(path)
	return strings.TrimSpace(string(raw)), err
}

// expandHome replaces a leading ~/ with the home folder.
func expandHome(path string) (string, error) {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, rest), nil
}

// readLineFromTTY reads one line from the terminal device.
func readLineFromTTY() (string, error) {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return "", err
	}
	defer tty.Close()
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return line, nil
}
