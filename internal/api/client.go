// Package api is the client for Cadre's JSON API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrPending means the login has not been approved yet.
	ErrPending = errors.New("login is pending")
	// ErrExpired means the login code expired or was already redeemed.
	ErrExpired = errors.New("login code expired")
	// ErrNotFound means the server does not know the login code.
	ErrNotFound = errors.New("login code not found")
	// ErrUnauthorized means the API key is missing, expired or revoked.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrMilestoneNotFound means no project has the slug, or it has no milestone with the number.
	ErrMilestoneNotFound = errors.New("milestone not found")
)

// StatusError is an HTTP status the API contract does not define for the call.
type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("the server answered with an unexpected status %d", e.Status)
}

// RateLimitedError is the server's 429: this address sent too many requests.
// RetryAfter is how long the server asks to wait, or zero when it did not say.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	if e.RetryAfter <= 0 {
		return "too many requests"
	}
	return fmt.Sprintf("too many requests; retry after %s", e.RetryAfter)
}

// FieldError is one rejected field of a request.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationError is the server's 422 answer.
type ValidationError struct {
	Errors []FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	lines := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		lines[i] = fe.Path + ": " + fe.Message
	}
	return strings.Join(lines, "\n")
}

// Login is a started device login.
type Login struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Redeemed is an approved login.
type Redeemed struct {
	APIKey    string `json:"api_key"`
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at"`
}

// Me is the signed-in user.
type Me struct {
	Email string `json:"email"`
}

// Created is a registered project. Raw holds the response body as received.
type Created struct {
	Slug       string `json:"slug"`
	Milestones int    `json:"milestones"`
	URL        string `json:"url"`
	Raw        []byte `json:"-"`
}

// Pushed is a stored spec. Raw holds the response body as received.
type Pushed struct {
	Slug      string `json:"slug"`
	Milestone int    `json:"milestone"`
	URL       string `json:"url"`
	Raw       []byte `json:"-"`
}

const (
	maxResponseBytes = 1 << 20
	defaultTimeout   = 15 * time.Second
	maxRetryAfter    = 24 * time.Hour
)

// Client talks to one Cadre server. APIKey may be empty for the login calls.
type Client struct {
	BaseURL   string
	APIKey    string
	UserAgent string
	HTTP      *http.Client
}

// StartLogin begins a device login.
func (c *Client) StartLogin(ctx context.Context) (Login, error) {
	var login Login
	status, body, err := c.do(ctx, http.MethodPost, "/api/cli/logins", nil)
	if err != nil {
		return login, err
	}
	if status != http.StatusCreated {
		return login, &StatusError{Status: status}
	}
	return login, c.decode(status, body, &login)
}

// RedeemLogin exchanges an approved device code for an API key.
// It returns ErrPending, ErrExpired or ErrNotFound while there is no key to give.
func (c *Client) RedeemLogin(ctx context.Context, deviceCode string) (Redeemed, error) {
	var redeemed Redeemed
	payload, err := json.Marshal(map[string]string{"device_code": deviceCode})
	if err != nil {
		return redeemed, err
	}
	status, body, err := c.do(ctx, http.MethodPost, "/api/cli/logins/redeem", payload)
	if err != nil {
		return redeemed, err
	}
	switch status {
	case http.StatusOK:
		if err := c.decode(status, body, &redeemed); err != nil {
			return Redeemed{}, err
		}
		if redeemed.APIKey == "" || redeemed.Email == "" {
			return Redeemed{}, c.unexpected(status, errors.New("the answer has no api_key or email"))
		}
		return redeemed, nil
	case http.StatusAccepted:
		return redeemed, ErrPending
	case http.StatusGone:
		return redeemed, ErrExpired
	case http.StatusNotFound:
		return redeemed, ErrNotFound
	default:
		return redeemed, &StatusError{Status: status}
	}
}

// Me returns the user the API key belongs to, or ErrUnauthorized.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var me Me
	status, body, err := c.do(ctx, http.MethodGet, "/api/me", nil)
	if err != nil {
		return me, err
	}
	switch status {
	case http.StatusOK:
		return me, c.decode(status, body, &me)
	case http.StatusUnauthorized:
		return me, ErrUnauthorized
	default:
		return me, &StatusError{Status: status}
	}
}

// CreateProject posts body unchanged to the projects endpoint.
// It returns ErrUnauthorized or a *ValidationError when the server refuses it.
func (c *Client) CreateProject(ctx context.Context, body []byte) (Created, error) {
	var created Created
	status, resp, err := c.do(ctx, http.MethodPost, "/api/projects", body)
	if err != nil {
		return created, err
	}
	switch status {
	case http.StatusCreated:
		created.Raw = resp
		return created, c.decode(status, resp, &created)
	case http.StatusUnauthorized:
		return created, ErrUnauthorized
	case http.StatusUnprocessableEntity:
		return created, validationError(status, resp)
	default:
		return created, &StatusError{Status: status}
	}
}

// PushSpec stores spec, unchanged, as the newest version of the spec of milestone number in the project slug.
// It returns ErrMilestoneNotFound, ErrUnauthorized or a *ValidationError when the server refuses it.
func (c *Client) PushSpec(ctx context.Context, slug string, number int, spec string) (Pushed, error) {
	var pushed Pushed
	payload, err := json.Marshal(map[string]string{"spec": spec})
	if err != nil {
		return pushed, err
	}
	path := fmt.Sprintf("/api/projects/%s/milestones/%d/spec", url.PathEscape(slug), number)
	status, resp, err := c.do(ctx, http.MethodPut, path, payload)
	if err != nil {
		return pushed, err
	}
	switch status {
	case http.StatusOK:
		pushed.Raw = resp
		return pushed, c.decode(status, resp, &pushed)
	case http.StatusUnauthorized:
		return pushed, ErrUnauthorized
	case http.StatusNotFound:
		return pushed, ErrMilestoneNotFound
	case http.StatusUnprocessableEntity:
		return pushed, validationError(status, resp)
	default:
		return pushed, &StatusError{Status: status}
	}
}

// validationError reads a 422 body. One with no errors is an unexpected status.
func validationError(status int, body []byte) error {
	verr := &ValidationError{}
	if err := json.Unmarshal(body, verr); err != nil || len(verr.Errors) == 0 {
		return &StatusError{Status: status}
	}
	return verr
}

// do sends one request and returns the status and body. The key goes only in the Authorization header.
// A 429 also returns a *RateLimitedError, whatever the call.
func (c *Client) do(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		err = &RateLimitedError{RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	return resp.StatusCode, body, err
}

// retryAfter reads a Retry-After given in seconds, capped at a day. Any other form is zero.
func retryAfter(header string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(min(seconds, int64(maxRetryAfter/time.Second))) * time.Second
}

// decode parses a JSON body, naming the server and status when it is not JSON.
func (c *Client) decode(status int, body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return c.unexpected(status, err)
	}
	return nil
}

func (c *Client) unexpected(status int, err error) error {
	return fmt.Errorf("unexpected response from %s (status %d): %w", c.BaseURL, status, err)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}
