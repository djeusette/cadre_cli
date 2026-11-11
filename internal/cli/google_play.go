package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/djeusette/cadre_cli/internal/api"
)

const msgNotAServiceAccount = "That file isn't a service account's JSON key."

// runGooglePlay dispatches `google-play account add` and `google-play account remove <email>`.
func runGooglePlay(env Env, asJSON bool, args []string) int {
	switch {
	case len(args) == 0:
		return showHelp(env, []string{"google-play"})
	case args[0] != "account":
		return unknownCommand(env, "google-play", args[0])
	case len(args) == 1:
		return showHelp(env, []string{"google-play", "account"})
	}
	rest := args[2:]
	switch args[1] {
	case "add":
		return runCommand(env, asJSON, "google-play account add", rest, 0, func(ctx context.Context, s *session, _ []string) int {
			return addGooglePlayAccount(ctx, s)
		})
	case "remove":
		if blankArgument(rest) {
			return usageError(env, "google-play account remove")
		}
		return runCommand(env, asJSON, "google-play account remove", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return removeAccount(ctx, s, "google_play", "account", strings.ToLower(strings.TrimSpace(a[0])))
		})
	default:
		return unknownCommand(env, "google-play account", args[1])
	}
}

// addGooglePlayAccount connects the service account whose JSON key file is read from stdin or at a path asked for.
// The file is the token as it is, and its client_email is the account's name.
func addGooglePlayAccount(ctx context.Context, s *session) int {
	if !s.requireKey() {
		return 1
	}
	key, ok := s.keyFileText("Path to the service account's JSON key: ")
	if !ok {
		return 1
	}
	var file struct {
		ClientEmail string `json:"client_email"`
	}
	email := ""
	if json.Unmarshal([]byte(key), &file) == nil {
		email = strings.ToLower(strings.TrimSpace(file.ClientEmail))
	}
	if email == "" {
		fmt.Fprintln(s.env.Stderr, msgNotAServiceAccount)
		return 1
	}
	return sendAccount(ctx, s, "google_play", "account", email, key, func(added api.Account) string {
		return fmt.Sprintf("Connected the Google Play account %s: Google accepts its key.", added.Name)
	})
}
