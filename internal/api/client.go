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
	// ErrSpecChanged means a push named a base that is no longer the newest version of the spec.
	ErrSpecChanged = errors.New("spec changed since the base version")
	// ErrProjectNotFound means no project has the slug.
	ErrProjectNotFound = errors.New("project not found")
	// ErrProjectArchived means an update named a project that is archived.
	ErrProjectArchived = errors.New("project is archived")
	// ErrPullRequestNotFound means GitHub has no pull request at the URL a link named.
	ErrPullRequestNotFound = errors.New("pull request not found")
	// ErrGitHubNotConnected means the server has no GitHub App set up yet.
	ErrGitHubNotConnected = errors.New("github is not connected")
	// ErrGitHubUnreachable means the server could not reach GitHub.
	ErrGitHubUnreachable = errors.New("github is unreachable")
	// ErrRepositoryNotFound means the server has no such repo.
	ErrRepositoryNotFound = errors.New("repository not found")
	// ErrTargetNotFound means the repo has no target on that platform.
	ErrTargetNotFound = errors.New("target not found")
	// ErrPlatformNotConnected means the server has no access to the platform yet.
	ErrPlatformNotConnected = errors.New("platform is not connected")
	// ErrPlatformUnreachable means the server could not reach the platform.
	ErrPlatformUnreachable = errors.New("platform is unreachable")
)

// ProjectChangedError is the server's 409 for an update of a project that changed since it was pulled.
// Version is the project's current version.
type ProjectChangedError struct{ Version int }

func (e *ProjectChangedError) Error() string {
	return fmt.Sprintf("project changed since you pulled it; it is now at version %d", e.Version)
}

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

// Pulled is a milestone's newest spec version. Spec, Version, UpdatedBy and UpdatedAt are nil when the
// milestone has no spec. Raw holds the response body as received.
type Pulled struct {
	Slug      string  `json:"slug"`
	Milestone int     `json:"milestone"`
	URL       string  `json:"url"`
	Spec      *string `json:"spec"`
	Version   *string `json:"version"`
	UpdatedBy *string `json:"updated_by"`
	UpdatedAt *string `json:"updated_at"`
	Raw       []byte  `json:"-"`
}

// FileMilestone is a milestone as the project file lists it. Field order is the file's key order.
type FileMilestone struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Repos     []string `json:"repos"`
	DesignURL *string  `json:"design_url"`
}

// ProjectFile is the file `project pull` writes and `project update` reads. Field order is the file's key order.
type ProjectFile struct {
	Slug       string          `json:"slug"`
	Name       string          `json:"name"`
	EndGoal    string          `json:"end_goal"`
	HomeRepo   string          `json:"home_repo"`
	CanvasURL  *string         `json:"canvas_url"`
	Version    int             `json:"version"`
	Milestones []FileMilestone `json:"milestones"`
}

// PulledProject is a project as the server has it. Raw holds the response body as received.
type PulledProject struct {
	File     ProjectFile
	Archived bool
	URL      string
	Raw      []byte
}

// UpdatedProject is an updated project. Raw holds the response body as received.
type UpdatedProject struct {
	Slug    string `json:"slug"`
	Version int    `json:"version"`
	URL     string `json:"url"`
	Raw     []byte `json:"-"`
}

// ArchivedProject is a project after archive or unarchive. Raw holds the response body as received.
type ArchivedProject struct {
	Slug     string `json:"slug"`
	Archived bool   `json:"archived"`
	URL      string `json:"url"`
	Raw      []byte `json:"-"`
}

// Linked is a pull request after a link or an unlink. Milestone and ProjectURL are nil once it is unlinked.
// Raw holds the response body as received.
type Linked struct {
	Repo       string  `json:"repo"`
	Number     int     `json:"number"`
	URL        string  `json:"url"`
	Milestone  *string `json:"milestone"`
	ProjectURL *string `json:"project_url"`
	Raw        []byte  `json:"-"`
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
// A non-empty base is the version the spec was edited from: the server refuses the push with ErrSpecChanged
// when it is no longer the newest. It returns ErrMilestoneNotFound, ErrUnauthorized or a *ValidationError
// when the server refuses it.
func (c *Client) PushSpec(ctx context.Context, slug string, number int, spec, base string) (Pushed, error) {
	var pushed Pushed
	payload, err := json.Marshal(pushBody(spec, base))
	if err != nil {
		return pushed, err
	}
	status, resp, err := c.do(ctx, http.MethodPut, specPath(slug, number), payload)
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
	case http.StatusConflict:
		return pushed, ErrSpecChanged
	case http.StatusUnprocessableEntity:
		return pushed, validationError(status, resp)
	default:
		return pushed, &StatusError{Status: status}
	}
}

// PullSpec returns the newest version of the spec of milestone number in the project slug.
// It returns ErrMilestoneNotFound or ErrUnauthorized when the server refuses it.
func (c *Client) PullSpec(ctx context.Context, slug string, number int) (Pulled, error) {
	var pulled Pulled
	status, resp, err := c.do(ctx, http.MethodGet, specPath(slug, number), nil)
	if err != nil {
		return pulled, err
	}
	switch status {
	case http.StatusOK:
		pulled.Raw = resp
		return pulled, c.decode(status, resp, &pulled)
	case http.StatusUnauthorized:
		return pulled, ErrUnauthorized
	case http.StatusNotFound:
		return pulled, ErrMilestoneNotFound
	default:
		return pulled, &StatusError{Status: status}
	}
}

// PullProject returns the project slug as the file `project update` takes.
// It returns ErrProjectNotFound or ErrUnauthorized when the server refuses it.
func (c *Client) PullProject(ctx context.Context, slug string) (PulledProject, error) {
	var pulled PulledProject
	status, resp, err := c.do(ctx, http.MethodGet, projectPath(slug, ""), nil)
	if err != nil {
		return pulled, err
	}
	if status != http.StatusOK {
		return pulled, projectStatusError(status, resp)
	}
	var meta struct {
		Archived bool   `json:"archived"`
		URL      string `json:"url"`
	}
	if err := c.decode(status, resp, &pulled.File); err != nil {
		return PulledProject{}, err
	}
	if err := c.decode(status, resp, &meta); err != nil {
		return PulledProject{}, err
	}
	pulled.Archived, pulled.URL, pulled.Raw = meta.Archived, meta.URL, resp
	return pulled, nil
}

// UpdateProject puts body, unchanged, to the project slug. It returns ErrProjectNotFound, ErrUnauthorized,
// ErrProjectArchived, a *ProjectChangedError or a *ValidationError when the server refuses it.
func (c *Client) UpdateProject(ctx context.Context, slug string, body []byte) (UpdatedProject, error) {
	var updated UpdatedProject
	status, resp, err := c.do(ctx, http.MethodPut, projectPath(slug, ""), body)
	if err != nil {
		return updated, err
	}
	if status != http.StatusOK {
		return updated, projectStatusError(status, resp)
	}
	updated.Raw = resp
	return updated, c.decode(status, resp, &updated)
}

// ArchiveProject archives the project slug.
func (c *Client) ArchiveProject(ctx context.Context, slug string) (ArchivedProject, error) {
	return c.setArchived(ctx, slug, "/archive")
}

// UnarchiveProject brings the project slug back.
func (c *Client) UnarchiveProject(ctx context.Context, slug string) (ArchivedProject, error) {
	return c.setArchived(ctx, slug, "/unarchive")
}

func (c *Client) setArchived(ctx context.Context, slug, action string) (ArchivedProject, error) {
	var archived ArchivedProject
	status, resp, err := c.do(ctx, http.MethodPost, projectPath(slug, action), nil)
	if err != nil {
		return archived, err
	}
	if status != http.StatusOK {
		return archived, projectStatusError(status, resp)
	}
	archived.Raw = resp
	return archived, c.decode(status, resp, &archived)
}

// LinkPullRequest links the pull request at prURL to milestone, written <project>/<n>, or unlinks it when milestone
// is nil. The server reads the pull request from GitHub first. It returns ErrMilestoneNotFound,
// ErrPullRequestNotFound, ErrGitHubNotConnected, ErrGitHubUnreachable, ErrUnauthorized or a *ValidationError when the
// server refuses it.
func (c *Client) LinkPullRequest(ctx context.Context, prURL string, milestone *string) (Linked, error) {
	var linked Linked
	payload, err := json.Marshal(linkBody{URL: prURL, Milestone: milestone})
	if err != nil {
		return linked, err
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/api/pull-requests/link", payload)
	if err != nil {
		return linked, err
	}
	switch status {
	case http.StatusOK:
		linked.Raw = resp
		return linked, c.decode(status, resp, &linked)
	case http.StatusUnauthorized:
		return linked, ErrUnauthorized
	case http.StatusNotFound:
		return linked, linkNotFound(status, resp)
	case http.StatusServiceUnavailable:
		return linked, ErrGitHubNotConnected
	case http.StatusBadGateway:
		return linked, ErrGitHubUnreachable
	case http.StatusUnprocessableEntity:
		return linked, validationError(status, resp)
	default:
		return linked, &StatusError{Status: status}
	}
}

// linkBody is a link call's JSON body. A nil Milestone is sent as null, which unlinks.
type linkBody struct {
	URL       string  `json:"url"`
	Milestone *string `json:"milestone"`
}

// linkNotFound tells a missing milestone from a pull request GitHub does not have.
func linkNotFound(status int, body []byte) error {
	var answer struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &answer)
	switch answer.Error {
	case "milestone_not_found":
		return ErrMilestoneNotFound
	case "pull_request_not_found":
		return ErrPullRequestNotFound
	}
	return &StatusError{Status: status}
}

// Repository is a repo with its deploy targets. Raw holds the response body as received.
type Repository struct {
	Name           string   `json:"name"`
	GitHubFullName string   `json:"github_full_name"`
	Targets        []Target `json:"targets"`
	Raw            []byte   `json:"-"`
}

// Target is a repo's deploy target on one platform. Repo is empty in a Repository's targets.
// Tracked is whether cadre can read that platform's deploys. Raw holds the response body as received.
type Target struct {
	Repo     string            `json:"repo"`
	Platform string            `json:"platform"`
	Settings map[string]string `json:"settings"`
	Tracked  bool              `json:"tracked"`
	Raw      []byte            `json:"-"`
}

// Unset is a removed deploy target. Raw holds the response body as received.
type Unset struct {
	Repo     string `json:"repo"`
	Platform string `json:"platform"`
	Raw      []byte `json:"-"`
}

// ShowRepository returns the repo and its deploy targets. It returns ErrRepositoryNotFound or ErrUnauthorized
// when the server refuses it.
func (c *Client) ShowRepository(ctx context.Context, repo string) (Repository, error) {
	var shown Repository
	status, resp, err := c.do(ctx, http.MethodGet, repositoryPath(repo, ""), nil)
	if err != nil {
		return shown, err
	}
	if status != http.StatusOK {
		return shown, repositoryStatusError(status, resp)
	}
	shown.Raw = resp
	return shown, c.decode(status, resp, &shown)
}

// SetTarget sets the repo's target on platform, replacing its settings. The server reads the platform first. It
// returns ErrRepositoryNotFound, ErrPlatformNotConnected, ErrPlatformUnreachable, ErrUnauthorized or a
// *ValidationError when the server refuses it.
func (c *Client) SetTarget(ctx context.Context, repo, platform string, settings map[string]string) (Target, error) {
	var target Target
	if settings == nil {
		settings = map[string]string{}
	}
	payload, err := json.Marshal(map[string]map[string]string{"settings": settings})
	if err != nil {
		return target, err
	}
	status, resp, err := c.do(ctx, http.MethodPut, targetPath(repo, platform), payload)
	if err != nil {
		return target, err
	}
	switch status {
	case http.StatusOK:
		target.Raw = resp
		return target, c.decode(status, resp, &target)
	case http.StatusServiceUnavailable:
		return target, ErrPlatformNotConnected
	case http.StatusBadGateway:
		return target, ErrPlatformUnreachable
	default:
		return target, repositoryStatusError(status, resp)
	}
}

// UnsetTarget removes the repo's target on platform. It returns ErrRepositoryNotFound, ErrTargetNotFound or
// ErrUnauthorized when the server refuses it.
func (c *Client) UnsetTarget(ctx context.Context, repo, platform string) (Unset, error) {
	var removed Unset
	status, resp, err := c.do(ctx, http.MethodDelete, targetPath(repo, platform), nil)
	if err != nil {
		return removed, err
	}
	if status != http.StatusOK {
		return removed, repositoryStatusError(status, resp)
	}
	removed.Raw = resp
	return removed, c.decode(status, resp, &removed)
}

// repositoryStatusError maps a repository call's non-200 answer to its error, telling the 404s apart by code.
func repositoryStatusError(status int, body []byte) error {
	switch status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &answer)
		switch answer.Error {
		case "repository_not_found":
			return ErrRepositoryNotFound
		case "target_not_found":
			return ErrTargetNotFound
		}
	case http.StatusUnprocessableEntity:
		return validationError(status, body)
	}
	return &StatusError{Status: status}
}

func repositoryPath(repo, suffix string) string {
	return "/api/repositories/" + url.PathEscape(repo) + suffix
}

func targetPath(repo, platform string) string {
	return repositoryPath(repo, "/targets/"+url.PathEscape(platform))
}

// projectStatusError maps a project call's non-200 answer to its error.
func projectStatusError(status int, body []byte) error {
	switch status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrProjectNotFound
	case http.StatusConflict:
		var conflict struct {
			Error   string `json:"error"`
			Version int    `json:"version"`
		}
		_ = json.Unmarshal(body, &conflict)
		switch conflict.Error {
		case "project_changed":
			return &ProjectChangedError{Version: conflict.Version}
		case "project_archived":
			return ErrProjectArchived
		}
	case http.StatusUnprocessableEntity:
		return validationError(status, body)
	}
	return &StatusError{Status: status}
}

func projectPath(slug, suffix string) string {
	return "/api/projects/" + url.PathEscape(slug) + suffix
}

// pushBody is a push's JSON body: the spec, and the base only when there is one.
func pushBody(spec, base string) map[string]string {
	if base == "" {
		return map[string]string{"spec": spec}
	}
	return map[string]string{"spec": spec, "base_version": base}
}

func specPath(slug string, number int) string {
	return fmt.Sprintf("/api/projects/%s/milestones/%d/spec", url.PathEscape(slug), number)
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
