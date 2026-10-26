package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/djeusette/cadre_cli/internal/api"
	"github.com/djeusette/cadre_cli/internal/config"
)

const (
	defaultPollInterval = 2 * time.Second
	msgBadURL           = "--url must be an http:// or https:// URL with a host, such as https://cadre.example.com."
)

// runLogin dispatches `login --url <url>` or `login --url=<url>`. The URL is required:
// it is the server to sign in to, and login stores it for every other command.
func runLogin(env Env, asJSON bool, args []string) int {
	raw, ok := loginURLArg(args)
	if !ok {
		return usageError(env)
	}
	serverURL, ok := parseServerURL(raw)
	if !ok {
		fmt.Fprintln(env.Stderr, msgBadURL)
		return 2
	}
	return runAgainst(env, asJSON, nil, 0, func(config.Config) string { return serverURL }, login)
}

func loginURLArg(args []string) (string, bool) {
	switch {
	case len(args) == 2 && args[0] == "--url":
		return args[1], true
	case len(args) == 1 && strings.HasPrefix(args[0], "--url="):
		return strings.TrimPrefix(args[0], "--url="), true
	default:
		return "", false
	}
}

// parseServerURL accepts an absolute http(s) URL with a host and no credentials, query or
// fragment, and returns it without a trailing slash.
func parseServerURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	ok := (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
	return strings.TrimRight(raw, "/"), ok
}

func login(ctx context.Context, s *session, _ []string) int {
	started, err := s.client.StartLogin(ctx)
	if err != nil {
		return s.failAPI(err)
	}

	out := s.progress()
	fmt.Fprintf(out, "Opening %s in your browser.\n", started.VerificationURI)
	fmt.Fprintf(out, "Check that the page shows this code: %s\n", started.UserCode)
	s.openBrowser(started.VerificationURI)

	redeemed, err := pollForApproval(ctx, s, started)
	switch {
	case errors.Is(err, api.ErrExpired), errors.Is(err, api.ErrNotFound):
		fmt.Fprintf(s.env.Stderr, "The code expired. Run cadre login --url %s again.\n", s.baseURL)
		return 1
	case err != nil:
		return s.failAPI(err)
	}

	saved := config.Config{URL: s.baseURL, APIKey: redeemed.APIKey, Email: redeemed.Email}
	if err := config.Save(s.cfgPath, saved); err != nil {
		return fail(s.env, fmt.Errorf("could not save %s: %w", s.cfgPath, err))
	}

	if s.asJSON {
		return printJSON(s, map[string]string{"email": redeemed.Email, "url": s.baseURL})
	}
	fmt.Fprintf(s.env.Stdout, "Signed in as %s.\n", redeemed.Email)
	return 0
}

// openBrowser opens uri only when it points at the server the CLI is talking to,
// so a hostile server cannot make the CLI launch file: or custom-scheme handlers.
// A failure to open is not an error: the URL is already printed for the user.
func (s *session) openBrowser(uri string) {
	if s.env.OpenBrowser == nil {
		return
	}
	if !sameServer(uri, s.baseURL) {
		fmt.Fprintf(s.env.Stderr, "Not opening an unexpected URL: %s\n", uri)
		return
	}
	_ = s.env.OpenBrowser(uri)
}

// sameServer reports whether uri is https (or http when base is http), on base's host and port, without userinfo.
func sameServer(uri, base string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	schemeOK := u.Scheme == "https" || (u.Scheme == "http" && b.Scheme == "http")
	return schemeOK && u.User == nil && u.Host != "" && u.Host == b.Host
}

// pollForApproval redeems the device code every interval until it is approved or gone.
// When the server rate limits it, it waits as long as the server asks, at least one
// interval, and goes on. Once expires_in seconds of waiting are spent it returns api.ErrExpired.
func pollForApproval(ctx context.Context, s *session, started api.Login) (api.Redeemed, error) {
	interval := time.Duration(started.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	remaining := time.Duration(started.ExpiresIn) * time.Second

	for wait := interval; wait <= remaining; {
		s.env.Sleep(wait)
		remaining -= wait

		redeemed, err := s.client.RedeemLogin(ctx, started.DeviceCode)
		var limited *api.RateLimitedError
		switch {
		case errors.Is(err, api.ErrPending):
			wait = interval
		case errors.As(err, &limited):
			wait = max(limited.RetryAfter, interval)
			fmt.Fprintf(s.progress(), "Too many requests from this address. Checking again in %s.\n", wait)
		default:
			return redeemed, err
		}
	}
	return api.Redeemed{}, api.ErrExpired
}
