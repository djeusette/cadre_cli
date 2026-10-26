package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/djeusette/cadre_cli/internal/api"
	"github.com/djeusette/cadre_cli/internal/config"
)

const msgNotSignedIn = "Not signed in. Run cadre login --url <url>."

// session is what a command needs once the config is loaded.
type session struct {
	env     Env
	asJSON  bool
	cfgPath string
	cfg     config.Config
	baseURL string
	client  *api.Client
}

type command func(ctx context.Context, s *session, args []string) int

// runCommand checks the argument count, loads the config and runs cmd against the stored server.
func runCommand(env Env, asJSON bool, args []string, wantArgs int, cmd command) int {
	return runAgainst(env, asJSON, args, wantArgs, config.BaseURL, cmd)
}

// runAgainst is runCommand with the server URL taken from serverURL(config) instead.
func runAgainst(env Env, asJSON bool, args []string, wantArgs int, serverURL func(config.Config) string, cmd command) int {
	if len(args) != wantArgs {
		return usageError(env)
	}

	cfgPath, err := config.Path(env.Getenv)
	if err != nil {
		return fail(env, err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fail(env, err)
	}

	baseURL := serverURL(cfg)
	s := &session{
		env:     env,
		asJSON:  asJSON,
		cfgPath: cfgPath,
		cfg:     cfg,
		baseURL: baseURL,
		client: &api.Client{
			BaseURL:   baseURL,
			APIKey:    keyFor(cfg, baseURL),
			UserAgent: "cadre-cli/" + env.Version,
			HTTP:      env.HTTP,
		},
	}
	return cmd(context.Background(), s, args)
}

// keyFor returns the stored API key only when baseURL is the server that issued it,
// so logging in to another server never sends it the key.
func keyFor(cfg config.Config, baseURL string) string {
	if strings.TrimRight(cfg.URL, "/") != baseURL {
		return ""
	}
	return cfg.APIKey
}

// progress is where status lines go: stdout, or stderr when stdout carries JSON.
func (s *session) progress() io.Writer {
	if s.asJSON {
		return s.env.Stderr
	}
	return s.env.Stdout
}

// requireKey reports whether there is an API key for the server in use, and says so if not.
func (s *session) requireKey() bool {
	if s.client.APIKey == "" || s.baseURL == "" {
		fmt.Fprintln(s.env.Stderr, msgNotSignedIn)
		return false
	}
	return true
}

// failAPI prints a failed API call to stderr and returns exit code 1.
func (s *session) failAPI(err error) int {
	var verr *api.ValidationError
	var limited *api.RateLimitedError
	switch {
	case errors.Is(err, api.ErrUnauthorized):
		fmt.Fprintf(s.env.Stderr, "Your key for %s is no longer valid. Run cadre login --url %s.\n", s.baseURL, s.baseURL)
	case errors.As(err, &limited):
		fmt.Fprintf(s.env.Stderr, "Too many requests to %s from this address. %s\n", s.baseURL, tryAgain(limited.RetryAfter))
	case errors.As(err, &verr):
		for _, fe := range verr.Errors {
			fmt.Fprintf(s.env.Stderr, "%s: %s\n", fe.Path, fe.Message)
		}
	default:
		fmt.Fprintf(s.env.Stderr, "Could not complete the request to %s: %v\n", s.baseURL, err)
	}
	return 1
}

// tryAgain says when to retry, or only that it can be retried when the server did not say.
func tryAgain(after time.Duration) string {
	if after <= 0 {
		return "Try again later."
	}
	return fmt.Sprintf("Try again in %s.", after)
}

func fail(env Env, err error) int {
	fmt.Fprintln(env.Stderr, err)
	return 1
}
