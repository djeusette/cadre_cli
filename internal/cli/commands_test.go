package cli_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djeusette/cadre_cli/internal/cli"
	"github.com/djeusette/cadre_cli/internal/config"
)

type harness struct {
	deps    cli.Env
	out     *strings.Builder
	errb    *strings.Builder
	cfgPath string
	opened  []string
	sleeps  []time.Duration
}

func newHarness(t *testing.T, srv *httptest.Server, cfg *config.Config) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{out: &strings.Builder{}, errb: &strings.Builder{}, cfgPath: filepath.Join(dir, "cadre", "config.json")}
	if cfg != nil {
		if cfg.URL == "" {
			cfg.URL = srv.URL
		}
		if err := config.Save(h.cfgPath, *cfg); err != nil {
			t.Fatal(err)
		}
	}
	h.deps = cli.Env{
		Stdout: h.out,
		Stderr: h.errb,
		Getenv: func(k string) string {
			switch k {
			case "XDG_CONFIG_HOME":
				return dir
			}
			return ""
		},
		OpenBrowser: func(u string) error { h.opened = append(h.opened, u); return nil },
		Sleep:       func(d time.Duration) { h.sleeps = append(h.sleeps, d) },
		HTTP:        srv.Client(),
		Version:     "1.2.3",
	}
	return h
}

func (h *harness) load(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func loginServer(t *testing.T, expiresIn int, redeem func(n int, w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	return loginServerWithURI(t, expiresIn, func(base string) string { return base + "/cli/authorize?code=ABCD-EFGH" }, redeem)
}

func loginServerWithURI(t *testing.T, expiresIn int, uriFor func(base string) string, redeem func(n int, w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	var polls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cli/logins":
			writeJSON(w, 201, `{"user_code":"ABCD-EFGH","device_code":"dev","verification_uri":"`+uriFor(srv.URL)+`","expires_in":`+strconv.Itoa(expiresIn)+`,"interval":2}`)
		case "/api/cli/logins/redeem":
			redeem(int(atomic.AddInt32(&polls, 1)), w)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func expiredCopy(url string) string {
	return "The code expired. Run cadre login --url " + url + " again."
}

func TestLoginSavesKeyAfterApproval(t *testing.T) {
	srv, _ := loginServer(t, 600, func(n int, w http.ResponseWriter) {
		if n < 3 {
			writeJSON(w, 202, `{"status":"pending"}`)
			return
		}
		writeJSON(w, 200, `{"api_key":"cadre_abc","email":"ada@colette.club","expires_at":"2027-01-01T00:00:00Z"}`)
	})
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	uri := srv.URL + "/cli/authorize?code=ABCD-EFGH"
	want := "Opening " + uri + " in your browser.\nCheck that the page shows this code: ABCD-EFGH\nSigned in as ada@colette.club.\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
	if len(h.opened) != 1 || h.opened[0] != uri {
		t.Errorf("opened = %v", h.opened)
	}
	if got := h.load(t); got != (config.Config{URL: srv.URL, APIKey: "cadre_abc", Email: "ada@colette.club"}) {
		t.Errorf("config = %+v", got)
	}
	if strings.Contains(h.out.String()+h.errb.String(), "cadre_abc") {
		t.Error("key was printed")
	}
	if len(h.sleeps) != 3 || h.sleeps[0] != 2*time.Second {
		t.Errorf("sleeps = %v, want three of 2s", h.sleeps)
	}
	assertMode(t, h.cfgPath, 0o600)
	assertMode(t, filepath.Dir(h.cfgPath), 0o700)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func TestLoginOpensOnlyTheServersOwnURL(t *testing.T) {
	approve := func(_ int, w http.ResponseWriter) {
		writeJSON(w, 200, `{"api_key":"k","email":"ada@colette.club","expires_at":"x"}`)
	}
	tests := []struct {
		name     string
		uriFor   func(base string) string
		wantOpen bool
	}{
		{"base host", func(b string) string { return b + "/cli/authorize?code=ABCD-EFGH" }, true},
		{"file scheme", func(string) string { return "file:///etc/passwd" }, false},
		{"custom scheme", func(b string) string { return "vscode://" + strings.TrimPrefix(b, "http://") }, false},
		{"other host", func(string) string { return "http://evil.example/cli/authorize" }, false},
		{"other port", func(string) string { return "http://127.0.0.1:1/cli/authorize" }, false},
		{"userinfo", func(b string) string { return "http://user:pw@" + strings.TrimPrefix(b, "http://") + "/x" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := loginServerWithURI(t, 600, tt.uriFor, approve)
			h := newHarness(t, srv, nil)
			uri := tt.uriFor(srv.URL)

			if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 0 {
				t.Fatalf("code = %d, stderr %q", code, h.errb)
			}
			if !strings.Contains(h.out.String(), "Opening "+uri+" in your browser.") {
				t.Errorf("stdout = %q", h.out)
			}
			if tt.wantOpen {
				if len(h.opened) != 1 || h.opened[0] != uri {
					t.Errorf("opened = %v", h.opened)
				}
				return
			}
			if len(h.opened) != 0 {
				t.Errorf("opened = %v, want none", h.opened)
			}
			if !strings.Contains(h.errb.String(), "Not opening an unexpected URL: "+uri) {
				t.Errorf("stderr = %q", h.errb)
			}
		})
	}
}

func TestLoginRejectsAnswerWithoutKey(t *testing.T) {
	srv, _ := loginServer(t, 600, func(_ int, w http.ResponseWriter) {
		writeJSON(w, 200, `{"api_key":"","email":"ada@colette.club","expires_at":"x"}`)
	})
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if strings.Contains(h.out.String(), "Signed in") || h.load(t).APIKey != "" {
		t.Errorf("stdout = %q, config = %+v", h.out, h.load(t))
	}
}

func TestLoginExpired(t *testing.T) {
	srv, _ := loginServer(t, 600, func(_ int, w http.ResponseWriter) { writeJSON(w, 410, `{"error":"expired"}`) })
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), expiredCopy(srv.URL)) {
		t.Errorf("stderr = %q", h.errb)
	}
	if h.load(t).APIKey != "" {
		t.Error("config was written")
	}
}

func TestLoginNotFoundMeansExpired(t *testing.T) {
	srv, _ := loginServer(t, 600, func(_ int, w http.ResponseWriter) { writeJSON(w, 404, `{"error":"not_found"}`) })
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 1 || !strings.Contains(h.errb.String(), expiredCopy(srv.URL)) {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestLoginStopsAtExpiresIn(t *testing.T) {
	srv, polls := loginServer(t, 4, func(_ int, w http.ResponseWriter) { writeJSON(w, 202, `{"status":"pending"}`) })
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if *polls != 2 {
		t.Errorf("polls = %d, want 2", *polls)
	}
	if !strings.Contains(h.errb.String(), expiredCopy(srv.URL)) {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestLoginWaitsWhenRedeemIsRateLimited(t *testing.T) {
	srv, polls := loginServer(t, 600, func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			w.Header().Set("Retry-After", "5")
			writeJSON(w, 429, `{"error":"rate_limited"}`)
		case 2:
			writeJSON(w, 429, `{"error":"rate_limited"}`)
		default:
			writeJSON(w, 200, `{"api_key":"cadre_abc","email":"ada@colette.club","expires_at":"x"}`)
		}
	})
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if *polls != 3 {
		t.Errorf("polls = %d, want 3", *polls)
	}
	if want := []time.Duration{2 * time.Second, 5 * time.Second, 2 * time.Second}; !slices.Equal(h.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", h.sleeps, want)
	}
	for _, line := range []string{
		"Too many requests from this address. Checking again in 5s.\n",
		"Too many requests from this address. Checking again in 2s.\n",
		"Signed in as ada@colette.club.\n",
	} {
		if !strings.Contains(h.out.String(), line) {
			t.Errorf("stdout = %q, want it to contain %q", h.out, line)
		}
	}
}

func TestLoginRateLimitedUntilTheCodeExpires(t *testing.T) {
	srv, polls := loginServer(t, 10, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "4")
		writeJSON(w, 429, `{"error":"rate_limited"}`)
	})
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if *polls != 3 {
		t.Errorf("polls = %d, want 3", *polls)
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second, 4 * time.Second}; !slices.Equal(h.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", h.sleeps, want)
	}
	if !strings.Contains(h.errb.String(), expiredCopy(srv.URL)) {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestLoginStartRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, 429, `{"error":"rate_limited"}`)
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "Too many requests to " + srv.URL + " from this address. Try again in 30s.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestLoginWhenBrowserFails(t *testing.T) {
	srv, _ := loginServer(t, 600, func(_ int, w http.ResponseWriter) {
		writeJSON(w, 200, `{"api_key":"k","email":"ada@colette.club","expires_at":"x"}`)
	})
	h := newHarness(t, srv, nil)
	h.deps.OpenBrowser = func(string) error { return io.ErrClosedPipe }

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if !strings.Contains(h.out.String(), "/cli/authorize?code=ABCD-EFGH") {
		t.Errorf("stdout = %q", h.out)
	}
}

func meServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" || r.Header.Get("Authorization") != "Bearer cadre_abc" {
			t.Errorf("bad request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if ua := r.Header.Get("User-Agent"); ua != "cadre-cli/1.2.3" {
			t.Errorf("User-Agent = %q", ua)
		}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWhoami(t *testing.T) {
	srv := meServer(t, 200, `{"email":"ada@colette.club"}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc", Email: "old@x"})

	if code := cli.Run([]string{"whoami"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Signed in to " + srv.URL + " as ada@colette.club.\n"; h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}

	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"whoami", "--json"}, h.deps); code != 0 {
		t.Fatalf("code = %d", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(h.out.String()), &got); err != nil || got["email"] != "ada@colette.club" || got["url"] != srv.URL {
		t.Errorf("json = %q, %v", h.out, err)
	}
}

func TestWhoamiNotSignedIn(t *testing.T) {
	srv := meServer(t, 200, `{}`)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"whoami"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), "Not signed in. Run cadre login --url <url>.") {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestWhoamiRateLimited(t *testing.T) {
	srv := meServer(t, 429, `{"error":"rate_limited"}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"whoami"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "Too many requests to " + srv.URL + " from this address. Try again later.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestWhoamiInvalidKey(t *testing.T) {
	srv := meServer(t, 401, `{"error":"unauthorized"}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"whoami"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), "Your key for "+srv.URL+" is no longer valid. Run cadre login --url "+srv.URL+".") {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestWhoamiNetworkErrorHidesKey(t *testing.T) {
	srv := meServer(t, 200, `{}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	srv.Close()

	if code := cli.Run([]string{"whoami"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if h.errb.Len() == 0 || strings.Contains(h.errb.String(), "cadre_abc") {
		t.Errorf("stderr = %q", h.errb)
	}
}

func projectServer(t *testing.T, status int, body string, gotBody *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		if r.URL.Path != "/api/projects" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer cadre_abc" {
			t.Errorf("bad request %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectCreate(t *testing.T) {
	var got string
	resp := `{"slug":"wish-reminders","milestones":2,"url":"https://cadre.example/"}`
	srv := projectServer(t, 201, resp, &got)
	file := writeFile(t, " {\"name\": \"Wish\"}\n")
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", file}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Registered wish-reminders with 2 milestones: https://cadre.example/\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got != " {\"name\": \"Wish\"}\n" {
		t.Errorf("body = %q", got)
	}

	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"--json", "project", "create", file}, h.deps); code != 0 || h.out.String() != resp {
		t.Errorf("code = %d, stdout = %q", code, h.out)
	}
}

func TestProjectCreateValidation(t *testing.T) {
	var got string
	srv := projectServer(t, 422, `{"errors":[{"path":"name","message":"is required"},{"path":"milestones.1.repos.0","message":"is invalid"}]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "name: is required\nmilestones.1.repos.0: is invalid\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestProjectCreateEmptyValidationErrorsFailsGenerically(t *testing.T) {
	var got string
	srv := projectServer(t, 422, `{"errors":[]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps); code != 1 || !strings.Contains(h.errb.String(), "422") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateNonJSONResponse(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `<html>proxy</html>`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps)
	if code != 1 || !strings.Contains(h.errb.String(), "unexpected response from "+srv.URL+" (status 201)") || strings.Contains(h.errb.String(), "cadre_abc") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateInvalidKey(t *testing.T) {
	var got string
	srv := projectServer(t, 401, `{"error":"unauthorized"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "Your key for "+srv.URL+" is no longer valid. Run cadre login --url "+srv.URL+".") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateNotSignedIn(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `{}`, &got)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "Not signed in. Run cadre login --url <url>.") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateMissingFile(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `{}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", "/nope/missing.json"}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "/nope/missing.json") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateUsage(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `{}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create"}, h.deps); code != 2 || !strings.Contains(h.errb.String(), "Usage: cadre") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestCommandsUseOnlyTheStoredURL(t *testing.T) {
	var otherHits int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&otherHits, 1)
		writeJSON(w, 200, `{"email":"eve@evil.example"}`)
	}))
	t.Cleanup(other.Close)
	srv := meServer(t, 200, `{"email":"ada@colette.club"}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.deps.Getenv = func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return filepath.Dir(filepath.Dir(h.cfgPath))
		case "CADRE_URL":
			return other.URL
		}
		return ""
	}

	if code := cli.Run([]string{"whoami"}, h.deps); code != 0 || h.out.String() != "Signed in to "+srv.URL+" as ada@colette.club.\n" {
		t.Errorf("code = %d, stdout %q, stderr %q", code, h.out, h.errb)
	}
	if n := atomic.LoadInt32(&otherHits); n != 0 {
		t.Errorf("CADRE_URL's server was called %d times", n)
	}
}

func TestNotSignedInWithoutAStoredURL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(w, 200, `{}`)
	}))
	t.Cleanup(srv.Close)

	for _, args := range [][]string{{"whoami"}, {"project", "create", writeFile(t, "{}")}, {"spec", "push", "wish-reminders/1", writeSpec(t, "x")}} {
		h := newHarness(t, srv, nil)
		if code := cli.Run(args, h.deps); code != 1 || h.errb.String() != "Not signed in. Run cadre login --url <url>.\n" {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("a server was called %d times", n)
	}
}

func TestLoginRequiresAnHTTPURL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(w, 500, `{}`)
	}))
	t.Cleanup(srv.Close)

	for _, args := range [][]string{{"login"}, {"login", "--url"}, {"login", srv.URL}, {"login", "--url", srv.URL, "extra"}} {
		h := newHarness(t, srv, nil)
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageLogin {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	for _, url := range []string{"", "colette-cadre.fly.dev", "ftp://cadre.example", "https://", "https://user:pw@cadre.example", "https://cadre.example/?a=1", "https://cadre.example/#x"} {
		h := newHarness(t, srv, nil)
		want := "--url must be an http:// or https:// URL with a host, such as https://cadre.example.com.\n"
		if code := cli.Run([]string{"login", "--url=" + url}, h.deps); code != 2 || h.errb.String() != want {
			t.Errorf("%q: code = %d, stderr %q", url, code, h.errb)
		}
		if _, err := os.Stat(h.cfgPath); !os.IsNotExist(err) {
			t.Errorf("%q: config written", url)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("the server was called %d times", n)
	}
}

func TestLoginStoresTheGivenURLWithoutItsTrailingSlash(t *testing.T) {
	approve := func(_ int, w http.ResponseWriter) {
		writeJSON(w, 200, `{"api_key":"cadre_abc","email":"ada@colette.club","expires_at":"x"}`)
	}
	for _, form := range []func(string) []string{
		func(u string) []string { return []string{"login", "--url", u + "/"} },
		func(u string) []string { return []string{"login", "--url=" + u} },
	} {
		srv, _ := loginServer(t, 600, approve)
		h := newHarness(t, srv, nil)
		if code := cli.Run(form(srv.URL), h.deps); code != 0 {
			t.Fatalf("code = %d, stderr %q", code, h.errb)
		}
		if got := h.load(t); got.URL != srv.URL {
			t.Errorf("stored URL = %q, want %q", got.URL, srv.URL)
		}
	}
}

func TestLoginOnAnotherServerReplacesTheStoredURL(t *testing.T) {
	srv, _ := loginServer(t, 600, func(n int, w http.ResponseWriter) {
		writeJSON(w, 200, `{"api_key":"cadre_new","email":"ada@colette.club","expires_at":"2027-01-01T00:00:00Z"}`)
	})
	h := newHarness(t, srv, &config.Config{URL: "https://issuer.example", APIKey: "cadre_old"})

	if code := cli.Run([]string{"login", "--url", srv.URL}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if got := h.load(t); got != (config.Config{URL: srv.URL, APIKey: "cadre_new", Email: "ada@colette.club"}) {
		t.Errorf("config = %+v", got)
	}
}

func TestProjectCreateRefusesInvalidJSON(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `{}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	file := writeFile(t, `{"name": `)

	if code := cli.Run([]string{"project", "create", file}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), file) || !strings.Contains(h.errb.String(), "not valid JSON") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
	if got != "" {
		t.Errorf("a request was sent: %q", got)
	}
}

type specRequest struct {
	method, path, auth string
	body               map[string]string
}

func specServer(t *testing.T, status int, body string, got *specRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = specRequest{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization")}
		if err := json.Unmarshal(b, &got.body); err != nil {
			t.Errorf("body %q: %v", b, err)
		}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const pushedBody = `{"slug":"wish-reminders","milestone":2,"url":"https://cadre.example/projects/wish-reminders"}`

func TestSpecPush(t *testing.T) {
	var got specRequest
	srv := specServer(t, 200, pushedBody, &got)
	spec := "# Wish reminders 2\n\n    indented\n"
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", writeSpec(t, spec)}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Pushed the spec of wish-reminders/2: https://cadre.example/projects/wish-reminders\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "PUT" || got.path != "/api/projects/wish-reminders/milestones/2/spec" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
	if len(got.body) != 1 || got.body["spec"] != spec {
		t.Errorf("body = %q", got.body)
	}
}

func TestSpecPushJSON(t *testing.T) {
	var got specRequest
	srv := specServer(t, 200, pushedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", writeSpec(t, "x"), "--json"}, h.deps); code != 0 || h.out.String() != pushedBody {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}
}

func TestSpecPushNotFound(t *testing.T) {
	var got specRequest
	srv := specServer(t, 404, `{"error":"not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "push", "wish-reminders/9", writeSpec(t, "x")}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "No milestone wish-reminders/9 on " + srv.URL + ".\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestSpecPushValidation(t *testing.T) {
	var got specRequest
	srv := specServer(t, 422, `{"errors":[{"path":"spec","message":"is required"}]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", writeSpec(t, "x")}, h.deps); code != 1 || h.errb.String() != "spec: is required\n" {
		t.Errorf("code = %d, stderr = %q", code, h.errb)
	}
}

func TestSpecPushUsage(t *testing.T) {
	var got specRequest
	srv := specServer(t, 200, pushedBody, &got)
	file := writeSpec(t, "x")
	cases := [][]string{
		{"spec", "push", "wish-reminders", file},
		{"spec", "push", "wish-reminders/0", file},
		{"spec", "push", "wish-reminders/02", file},
		{"spec", "push", "Wish/1", file},
		{"spec", "push", "/1", file},
		{"spec", "push", "a/1/2", file},
		{"spec", "push", "wish--reminders/1", file},
		{"spec", "push", "wish-reminders/99999999999999999999", file},
		{"spec", "push", "wish-reminders/1"},
		{"spec", "push", "wish-reminders/1", file, "--base"},
		{"spec", "push", "wish-reminders/1", file, "--base=v1", "--base", "v2"},
		{"spec", "push", "wish-reminders/1", file, "--base="},
		{"spec", "push", "wish-reminders/1", file, "--base", ""},
		{"spec", "push", "wish-reminders/1", file, "--base", "-v1"},
		{"spec", "push"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageSpecPush {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.method != "" {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestSpecPushMissingFile(t *testing.T) {
	var got specRequest
	srv := specServer(t, 200, pushedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", "/nope/missing.md"}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "/nope/missing.md") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestSpecPushNotUTF8(t *testing.T) {
	var got specRequest
	srv := specServer(t, 200, pushedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	file := writeSpec(t, "\xff\xfe")

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", file}, h.deps); code != 1 || h.errb.String() != file+" is not UTF-8 text.\n" {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
	if got.method != "" {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestSpecPushNotSignedIn(t *testing.T) {
	var got specRequest
	srv := specServer(t, 200, pushedBody, &got)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", writeSpec(t, "x")}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "Not signed in. Run cadre login --url <url>.") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestSpecPushInvalidKey(t *testing.T) {
	var got specRequest
	srv := specServer(t, 401, `{"error":"unauthorized"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", writeSpec(t, "x")}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "Your key for "+srv.URL+" is no longer valid. Run cadre login --url "+srv.URL+".") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func pullServer(t *testing.T, status int, body string, got *specRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = specRequest{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization")}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	pulledSpec   = "  lead\r\n\n    code\n\n"
	pulledBody   = `{"slug":"wish-reminders","milestone":2,"url":"https://cadre.example/projects/wish-reminders","spec":"  lead\r\n\n    code\n\n","version":"0c6f6e2a-7a51-4c55-9a6b-2b1f3c4d5e6f","updated_by":"ada@colette.club","updated_at":"2026-10-26T15:00:00.000000Z"}`
	noSpecBody   = `{"slug":"wish-reminders","milestone":2,"url":"https://cadre.example/projects/wish-reminders","spec":null,"version":null,"updated_by":null,"updated_at":null}`
	specChangedJ = `{"error":"spec_changed"}`
)

func TestSpecPullToStdout(t *testing.T) {
	var got specRequest
	srv := pullServer(t, 200, pulledBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "pull", "wish-reminders/2"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.out.String() != pulledSpec || h.errb.String() != "" {
		t.Errorf("stdout = %q, stderr = %q", h.out, h.errb)
	}
	if got.method != "GET" || got.path != "/api/projects/wish-reminders/milestones/2/spec" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
}

func TestSpecPullToFile(t *testing.T) {
	for _, flag := range [][]string{{"-o", "OUT"}, {"-o=OUT"}} {
		var got specRequest
		srv := pullServer(t, 200, pulledBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		out := filepath.Join(t.TempDir(), "spec.md")
		args := []string{"spec", "pull", "wish-reminders/2"}
		for _, f := range flag {
			args = append(args, strings.ReplaceAll(f, "OUT", out))
		}

		if code := cli.Run(args, h.deps); code != 0 {
			t.Fatalf("%q: code = %d, stderr %q", flag, code, h.errb)
		}
		if b, err := os.ReadFile(out); err != nil || string(b) != pulledSpec {
			t.Errorf("%q: file = %q, %v", flag, b, err)
		}
		assertMode(t, out, 0o644)
		if want := "Pulled the spec of wish-reminders/2 to " + out + ".\n"; h.out.String() != want {
			t.Errorf("%q: stdout = %q, want %q", flag, h.out, want)
		}
	}
}

func TestSpecPullReplacesAFile(t *testing.T) {
	var got specRequest
	srv := pullServer(t, 200, pulledBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	out := writeSpec(t, "an older, longer text that must not survive")

	if code := cli.Run([]string{"spec", "pull", "wish-reminders/2", "-o", out}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if b, _ := os.ReadFile(out); string(b) != pulledSpec {
		t.Errorf("file = %q", b)
	}
}

func TestSpecPullJSON(t *testing.T) {
	var got specRequest
	srv := pullServer(t, 200, pulledBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	out := filepath.Join(t.TempDir(), "spec.md")

	if code := cli.Run([]string{"spec", "pull", "wish-reminders/2", "-o", out, "--json"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.out.String() != pulledBody {
		t.Errorf("stdout = %q", h.out)
	}
	if want := "Pulled the spec of wish-reminders/2 to " + out + ".\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
	if b, _ := os.ReadFile(out); string(b) != pulledSpec {
		t.Errorf("file = %q", b)
	}
}

func TestSpecPullNoSpec(t *testing.T) {
	for _, extra := range [][]string{nil, {"--json"}} {
		var got specRequest
		srv := pullServer(t, 200, noSpecBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		out := filepath.Join(t.TempDir(), "spec.md")
		args := append([]string{"spec", "pull", "wish-reminders/2", "-o", out}, extra...)

		if code := cli.Run(args, h.deps); code != 1 || h.errb.String() != "wish-reminders/2 has no spec yet.\n" || h.out.String() != "" {
			t.Errorf("%q: code = %d, stdout = %q, stderr = %q", extra, code, h.out, h.errb)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("%q: a file was written: %v", extra, err)
		}
	}
}

func TestSpecPullNotFound(t *testing.T) {
	var got specRequest
	srv := pullServer(t, 404, `{"error":"not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "pull", "wish-reminders/9"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "No milestone wish-reminders/9 on " + srv.URL + ".\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestSpecPullUsage(t *testing.T) {
	var got specRequest
	srv := pullServer(t, 200, pulledBody, &got)
	file := writeSpec(t, "x")
	cases := [][]string{
		{"spec", "pull"},
		{"spec", "pull", "wish-reminders"},
		{"spec", "pull", "wish-reminders/0"},
		{"spec", "pull", "Wish/1"},
		{"spec", "pull", "wish-reminders/1", "extra"},
		{"spec", "pull", "wish-reminders/1", "-o"},
		{"spec", "pull", "wish-reminders/1", "-o="},
		{"spec", "pull", "wish-reminders/1", "-o", ""},
		{"spec", "pull", "wish-reminders/1", "-o", "-x"},
		{"spec", "pull", "wish-reminders/1", "-o=-x"},
		{"spec", "pull", "wish-reminders/1", "-o", "a.md", "-o", "b.md"},
		{"spec", "pull", "wish-reminders/1", file},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageSpecPull {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.method != "" {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestSpecPullNotSignedIn(t *testing.T) {
	var got specRequest
	srv := pullServer(t, 200, pulledBody, &got)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"spec", "pull", "wish-reminders/2"}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "Not signed in. Run cadre login --url <url>.") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestSpecPushWithBase(t *testing.T) {
	for _, flag := range [][]string{{"--base", "v1"}, {"--base=v1"}} {
		var got specRequest
		srv := specServer(t, 200, pushedBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		args := append([]string{"spec", "push", "wish-reminders/2", writeSpec(t, "x")}, flag...)

		if code := cli.Run(args, h.deps); code != 0 {
			t.Fatalf("%q: code = %d, stderr %q", flag, code, h.errb)
		}
		if len(got.body) != 2 || got.body["spec"] != "x" || got.body["base_version"] != "v1" {
			t.Errorf("%q: body = %q", flag, got.body)
		}
	}
}

func TestSpecPushChanged(t *testing.T) {
	var got specRequest
	srv := specServer(t, 409, specChangedJ, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"spec", "push", "wish-reminders/2", writeSpec(t, "x"), "--base", "v1"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "The spec of wish-reminders/2 changed since you pulled it. Pull it again.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

type projectRequest struct {
	method, path, auth, body string
	calls                    int
}

func projectCallServer(t *testing.T, status int, body string, got *projectRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = projectRequest{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization"), body: string(b), calls: got.calls + 1}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	projectPulledBody = `{"slug":"wish-reminders","name":"Wish reminders","end_goal":"Remind <everyone> & more","home_repo":"colette/wish","canvas_url":null,"version":4,"archived":false,"url":"https://cadre.example/projects/wish-reminders","milestones":[{"number":1,"title":"Schema","repos":["colette/wish","colette/api"],"design_url":"https://example.com/d?a=1&b=2"},{"number":3,"title":"UI","repos":[],"design_url":null}]}`
	projectFileText   = `{
  "slug": "wish-reminders",
  "name": "Wish reminders",
  "end_goal": "Remind <everyone> & more",
  "home_repo": "colette/wish",
  "canvas_url": null,
  "version": 4,
  "milestones": [
    {
      "number": 1,
      "title": "Schema",
      "repos": [
        "colette/wish",
        "colette/api"
      ],
      "design_url": "https://example.com/d?a=1&b=2"
    },
    {
      "number": 3,
      "title": "UI",
      "repos": [],
      "design_url": null
    }
  ]
}
`
	projectUpdatedBody = `{"slug":"wish-reminders","version":5,"url":"https://cadre.example/projects/wish-reminders"}`
)

func TestProjectPullToStdout(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 200, projectPulledBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "pull", "wish-reminders"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.out.String() != projectFileText || h.errb.String() != "" {
		t.Errorf("stdout = %q, stderr = %q", h.out, h.errb)
	}
	if got.method != "GET" || got.path != "/api/projects/wish-reminders" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
}

func TestProjectPullToFile(t *testing.T) {
	for _, flag := range [][]string{{"-o", "OUT"}, {"-o=OUT"}} {
		var got projectRequest
		srv := projectCallServer(t, 200, projectPulledBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		out := filepath.Join(t.TempDir(), "p.json")
		args := []string{"project", "pull", "wish-reminders"}
		for _, f := range flag {
			args = append(args, strings.ReplaceAll(f, "OUT", out))
		}

		if code := cli.Run(args, h.deps); code != 0 {
			t.Fatalf("%q: code = %d, stderr %q", flag, code, h.errb)
		}
		if b, err := os.ReadFile(out); err != nil || string(b) != projectFileText {
			t.Errorf("%q: file = %q, %v", flag, b, err)
		}
		assertMode(t, out, 0o644)
		if want := "Pulled wish-reminders (version 4) to " + out + ".\n"; h.out.String() != want {
			t.Errorf("%q: stdout = %q, want %q", flag, h.out, want)
		}
	}
}

func TestProjectPullReplacesAFile(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 200, projectPulledBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	out := writeSpec(t, "an older, longer text that must not survive")

	if code := cli.Run([]string{"project", "pull", "wish-reminders", "-o", out}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if b, _ := os.ReadFile(out); string(b) != projectFileText {
		t.Errorf("file = %q", b)
	}
}

func TestProjectPullJSON(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 200, projectPulledBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	out := filepath.Join(t.TempDir(), "p.json")

	if code := cli.Run([]string{"project", "pull", "wish-reminders", "--json", "-o", out}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.out.String() != projectPulledBody {
		t.Errorf("stdout = %q", h.out)
	}
	if want := "Pulled wish-reminders (version 4) to " + out + ".\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
	if b, _ := os.ReadFile(out); string(b) != projectFileText {
		t.Errorf("file = %q", b)
	}
}

func TestProjectPullNotFound(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 404, `{"error":"not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "pull", "nope"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "No project nope on " + srv.URL + ".\n"; h.errb.String() != want || h.out.String() != "" {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestProjectUpdate(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 200, projectUpdatedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	file := writeSpec(t, projectFileText)

	if code := cli.Run([]string{"project", "update", file}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if got.method != "PUT" || got.path != "/api/projects/wish-reminders" || got.auth != "Bearer cadre_abc" || got.body != projectFileText {
		t.Errorf("request = %+v", got)
	}
	if want := "Updated wish-reminders to version 5: https://cadre.example/projects/wish-reminders\n"; h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}

	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"project", "update", file, "--json"}, h.deps); code != 0 || h.out.String() != projectUpdatedBody {
		t.Errorf("--json: code = %d, stdout = %q", code, h.out)
	}
}

func TestProjectUpdateChanged(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 409, `{"error":"project_changed","version":5}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "update", writeSpec(t, projectFileText)}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "wish-reminders changed since you pulled it (version 4, now 5). Pull it again.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestProjectUpdateArchived(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 409, `{"error":"project_archived"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "update", writeSpec(t, projectFileText)}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "wish-reminders is archived. Run cadre project unarchive wish-reminders first.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestProjectUpdateNotFound(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 404, `{"error":"not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "update", writeSpec(t, projectFileText)}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "No project wish-reminders on " + srv.URL + ".\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestProjectUpdateNoSlug(t *testing.T) {
	for _, content := range []string{`{"name":"x","version":1}`, `{"slug":"","version":1}`} {
		var got projectRequest
		srv := projectCallServer(t, 200, projectUpdatedBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		file := writeSpec(t, content)

		if code := cli.Run([]string{"project", "update", file}, h.deps); code != 1 {
			t.Fatalf("code = %d", code)
		}
		if want := file + " has no slug.\n"; h.errb.String() != want {
			t.Errorf("stderr = %q, want %q", h.errb, want)
		}
		if got.calls != 0 {
			t.Errorf("a request was made: %+v", got)
		}
	}
}

func TestProjectUpdateUnreadableFile(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 200, projectUpdatedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "update", filepath.Join(t.TempDir(), "missing.json")}, h.deps); code != 1 || got.calls != 0 {
		t.Errorf("code = %d, calls = %d", code, got.calls)
	}
	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	file := writeSpec(t, "not json")
	if code := cli.Run([]string{"project", "update", file}, h.deps); code != 1 || got.calls != 0 || h.errb.String() != file+" is not valid JSON\n" {
		t.Errorf("code = %d, calls = %d, stderr %q", code, got.calls, h.errb)
	}
}

func TestProjectUpdateInvalid(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 422, `{"errors":[{"path":"milestones.0.number","message":"is not a milestone of this project"}]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "update", writeSpec(t, projectFileText)}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "milestones.0.number: is not a milestone of this project\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestProjectArchive(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 200, `{"slug":"gift-wrapping","archived":true,"url":"https://cadre.example/projects/gift-wrapping"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "archive", "gift-wrapping"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if got.method != "POST" || got.path != "/api/projects/gift-wrapping/archive" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
	if want := "Archived gift-wrapping. Run cadre project unarchive gift-wrapping to bring it back.\n"; h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
}

func TestProjectUnarchive(t *testing.T) {
	var got projectRequest
	body := `{"slug":"gift-wrapping","archived":false,"url":"https://cadre.example/projects/gift-wrapping"}`
	srv := projectCallServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "unarchive", "gift-wrapping"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if got.method != "POST" || got.path != "/api/projects/gift-wrapping/unarchive" {
		t.Errorf("request = %+v", got)
	}
	if want := "Unarchived gift-wrapping: https://cadre.example/projects/gift-wrapping\n"; h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}

	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"project", "unarchive", "gift-wrapping", "--json"}, h.deps); code != 0 || h.out.String() != body {
		t.Errorf("--json: code = %d, stdout = %q", code, h.out)
	}
}

func TestProjectArchiveNotFound(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 404, `{"error":"not_found"}`, &got)
	for _, verb := range []string{"archive", "unarchive"} {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run([]string{"project", verb, "nope"}, h.deps); code != 1 {
			t.Fatalf("%s: code = %d", verb, code)
		}
		if want := "No project nope on " + srv.URL + ".\n"; h.errb.String() != want {
			t.Errorf("%s: stderr = %q, want %q", verb, h.errb, want)
		}
	}
}

func TestProjectUsage(t *testing.T) {
	var got projectRequest
	srv := projectCallServer(t, 200, projectPulledBody, &got)
	file := writeSpec(t, projectFileText)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"project", "pull"}, usageProjectPull},
		{[]string{"project", "pull", "-o", "p.json"}, usageProjectPull},
		{[]string{"project", "pull", "wish-reminders", "-o"}, usageProjectPull},
		{[]string{"project", "pull", "wish-reminders", "-o="}, usageProjectPull},
		{[]string{"project", "pull", "wish-reminders", "extra"}, usageProjectPull},
		{[]string{"project", "update"}, usageProjectUpdate},
		{[]string{"project", "update", file, file}, usageProjectUpdate},
		{[]string{"project", "archive"}, usageProjectArchive},
		{[]string{"project", "archive", "a", "b"}, usageProjectArchive},
		{[]string{"project", "unarchive"}, usageProjectUnarchive},
		{[]string{"project", "unarchive", "a", "b"}, usageProjectUnarchive},
	}
	for _, c := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(c.args, h.deps); code != 2 || h.errb.String() != c.want {
			t.Errorf("%q: code = %d, stderr %q", c.args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

type linkRequest struct {
	calls              int
	method, path, auth string
	query              string
	body               map[string]any
}

func linkServer(t *testing.T, status int, body string, got *linkRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.calls++
		got.method, got.path, got.auth, got.query = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.URL.RawQuery
		if err := json.Unmarshal(b, &got.body); len(b) > 0 && err != nil {
			t.Errorf("body %q: %v", b, err)
		}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const prURL = "https://github.com/colette-club/colette-api/pull/418"

const linkedBody = `{"repo":"colette-api","number":418,"url":"https://github.com/colette-club/colette-api/pull/418","milestone":"wish-reminders/2","project_url":"https://cadre.example/projects/wish-reminders"}`

const unlinkedBody = `{"repo":"colette-api","number":418,"url":"https://github.com/colette-club/colette-api/pull/418","milestone":null,"project_url":null}`

func TestPrLink(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, linkedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"pr", "link", prURL, "wish-reminders/2"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Linked colette-api #418 to wish-reminders/2: https://cadre.example/projects/wish-reminders\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "POST" || got.path != "/api/pull-requests/link" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
	if len(got.body) != 2 || got.body["url"] != prURL || got.body["milestone"] != "wish-reminders/2" {
		t.Errorf("body = %v", got.body)
	}
}

const linkedAppBody = `{"repo":"match-day","number":37,"url":"https://github.com/acme/match-day/pull/37","milestone":"live-scores/2","app":"api","project_url":"https://cadre.example/projects/live-scores"}`

func TestPrLinkApp(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, linkedAppBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"pr", "link", prURL, "live-scores/2", "--app", "api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Linked match-day #37 to live-scores/2 api: https://cadre.example/projects/live-scores\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if len(got.body) != 3 || got.body["url"] != prURL || got.body["milestone"] != "live-scores/2" || got.body["app"] != "api" {
		t.Errorf("body = %v", got.body)
	}
}

func TestPrLinkAppJSON(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, linkedAppBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"pr", "link", prURL, "live-scores/2", "--app", "api", "--json"}, h.deps); code != 0 || h.out.String() != linkedAppBody {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}
	if got.body["app"] != "api" {
		t.Errorf("body = %v", got.body)
	}
}

func TestPrLinkAppIsAUsageError(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, linkedAppBody, &got)
	cases := [][]string{
		{"pr", "link", "--app", "api"},
		{"pr", "link", prURL, "--app", "api", "--none"},
		{"pr", "link", prURL, "live-scores/2", "--app"},
		{"pr", "link", prURL, "live-scores/2", "--app="},
		{"pr", "link", prURL, "live-scores/2", "--app", "a", "--app", "b"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usagePrLink {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestPrLinkAppRefused(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 422, `{"errors":[{"path":"app","message":"must be lowercase letters, digits, - or _"}]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"pr", "link", prURL, "live-scores/2", "--app", "Web"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "app: must be lowercase letters, digits, - or _\n"; h.errb.String() != want {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestPrLinkNone(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, unlinkedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"pr", "link", "--none", prURL}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Unlinked colette-api #418.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	milestone, sent := got.body["milestone"]
	if len(got.body) != 2 || got.body["url"] != prURL || !sent || milestone != nil {
		t.Errorf("body = %v", got.body)
	}
}

func TestPrLinkJSON(t *testing.T) {
	for _, tc := range []struct {
		args []string
		body string
	}{
		{[]string{"pr", "link", prURL, "wish-reminders/2", "--json"}, linkedBody},
		{[]string{"pr", "link", prURL, "--none", "--json"}, unlinkedBody},
	} {
		var got linkRequest
		srv := linkServer(t, 200, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(tc.args, h.deps); code != 0 || h.out.String() != tc.body {
			t.Errorf("%q: code = %d, stdout = %q, stderr %q", tc.args, code, h.out, h.errb)
		}
	}
}

func TestPrLinkErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   func(server string) string
	}{
		{404, `{"error":"milestone_not_found"}`, func(server string) string { return "No milestone wish-reminders/9 on " + server + ".\n" }},
		{404, `{"error":"pull_request_not_found"}`, func(string) string { return "GitHub has no pull request at " + prURL + ".\n" }},
		{503, `{"error":"github_not_connected"}`, func(server string) string {
			return "GitHub isn't connected on " + server + " yet. An org owner sets it up on its Setup page.\n"
		}},
		{502, `{"error":"github_unreachable"}`, func(string) string { return "Cadre couldn't reach GitHub. Try again in a minute.\n" }},
		{422, `{"errors":[{"path":"url","message":"is in a repo no project names"}]}`, func(string) string { return "url: is in a repo no project names\n" }},
		{401, `{"error":"unauthorized"}`, func(server string) string {
			return "Your key for " + server + " is no longer valid. Run cadre login --url " + server + ".\n"
		}},
	}
	for _, tc := range cases {
		var got linkRequest
		srv := linkServer(t, tc.status, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"pr", "link", prURL, "wish-reminders/9"}, h.deps); code != 1 {
			t.Errorf("%d %s: code = %d", tc.status, tc.body, code)
		}
		if want := tc.want(srv.URL); h.errb.String() != want {
			t.Errorf("%d %s: stderr = %q, want %q", tc.status, tc.body, h.errb, want)
		}
	}
}

func TestPrLinkUsage(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, linkedBody, &got)
	cases := [][]string{
		{"pr", "link"},
		{"pr", "link", prURL},
		{"pr", "link", "--none"},
		{"pr", "link", prURL, "wish-reminders/2", "--none"},
		{"pr", "link", prURL, "nope"},
		{"pr", "link", prURL, "wish-reminders/0"},
		{"pr", "link", prURL, "wish-reminders/2", "extra"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usagePrLink {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestPrLinkNotSignedIn(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, linkedBody, &got)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"pr", "link", prURL, "wish-reminders/2"}, h.deps); code != 1 || h.errb.String() != "Not signed in. Run cadre login --url <url>.\n" {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

const repoShowBody = `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette","last_deploy":{"commit":"4370c48d1f0e9a3b5c6d7e8f90a1b2c3d4e5f607","deployed_at":"2026-11-03T14:05:00Z"}},{"platform":"render","settings":{},"tracked":false,"account":null}]}`

func TestRepoSetFly(t *testing.T) {
	var requests []linkRequest
	body := `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette"}`
	srv := severalAppsServer(t, body, flyShow("colette-api-prod"), &requests)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app colette-api-prod, in the colette org. It deploys with fly.production.toml, else fly.toml.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if len(requests) == 0 {
		t.Fatal("no request")
	}
	got := requests[0]
	settings, _ := got.body["settings"].(map[string]any)
	if got.method != "PUT" || got.path != "/api/repositories/colette-api/targets/fly" || got.auth != "Bearer cadre_abc" || len(got.body) != 1 || len(settings) != 1 || settings["app"] != "colette-api-prod" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoSetFlyConfig(t *testing.T) {
	var requests []linkRequest
	body := `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod","config":"fly.staging.toml"},"tracked":true,"account":"colette"}`
	srv := severalAppsServer(t, body, flyShow("colette-api-prod"), &requests)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	args := []string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--fly-config", "fly.staging.toml"}
	if code := cli.Run(args, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app colette-api-prod, in the colette org. It deploys with fly.staging.toml.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if len(requests) == 0 {
		t.Fatal("no request")
	}
	got := requests[0]
	settings, _ := got.body["settings"].(map[string]any)
	if got.method != "PUT" || got.path != "/api/repositories/colette-api/targets/fly" || len(got.body) != 1 || len(settings) != 2 || settings["app"] != "colette-api-prod" || settings["config"] != "fly.staging.toml" {
		t.Errorf("request = %+v", got)
	}
}

// The line reads the server's answer, not the flags: a config that didn't stick must not read as set.
func TestRepoSetSaysWhatItDeploysWith(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		body string
		want string
	}{
		{
			"a stored config, with no account",
			[]string{"--fly-app", "colette-api-prod", "--fly-config", "fly.staging.toml"},
			`{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod","config":"fly.staging.toml"},"tracked":true,"account":null}`,
			"Connected colette-api to the Fly app colette-api-prod. It deploys with fly.staging.toml.\n",
		},
		{
			"a config the answer doesn't hold",
			[]string{"--fly-app", "colette-api-prod", "--fly-config", "fly.staging.toml"},
			`{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette"}`,
			"Connected colette-api to the Fly app colette-api-prod, in the colette org. It deploys with fly.production.toml, else fly.toml.\n",
		},
		{
			"another tracked platform",
			[]string{"--platform", "heroku"},
			`{"repo":"colette-api","platform":"heroku","settings":{"app":"colette-api-prod"},"tracked":true,"account":null}`,
			"Connected colette-api to the Heroku app colette-api-prod.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got linkRequest
			h := newHarness(t, linkServer(t, 200, tc.body, &got), &config.Config{APIKey: "cadre_abc"})

			if code := cli.Run(append([]string{"repo", "set", "colette-api"}, tc.args...), h.deps); code != 0 {
				t.Fatalf("code = %d, stderr %q", code, h.errb)
			}
			if h.out.String() != tc.want {
				t.Errorf("stdout = %q, want %q", h.out, tc.want)
			}
		})
	}
}

func TestRepoSetUntrackedPlatform(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"colette-site","platform":"render","settings":{},"tracked":false}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-site", "--platform", "render"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "cadre can't read Render deploys yet, so colette-site shows as not tracked.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	settings, ok := got.body["settings"].(map[string]any)
	if got.method != "PUT" || got.path != "/api/repositories/colette-site/targets/render" || !ok || len(settings) != 0 {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoUnset(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"colette-site","platform":"render"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "colette-site", "--platform", "render"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed colette-site's Render app.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "DELETE" || got.path != "/api/repositories/colette-site/targets/render" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoUnsetDisplaysPlatformLowerCased(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 404, `{"error":"target_not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "colette-site", "--platform", "RENDER"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "colette-site has no Render target.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestRepoSetPlatformFlyPointsToItsFlag(t *testing.T) {
	for _, platform := range []string{"fly", "FLY", "Fly"} {
		var got linkRequest
		srv := linkServer(t, 200, repoShowBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"repo", "set", "colette-api", "--platform", platform}, h.deps); code != 1 {
			t.Errorf("%s: code = %d", platform, code)
		}
		if want := "Fly has its own flag: cadre repo set colette-api --fly-app <app>.\n"; h.errb.String() != want {
			t.Errorf("%s: stderr = %q, want %q", platform, h.errb, want)
		}
		if got.calls != 0 {
			t.Errorf("%s: the server was called: %+v", platform, got)
		}
	}
}

func TestRepoShowPrintsAnySettingValue(t *testing.T) {
	var got linkRequest
	body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod","port":8080},"tracked":true,"account":"colette"}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, port 8080, org colette, no deploy seen yet\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoShow(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, repoShowBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "colette-api (colette-club/colette-api)\n  Fly     app colette-api-prod, org colette, last deploy Nov 3 (4370c48)\n  Render  not tracked\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "GET" || got.path != "/api/repositories/colette-api" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoShowLastDeploy(t *testing.T) {
	for name, tc := range map[string]struct{ lastDeploy, want string }{
		"a commit":         {`,"last_deploy":{"commit":"4370c48d1f0e9a3b5c6d7e8f90a1b2c3d4e5f607","deployed_at":"2026-11-03T14:05:00Z"}`, "last deploy Nov 3 (4370c48)"},
		"no commit":        {`,"last_deploy":{"commit":null,"deployed_at":"2026-11-03T14:05:00Z"}`, "last deploy Nov 3 (commit unknown)"},
		"null":             {`,"last_deploy":null`, "no deploy seen yet"},
		"key absent":       {``, "no deploy seen yet"},
		"dates are in UTC": {`,"last_deploy":{"commit":null,"deployed_at":"2026-11-03T23:30:00-05:00"}`, "last deploy Nov 4 (commit unknown)"},
	} {
		var got linkRequest
		body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette"` + tc.lastDeploy + `}]}`
		srv := linkServer(t, 200, body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
			t.Fatalf("%s: code = %d, stderr %q", name, code, h.errb)
		}
		if want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, org colette, " + tc.want + "\n"; h.out.String() != want {
			t.Errorf("%s: stdout = %q, want %q", name, h.out, want)
		}
	}
}

func TestRepoShowSortsSettings(t *testing.T) {
	var got linkRequest
	body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"region":"cdg","app":"colette-api-prod"},"tracked":true}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, region cdg, no deploy seen yet\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoShowWithoutTargets(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "colette-api (colette-club/colette-api) has no deploy target yet.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoJSON(t *testing.T) {
	for _, tc := range []struct {
		args []string
		body string
	}{
		{[]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--json"}, `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true}`},
		{[]string{"repo", "set", "colette-site", "--platform", "render", "--json"}, `{"repo":"colette-site","platform":"render","settings":{},"tracked":false}`},
		{[]string{"repo", "unset", "colette-site", "--platform", "render", "--json"}, `{"repo":"colette-site","platform":"render"}`},
		{[]string{"repo", "show", "colette-api", "--json"}, repoShowBody},
	} {
		var got linkRequest
		srv := linkServer(t, 200, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(tc.args, h.deps); code != 0 || h.out.String() != tc.body {
			t.Errorf("%q: code = %d, stdout = %q, stderr %q", tc.args, code, h.out, h.errb)
		}
	}
}

func TestRepoErrors(t *testing.T) {
	set := []string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod"}
	unset := []string{"repo", "unset", "colette-api", "--platform", "render"}
	show := []string{"repo", "show", "colette-x"}
	cases := []struct {
		args   []string
		status int
		body   string
		want   func(server string) string
	}{
		{show, 404, `{"error":"repository_not_found"}`, func(server string) string { return "No repo colette-x on " + server + ".\n" }},
		{set, 404, `{"error":"repository_not_found"}`, func(server string) string { return "No repo colette-api on " + server + ".\n" }},
		{unset, 404, `{"error":"repository_not_found"}`, func(server string) string { return "No repo colette-api on " + server + ".\n" }},
		{unset, 404, `{"error":"target_not_found"}`, func(string) string { return "colette-api has no Render target.\n" }},
		{set, 503, `{"error":"platform_not_connected","platform":"fly"}`, func(server string) string {
			return "Fly isn't connected on " + server + " yet. Add an org with cadre fly org add <org>.\n"
		}},
		{set, 502, `{"error":"platform_unreachable","platform":"fly"}`, func(string) string { return "Cadre couldn't reach Fly. Try again in a minute.\n" }},
		{set, 422, `{"errors":[{"path":"settings.app","message":"is not an app in the org"}]}`, func(string) string { return "settings.app: is not an app in the org\n" }},
		{set, 422, `{"errors":[{"path":"settings.config","message":"must be a path inside the repo, such as fly.production.toml"}]}`, func(string) string {
			return "settings.config: must be a path inside the repo, such as fly.production.toml\n"
		}},
		{show, 401, `{"error":"unauthorized"}`, func(server string) string {
			return "Your key for " + server + " is no longer valid. Run cadre login --url " + server + ".\n"
		}},
	}
	for _, tc := range cases {
		var got linkRequest
		srv := linkServer(t, tc.status, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(tc.args, h.deps); code != 1 {
			t.Errorf("%q %d %s: code = %d", tc.args, tc.status, tc.body, code)
		}
		if want := tc.want(srv.URL); h.errb.String() != want {
			t.Errorf("%q %d %s: stderr = %q, want %q", tc.args, tc.status, tc.body, h.errb, want)
		}
	}
}

func TestRepoUsage(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, repoShowBody, &got)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"repo", "set"}, usageRepoSet},
		{[]string{"repo", "set", "--fly-app", "colette-api-prod"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--platform", "render"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "--fly-app"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "--platform", "--fly-app"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "--platform", "--fly-config", "fly.toml"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "--fly-config", "fly.toml"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "--platform", "render", "--fly-config", "fly.toml"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--fly-config"}, usageRepoSet},
		{[]string{"repo", "set", "colette-api", "other", "--fly-app", "colette-api-prod"}, usageRepoSet},
		{[]string{"repo", "unset", "colette-site"}, usageRepoUnset},
		{[]string{"repo", "unset", "--platform", "render"}, usageRepoUnset},
		{[]string{"repo", "unset", "colette-site", "--fly-app", "x"}, usageRepoUnset},
		{[]string{"repo", "show"}, usageRepoShow},
		{[]string{"repo", "show", "colette-api", "colette-site"}, usageRepoShow},
	}
	for _, c := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(c.args, h.deps); code != 2 || h.errb.String() != c.want {
			t.Errorf("%q: code = %d, stderr %q", c.args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestRepoNotSignedIn(t *testing.T) {
	for _, args := range [][]string{
		{"repo", "set", "colette-api", "--fly-app", "colette-api-prod"},
		{"repo", "unset", "colette-site", "--platform", "render"},
		{"repo", "show", "colette-api"},
	} {
		var got linkRequest
		srv := linkServer(t, 200, repoShowBody, &got)
		h := newHarness(t, srv, nil)

		if code := cli.Run(args, h.deps); code != 1 || h.errb.String() != "Not signed in. Run cadre login --url <url>.\n" {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
		if got.calls != 0 {
			t.Errorf("the server was called: %+v", got)
		}
	}
}

func TestRepoSetWithoutAccountKeepsTheShortLine(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":null}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app colette-api-prod. It deploys with fly.production.toml, else fly.toml.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

type flyRequest struct {
	method, path, auth, raw string
	calls                   int
}

func flyServer(t *testing.T, status int, body string, got *flyRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.calls++
		got.method, got.path, got.auth, got.raw = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), string(b)
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withStdin makes the harness read piped input, or a terminal answering with secret.
func (h *harness) pipe(input string) {
	h.deps.Stdin = strings.NewReader(input)
	h.deps.Terminal = func() bool { return false }
}

func (h *harness) terminal(secret string, err error, asked *int) {
	h.deps.Stdin = strings.NewReader("never read")
	h.deps.Terminal = func() bool { return true }
	h.deps.ReadSecret = func() (string, error) {
		if asked != nil {
			*asked++
		}
		return secret, err
	}
}

const addedBody = `{"platform":"fly","name":"acme","apps":4}`

func TestFlyOrgAddFromAPipe(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, addedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe("  fly_secret_token \n")

	if code := cli.Run([]string{"fly", "org", "add", "acme"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected the Fly org acme: its token reads 4 apps.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if h.errb.String() != "" {
		t.Errorf("stderr = %q", h.errb)
	}
	if got.method != "PUT" || got.path != "/api/platforms/fly/accounts/acme" || got.auth != "Bearer cadre_abc" || got.raw != `{"token":"fly_secret_token"}` {
		t.Errorf("request = %+v", got)
	}
}

func TestFlyOrgAddOneApp(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, `{"platform":"fly","name":"acme","apps":1}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe("tok")

	if code := cli.Run([]string{"fly", "org", "add", "acme"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected the Fly org acme: its token reads 1 app.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestFlyOrgAddAsksOnATerminal(t *testing.T) {
	var got flyRequest
	asked := 0
	srv := flyServer(t, 200, addedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.terminal("typed_token", nil, &asked)

	if code := cli.Run([]string{"fly", "org", "add", "acme"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.errb.String() != "Token for acme: " || asked != 1 {
		t.Errorf("stderr = %q, asked %d", h.errb, asked)
	}
	if got.raw != `{"token":"typed_token"}` {
		t.Errorf("request = %+v", got)
	}
}

func TestFlyOrgAddJSON(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, addedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe("tok")

	if code := cli.Run([]string{"fly", "org", "add", "acme", "--json"}, h.deps); code != 0 || h.out.String() != addedBody {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}
}

func TestFlyOrgAddWithoutATokenSendsNothing(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"empty pipe":     func(h *harness) { h.pipe("") },
		"blank pipe":     func(h *harness) { h.pipe(" \n\t\n") },
		"blank on a tty": func(h *harness) { h.terminal("  ", nil, nil) },
		"empty on a tty": func(h *harness) { h.terminal("", nil, nil) },
	} {
		var got flyRequest
		srv := flyServer(t, 200, addedBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		setup(h)

		if code := cli.Run([]string{"fly", "org", "add", "acme"}, h.deps); code != 1 {
			t.Errorf("%s: code = %d", name, code)
		}
		if !strings.HasSuffix(h.errb.String(), "No token given. Pipe it in, or type it when asked.\n") {
			t.Errorf("%s: stderr = %q", name, h.errb)
		}
		if got.calls != 0 {
			t.Errorf("%s: the server was called: %+v", name, got)
		}
	}
}

func TestFlyOrgAddTokenReadFailureDoesNotEchoTheToken(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, addedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.terminal("fly_secret_token", errors.New("no tty"), nil)

	if code := cli.Run([]string{"fly", "org", "add", "acme"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if strings.Contains(h.errb.String(), "fly_secret_token") || !strings.Contains(h.errb.String(), "Could not read the token: no tty") {
		t.Errorf("stderr = %q", h.errb)
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestFlyOrgRemove(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, `{"platform":"fly","name":"acme"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"fly", "org", "remove", "acme"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed the Fly org acme.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "DELETE" || got.path != "/api/platforms/fly/accounts/acme" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
}

func TestFlyOrgRemoveJSON(t *testing.T) {
	var got flyRequest
	const body = `{"platform":"fly","name":"acme"}`
	srv := flyServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"fly", "org", "remove", "acme", "--json"}, h.deps); code != 0 || h.out.String() != body {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}
}

func TestFlyOrgErrors(t *testing.T) {
	add := []string{"fly", "org", "add", "acme"}
	remove := []string{"fly", "org", "remove", "acme"}
	cases := []struct {
		args   []string
		status int
		body   string
		want   func(server string) string
	}{
		{add, 422, `{"error":"token_refused"}`, func(string) string { return "Fly refused this token for acme.\n" }},
		{add, 422, `{"error":"org_not_found"}`, func(string) string { return "Fly has no org acme that this token can read.\n" }},
		{add, 422, `{"errors":[{"path":"name","message":"is invalid"}]}`, func(string) string { return "name: is invalid\n" }},
		{add, 502, `{"error":"platform_unreachable","platform":"fly"}`, func(string) string { return "Cadre couldn't reach Fly. Try again in a minute.\n" }},
		{add, 404, `{"error":"platform_not_found"}`, func(server string) string { return "Fly isn't a platform on " + server + ".\n" }},
		{add, 401, `{"error":"unauthorized"}`, func(server string) string {
			return "Your key for " + server + " is no longer valid. Run cadre login --url " + server + ".\n"
		}},
		{remove, 404, `{"error":"account_not_found"}`, func(server string) string { return "No Fly org acme on " + server + ".\n" }},
		{remove, 409, `{"error":"account_in_use","repos":["colette-api","colette-web"]}`, func(string) string {
			return "acme still reads colette-api, colette-web. Unset or move those targets first.\n"
		}},
		{remove, 401, `{"error":"unauthorized"}`, func(server string) string {
			return "Your key for " + server + " is no longer valid. Run cadre login --url " + server + ".\n"
		}},
	}
	for _, tc := range cases {
		var got flyRequest
		srv := flyServer(t, tc.status, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.pipe("fly_secret_token")

		if code := cli.Run(tc.args, h.deps); code != 1 {
			t.Errorf("%q %d %s: code = %d", tc.args, tc.status, tc.body, code)
		}
		if want := tc.want(srv.URL); h.errb.String() != want {
			t.Errorf("%q %d %s: stderr = %q, want %q", tc.args, tc.status, tc.body, h.errb, want)
		}
		if h.out.String() != "" {
			t.Errorf("%q: stdout = %q", tc.args, h.out)
		}
	}
}

func TestFlyOrgUsage(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, addedBody, &got)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"fly", "org", "add"}, usageFlyOrgAdd},
		{[]string{"fly", "org", "remove"}, usageFlyOrgRemove},
		{[]string{"fly", "org", "add", "acme", "extra"}, usageFlyOrgAdd},
		{[]string{"fly", "org", "remove", "acme", "extra"}, usageFlyOrgRemove},
		{[]string{"fly", "org", "add", "acme", "--token", "x"}, usageFlyOrgAdd},
	}
	for _, c := range cases {
		asked := 0
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.terminal("tok", nil, &asked)
		if code := cli.Run(c.args, h.deps); code != 2 || h.errb.String() != c.want {
			t.Errorf("%q: code = %d, stderr %q", c.args, code, h.errb)
		}
		if asked != 0 {
			t.Errorf("%q: asked for a token", c.args)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestFlyOrgNotSignedIn(t *testing.T) {
	for _, args := range [][]string{{"fly", "org", "add", "acme"}, {"fly", "org", "remove", "acme"}} {
		var got flyRequest
		asked := 0
		srv := flyServer(t, 200, addedBody, &got)
		h := newHarness(t, srv, nil)
		h.terminal("tok", nil, &asked)

		if code := cli.Run(args, h.deps); code != 1 || h.errb.String() != "Not signed in. Run cadre login --url <url>.\n" {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
		if got.calls != 0 || asked != 0 {
			t.Errorf("%q: the server was called (%+v) or the token asked (%d)", args, got, asked)
		}
	}
}

func TestFlyOrgPrintsTheServersName(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, addedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe("tok")

	if code := cli.Run([]string{"fly", "org", "add", "ACME"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected the Fly org acme: its token reads 4 apps.\n"; h.out.String() != want {
		t.Errorf("add stdout = %q", h.out)
	}

	srv = flyServer(t, 200, `{"platform":"fly","name":"acme"}`, &got)
	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"fly", "org", "remove", "ACME"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed the Fly org acme.\n"; h.out.String() != want {
		t.Errorf("remove stdout = %q", h.out)
	}
}

func TestRepoSetSendsStatusURL(t *testing.T) {
	var got linkRequest
	body := `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette","status_url":"https://app.test/status"}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	args := []string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--status-url", "https://app.test/status"}
	if code := cli.Run(args, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	settings, _ := got.body["settings"].(map[string]any)
	if got.body["status_url"] != "https://app.test/status" || len(got.body) != 2 || len(settings) != 1 || settings["app"] != "colette-api-prod" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoSetWithoutStatusFlagsLeavesTheKeyOut(t *testing.T) {
	var got linkRequest
	body := `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette"}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if _, present := got.body["status_url"]; present {
		t.Errorf("body has status_url: %+v", got.body)
	}
}

func TestRepoSetStatusFlagsNeedAnApp(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, repoShowBody, &got)
	for _, args := range [][]string{
		{"repo", "set", "colette-api", "--platform", "render", "--status-url", "https://app.test/status"},
		{"repo", "set", "colette-api", "--platform", "render", "--no-status-url"},
		{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--status-url", "https://app.test/status", "--no-status-url"},
		append(slices.Clone(vercelSet), "--status-url", "https://site.test/up", "--no-status-url"),
	} {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageRepoSet {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestRepoSetVercelSendsStatusURL(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, vercelTargetBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(append(slices.Clone(vercelSet), "--status-url", "https://site.test/up"), h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if got.body["status_url"] != "https://site.test/up" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoSetNoStatusURLSendsNull(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		body string
	}{
		"fly":    {[]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--no-status-url"}, flyAppPut},
		"vercel": {append(slices.Clone(vercelSet), "--no-status-url"), vercelTargetBody},
	} {
		var got linkRequest
		srv := linkServer(t, 200, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(tc.args, h.deps); code != 0 {
			t.Fatalf("%s: code = %d, stderr %q", name, code, h.errb)
		}
		if value, present := got.body["status_url"]; !present || value != nil {
			t.Errorf("%s: status_url = %v (present %v) in %+v", name, value, present, got.body)
		}
	}
}

func TestRepoSetPrintsTheStatusURLItKept(t *testing.T) {
	var got linkRequest
	body := `{"repo":"colette-site","platform":"vercel","name":"linear","settings":{"dir":"apps/linear","project":"prj_1"},"account":"colette-club","tracked":true,"status_url":"https://www.colette.club/","live":"not_checked","not_live_since":null,"status_url_note":null}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(vercelSet, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if !strings.HasSuffix(h.out.String(), "\nStatus URL: https://www.colette.club/\n") {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoSetSaysWhyItKeptNoStatusURL(t *testing.T) {
	var got linkRequest
	body := `{"repo":"colette-site","platform":"vercel","name":"linear","settings":{"dir":"apps/linear","project":"prj_1"},"account":"colette-club","tracked":true,"status_url":null,"live":null,"not_live_since":null,"status_url_note":"mosh.colette.club answers 302, so pass --status-url"}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(vercelSet, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if !strings.HasSuffix(h.out.String(), "\nNo status URL: mosh.colette.club answers 302, so pass --status-url.\n") {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoSetPrintsNoStatusLineWithoutOne(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, vercelTargetBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(vercelSet, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if strings.Contains(h.out.String(), "tatus URL") {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoSetShowsAStatusURLRefusal(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 422, `{"errors":[{"path":"status_url","message":"must be an https URL"}]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	args := []string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--status-url", "http://app.test"}
	if code := cli.Run(args, h.deps); code != 1 || h.errb.String() != "status_url: must be an https URL\n" {
		t.Errorf("code = %d, stderr = %q", code, h.errb)
	}
}

func TestRepoShowPrintsTheStatusURL(t *testing.T) {
	for name, tc := range map[string]struct{ fields, want string }{
		"live":         {`"status_url":"https://app.test/status","live":"live","not_live_since":null`, "Status URL https://app.test/status: live"},
		"not live":     {`"status_url":"https://app.test/status","live":"not_live","not_live_since":"2026-11-05T08:14:00Z"`, "Status URL https://app.test/status: not live since Nov 5, 08:14"},
		"not checked":  {`"status_url":"https://app.test/status","live":"not_checked","not_live_since":null`, "Status URL https://app.test/status: not checked yet"},
		"no URL, live": {`"status_url":null,"live":"live","not_live_since":null`, "Status: live"},
		"no URL, down": {`"status_url":null,"live":"not_live","not_live_since":"2026-11-05T08:14:00Z"`, "Status: not live since Nov 5, 08:14"},
	} {
		var got linkRequest
		body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette",` + tc.fields + `}]}`
		srv := linkServer(t, 200, body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
			t.Fatalf("%s: code = %d, stderr %q", name, code, h.errb)
		}
		want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, org colette, no deploy seen yet\n  " + tc.want + "\n"
		if h.out.String() != want {
			t.Errorf("%s: stdout = %q, want %q", name, h.out, want)
		}
	}
}

func TestRepoShowPrintsNoStatusLineWithoutAURL(t *testing.T) {
	for name, fields := range map[string]string{
		"null live":   `,"status_url":null,"live":null,"not_live_since":null`,
		"keys absent": ``,
	} {
		var got linkRequest
		body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette"` + fields + `}]}`
		srv := linkServer(t, 200, body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
			t.Fatalf("%s: code = %d, stderr %q", name, code, h.errb)
		}
		if want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, org colette, no deploy seen yet\n"; h.out.String() != want {
			t.Errorf("%s: stdout = %q", name, h.out)
		}
	}
}

func TestRepoShowPrintsAStatusURLOnOneLine(t *testing.T) {
	var got linkRequest
	body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"account":"colette","status_url":"https://app.test/\u001b[2J\nFake: line","live":"live","not_live_since":null}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, org colette, no deploy seen yet\n  Status URL https://app.test/ [2J Fake: line: live\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
	if strings.ContainsRune(h.out.String(), '\x1b') {
		t.Errorf("stdout has an escape: %q", h.out)
	}
}

const vercelAddedBody = `{"platform":"vercel","name":"colette-club","apps":5}`

func TestVercelTeamAdd(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, vercelAddedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe("  vercel_secret_token \n")

	if code := cli.Run([]string{"vercel", "team", "add", "colette-club"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected the Vercel team colette-club: its token reads 5 apps.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "PUT" || got.path != "/api/platforms/vercel/accounts/colette-club" || got.auth != "Bearer cadre_abc" || got.raw != `{"token":"vercel_secret_token"}` {
		t.Errorf("request = %+v", got)
	}
}

func TestVercelTeamAddAsksOnATerminal(t *testing.T) {
	var got flyRequest
	asked := 0
	srv := flyServer(t, 200, vercelAddedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.terminal("typed_token", nil, &asked)

	if code := cli.Run([]string{"vercel", "team", "add", "colette-club"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.errb.String() != "Token for colette-club: " || asked != 1 || got.raw != `{"token":"typed_token"}` {
		t.Errorf("stderr = %q, asked %d, request %+v", h.errb, asked, got)
	}
}

func TestVercelTeamAddJSON(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, vercelAddedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe("tok")

	if code := cli.Run([]string{"vercel", "team", "add", "colette-club", "--json"}, h.deps); code != 0 || h.out.String() != vercelAddedBody {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}
}

func TestVercelTeamAddWithoutATokenSendsNothing(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, vercelAddedBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(" \n")

	if code := cli.Run([]string{"vercel", "team", "add", "colette-club"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if h.errb.String() != "No token given. Pipe it in, or type it when asked.\n" || got.calls != 0 {
		t.Errorf("stderr = %q, request %+v", h.errb, got)
	}
}

func TestVercelTeamRemove(t *testing.T) {
	var got flyRequest
	const body = `{"platform":"vercel","name":"acme"}`
	srv := flyServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"vercel", "team", "remove", "acme"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed the Vercel team acme.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "DELETE" || got.path != "/api/platforms/vercel/accounts/acme" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"vercel", "team", "remove", "acme", "--json"}, h.deps); code != 0 || h.out.String() != body {
		t.Errorf("json: code = %d, stdout = %q", code, h.out)
	}
}

func TestVercelTeamErrors(t *testing.T) {
	add := []string{"vercel", "team", "add", "acme"}
	remove := []string{"vercel", "team", "remove", "acme"}
	cases := []struct {
		args   []string
		status int
		body   string
		want   func(server string) string
	}{
		{add, 422, `{"error":"token_refused"}`, func(string) string { return "Vercel refused this token for acme.\n" }},
		{add, 422, `{"error":"org_not_found"}`, func(string) string { return "Vercel has no team acme that this token can read.\n" }},
		{add, 502, `{"error":"platform_unreachable","platform":"vercel"}`, func(string) string { return "Cadre couldn't reach Vercel. Try again in a minute.\n" }},
		{add, 404, `{"error":"platform_not_found"}`, func(server string) string { return "Vercel isn't a platform on " + server + ".\n" }},
		{remove, 404, `{"error":"account_not_found"}`, func(server string) string { return "No Vercel team acme on " + server + ".\n" }},
		{remove, 409, `{"error":"account_in_use","repos":["colette-agents"]}`, func(string) string {
			return "acme still reads colette-agents. Unset or move those targets first.\n"
		}},
	}
	for _, tc := range cases {
		var got flyRequest
		srv := flyServer(t, tc.status, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.pipe("vercel_secret_token")

		if code := cli.Run(tc.args, h.deps); code != 1 {
			t.Errorf("%q %d %s: code = %d", tc.args, tc.status, tc.body, code)
		}
		if want := tc.want(srv.URL); h.errb.String() != want {
			t.Errorf("%q %d %s: stderr = %q, want %q", tc.args, tc.status, tc.body, h.errb, want)
		}
	}
}

func TestVercelTeamUsage(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, vercelAddedBody, &got)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"vercel", "team", "add"}, usageVercelTeamAdd},
		{[]string{"vercel", "team", "add", "acme", "extra"}, usageVercelTeamAdd},
		{[]string{"vercel", "team", "remove"}, usageVercelTeamRemove},
		{[]string{"vercel", "team", "remove", "acme", "extra"}, usageVercelTeamRemove},
	} {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.pipe("tok")
		if code := cli.Run(c.args, h.deps); code != 2 || h.errb.String() != c.want {
			t.Errorf("%q: code = %d, stderr %q", c.args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

const vercelTargetBody = `{"repo":"colette-agents","platform":"vercel","name":"linear","settings":{"dir":"apps/linear","project":"prj_1"},"account":"colette-club","tracked":true,"status_url":null,"live":null,"not_live_since":null}`

var vercelSet = []string{"repo", "set", "colette-agents", "--vercel-project", "prj_1", "--name", "linear", "--vercel-dir", "apps/linear"}

func TestRepoSetVercelAtTheRepoRoot(t *testing.T) {
	body := strings.Replace(vercelTargetBody, `"dir":"apps/linear"`, `"dir":"."`, 1)
	var got linkRequest
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	args := []string{"repo", "set", "colette-agents", "--vercel-project", "prj_1", "--name", "linear", "--vercel-dir", "."}
	if code := cli.Run(args, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-agents to the Vercel project prj_1 as linear, in the colette-club team. It deploys from the repo's root.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoSetVercel(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, vercelTargetBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(vercelSet, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-agents to the Vercel project prj_1 as linear, in the colette-club team. It deploys from apps/linear.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	settings, _ := got.body["settings"].(map[string]any)
	if got.method != "PUT" || got.path != "/api/repositories/colette-agents/targets/vercel" || got.auth != "Bearer cadre_abc" ||
		len(got.body) != 2 || got.body["name"] != "linear" || len(settings) != 2 || settings["project"] != "prj_1" || settings["dir"] != "apps/linear" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoSetVercelWithoutAnAccount(t *testing.T) {
	var got linkRequest
	body := `{"repo":"colette-agents","platform":"vercel","name":"linear","settings":{"dir":"apps/linear","project":"prj_1"},"account":null,"tracked":true}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(vercelSet, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-agents to the Vercel project prj_1 as linear. It deploys from apps/linear.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoSetVercelJSON(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, vercelTargetBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(append(slices.Clone(vercelSet), "--json"), h.deps); code != 0 || h.out.String() != vercelTargetBody {
		t.Errorf("code = %d, stdout = %q", code, h.out)
	}
}

func TestRepoSetVercelUsage(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, vercelTargetBody, &got)
	cases := [][]string{
		{"repo", "set", "x", "--vercel-project", "prj_1", "--vercel-dir", "apps/linear"},
		{"repo", "set", "x", "--vercel-project", "prj_1", "--name", "linear"},
		{"repo", "set", "x", "--vercel-project", "prj_1"},
		{"repo", "set", "x", "--name", "linear"},
		{"repo", "set", "x", "--vercel-dir", "apps/linear"},
		{"repo", "set", "x", "--name", "linear", "--vercel-dir", "apps/linear"},
		{"repo", "set", "x", "--vercel-project"},
		{"repo", "set", "x", "--platform", "render", "--name", "linear"},
		{"repo", "set", "x", "--platform", "render", "--vercel-dir", "apps/linear"},
		append(slices.Clone(vercelSet), "--fly-app", "x"),
		append(slices.Clone(vercelSet), "--platform", "render"),
		append(slices.Clone(vercelSet), "--fly-config", "fly.toml"),
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageRepoSet {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestRepoSetPlatformVercelPointsToItsFlags(t *testing.T) {
	for _, platform := range []string{"vercel", "Vercel", "VERCEL"} {
		var got linkRequest
		srv := linkServer(t, 200, vercelTargetBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"repo", "set", "x", "--platform", platform}, h.deps); code != 1 {
			t.Errorf("%s: code = %d", platform, code)
		}
		if want := "Vercel has its own flags: cadre repo set x --vercel-project <id> --name <app> --vercel-dir <folder>.\n"; h.errb.String() != want {
			t.Errorf("%s: stderr = %q", platform, h.errb)
		}
		if got.calls != 0 {
			t.Errorf("%s: the server was called: %+v", platform, got)
		}
	}
}

func TestRepoSetVercelNotConnected(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 503, `{"error":"platform_not_connected","platform":"vercel"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(vercelSet, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "Vercel isn't connected on " + srv.URL + " yet. Add a team with cadre vercel team add <team>.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestRepoUnsetVercel(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"colette-agents","platform":"vercel"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "colette-agents", "--platform", "vercel", "--name", "linear"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed colette-agents's Vercel app linear.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "DELETE" || got.path != "/api/repositories/colette-agents/targets/vercel" || got.query != "name=linear" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoUnsetVercelNotFound(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 404, `{"error":"target_not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "colette-agents", "--platform", "vercel", "--name", "linear"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "colette-agents has no Vercel target linear.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestRepoUnsetWithoutANameKeepsItsLine(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"colette-site","platform":"render"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "colette-site", "--platform", "render"}, h.deps); code != 0 || got.query != "" {
		t.Errorf("code = %d, request %+v", code, got)
	}
}

func TestRepoShowVercelTargets(t *testing.T) {
	var got linkRequest
	body := `{"name":"colette-agents","github_full_name":"colette-club/colette-agents","targets":[` +
		`{"platform":"fly","name":null,"settings":{"app":"colette-agents-prod"},"tracked":true,"account":"colette"},` +
		`{"platform":"vercel","name":"linear","settings":{"project":"prj_1","dir":"apps/linear"},"tracked":true,"account":"colette-club","last_deploy":{"commit":"0d97f55aa11bb22cc33dd44ee55ff66aa77bb88c","deployed_at":"2026-11-05T09:00:00Z"}},` +
		`{"platform":"vercel","name":"docs","settings":{"project":"prj_2","dir":"apps/docs"},"tracked":true,"account":"colette-club","last_deploy":null}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-agents"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "colette-agents (colette-club/colette-agents)\n" +
		"  Fly     app colette-agents-prod, org colette, no deploy seen yet\n" +
		"  Vercel  linear, dir apps/linear, project prj_1, team colette-club, last deploy Nov 5 (0d97f55)\n" +
		"  Vercel  docs, dir apps/docs, project prj_2, team colette-club, no deploy seen yet\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant %q", h.out, want)
	}
}

func TestRepoPrintsNoEscapesFromAVercelTarget(t *testing.T) {
	const evil = `li\u001b[2Jnear\nFake: line`
	set := `{"repo":"colette-agents","platform":"vercel","name":"` + evil + `","settings":{"dir":"` + evil + `","project":"` + evil + `"},"account":"` + evil + `","tracked":true}`
	show := `{"name":"colette-agents","github_full_name":"colette-club/colette-agents","targets":[{"platform":"vercel","name":"` + evil + `","settings":{"dir":"` + evil + `","pr\u001boject":"` + evil + `"},"tracked":true,"account":"` + evil + `"}]}`
	for name, tc := range map[string]struct {
		args []string
		body string
	}{"set": {vercelSet, set}, "show": {[]string{"repo", "show", "colette-agents"}, show}} {
		var got linkRequest
		srv := linkServer(t, 200, tc.body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(tc.args, h.deps); code != 0 {
			t.Fatalf("%s: code = %d, stderr %q", name, code, h.errb)
		}
		if strings.ContainsRune(h.out.String(), '\x1b') || strings.Contains(h.out.String(), "\nFake") {
			t.Errorf("%s: stdout has a control character: %q", name, h.out)
		}
	}
}

func TestRepoSetVercelWithoutDirOrProjectPrintsNoNil(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"colette-agents","platform":"vercel","name":"linear","settings":{},"account":null,"tracked":true}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run(vercelSet, h.deps); code != 0 || strings.Contains(h.out.String(), "nil") || strings.Contains(h.out.String(), "deploys from") {
		t.Errorf("code = %d, stdout = %q", code, h.out)
	}
}

func TestRepoUnsetVercelWithoutANameIsLeftToTheServer(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"colette-agents","platform":"vercel"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "colette-agents", "--platform", "vercel"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if got.method != "DELETE" || got.path != "/api/repositories/colette-agents/targets/vercel" || got.query != "" {
		t.Errorf("request = %+v", got)
	}
}

// severalAppsServer answers a PUT with put and a GET with show, and keeps every request it got.
func severalAppsServer(t *testing.T, put, show string, got *[]linkRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := linkRequest{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization"), query: r.URL.RawQuery}
		if len(b) > 0 {
			if err := json.Unmarshal(b, &req.body); err != nil {
				t.Errorf("body %q: %v", b, err)
			}
		}
		*got = append(*got, req)
		if r.Method == http.MethodGet {
			writeJSON(w, 200, show)
			return
		}
		writeJSON(w, 200, put)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const flyAppPut = `{"repo":"colette-api","platform":"fly","name":"other-app","settings":{"app":"other-app"},"tracked":true,"account":"colette"}`

func flyShow(names ...string) string {
	targets := make([]string, len(names))
	for i, n := range names {
		targets[i] = `{"platform":"fly","name":"` + n + `","settings":{"app":"` + n + `"},"tracked":true,"account":"colette"}`
	}
	return `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[` + strings.Join(targets, ",") + `]}`
}

// The line confirms the app's name when it isn't its Fly app's, as the Vercel line does.
func TestRepoSetNamesAFlyApp(t *testing.T) {
	var got []linkRequest
	put := `{"repo":"colette-api","platform":"fly","name":"api","settings":{"app":"other-app"},"tracked":true,"account":"colette"}`
	srv := severalAppsServer(t, put, flyShow("api"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app", "--name", "api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app other-app as api, in the colette org. It deploys with fly.production.toml, else fly.toml.\n"; h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
	if len(got) != 1 || got[0].method != "PUT" || got[0].path != "/api/repositories/colette-api/targets/fly" || got[0].body["name"] != "api" {
		t.Errorf("requests = %+v", got)
	}
}

func TestRepoSetPrintsAFlyAppsNameOnOneLine(t *testing.T) {
	var got []linkRequest
	put := `{"repo":"colette-api","platform":"fly","name":"a\u001b[2Jpi\nFake: line","settings":{"app":"other-app"},"tracked":true,"account":"colette"}`
	srv := severalAppsServer(t, put, flyShow("api"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app", "--name", "api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app other-app as a [2Jpi Fake: line, in the colette org. It deploys with fly.production.toml, else fly.toml.\n"; h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
}

// The server answers the same whether the set added other-app or only changed it, so this also covers a set that only
// changed an app of a repo with several (Ruling 25).
func TestRepoSetSaysItAddedAnApp(t *testing.T) {
	var got []linkRequest
	srv := severalAppsServer(t, flyAppPut, flyShow("other-app", "colette-api-prod"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Connected colette-api to the Fly app other-app, in the colette org. It deploys with fly.production.toml, else fly.toml.\n" +
		"colette-api now has 2 apps: colette-api-prod, other-app. To move an app instead, pass --name <app>.\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
	if len(got) != 2 || got[1].method != "GET" || got[1].path != "/api/repositories/colette-api" {
		t.Errorf("requests = %+v", got)
	}
}

func TestRepoSetSaysNothingForTheFirstApp(t *testing.T) {
	var got []linkRequest
	srv := severalAppsServer(t, flyAppPut, flyShow("other-app"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app other-app, in the colette org. It deploys with fly.production.toml, else fly.toml.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoSetNamedAppReadsNothingBack(t *testing.T) {
	var got []linkRequest
	srv := severalAppsServer(t, flyAppPut, flyShow("a", "b"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app", "--name", "b"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if strings.Contains(h.out.String(), "now has") || len(got) != 1 {
		t.Errorf("stdout = %q, requests = %+v", h.out, got)
	}
}

func TestRepoSetKeepsItsSuccessWhenTheFollowUpReadFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, 500, `oops`)
			return
		}
		writeJSON(w, 200, flyAppPut)
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app other-app, in the colette org. It deploys with fly.production.toml, else fly.toml.\n"; h.out.String() != want || h.errb.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q", h.out, h.errb)
	}
}

func TestRepoSetAddsNothingToItsJSON(t *testing.T) {
	var got []linkRequest
	srv := severalAppsServer(t, flyAppPut, flyShow("other-app", "colette-api-prod"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app", "--json"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.out.String() != flyAppPut || len(got) != 1 {
		t.Errorf("stdout = %q, requests = %+v", h.out, got)
	}
}

func TestRepoUnsetByNameAlone(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"kinfield","platform":"fly"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "kinfield", "--name", "kinfield-family"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed kinfield's Fly app kinfield-family.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "DELETE" || got.path != "/api/repositories/kinfield/apps/kinfield-family" || got.query != "" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoUnsetNeedsANameOrAPlatform(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "kinfield"}, h.deps); code != 2 || h.errb.String() != usageRepoUnset {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestRepoUnsetWithNoSuchApp(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 404, `{"error":"target_not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "unset", "kinfield", "--name", "linear"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "kinfield has no app linear.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

// Unsetting by name alone answers every other failure as any repo call does, and --json prints the answer as it came.
func TestRepoUnsetByNameAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		json   bool
		code   int
		stdout string
		stderr func(server string) string
	}{
		{"an unknown repo", 404, `{"error":"repository_not_found"}`, false, 1, "",
			func(server string) string { return "No repo kinfield on " + server + ".\n" }},
		{"a key no longer valid", 401, `{"error":"unauthorized"}`, false, 1, "",
			func(server string) string {
				return "Your key for " + server + " is no longer valid. Run cadre login --url " + server + ".\n"
			}},
		{"--json", 200, `{"repo":"kinfield","platform":"fly"}`, true, 0, `{"repo":"kinfield","platform":"fly"}`,
			func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got linkRequest
			srv := linkServer(t, tc.status, tc.body, &got)
			h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
			args := []string{"repo", "unset", "kinfield", "--name", "x"}
			if tc.json {
				args = append(args, "--json")
			}

			if code := cli.Run(args, h.deps); code != tc.code {
				t.Errorf("code = %d, want %d", code, tc.code)
			}
			if h.out.String() != tc.stdout {
				t.Errorf("stdout = %q, want %q", h.out, tc.stdout)
			}
			if want := tc.stderr(srv.URL); h.errb.String() != want {
				t.Errorf("stderr = %q, want %q", h.errb, want)
			}
			if got.method != "DELETE" || got.path != "/api/repositories/kinfield/apps/x" {
				t.Errorf("request = %+v", got)
			}
		})
	}
}

func TestRepoShowNamesAnAppWhoseNameDiffers(t *testing.T) {
	var got linkRequest
	body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[` +
		`{"platform":"fly","name":"api","settings":{"app":"colette-api-prod"},"tracked":true},` +
		`{"platform":"fly","name":"other-app","settings":{"app":"other-app"},"tracked":true}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "colette-api (colette-club/colette-api)\n" +
		"  Fly  api, app colette-api-prod, no deploy seen yet\n" +
		"  Fly  app other-app, no deploy seen yet\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant %q", h.out, want)
	}
}

const vercelShowTarget = `{"platform":"vercel","name":"linear","settings":{"project":"prj_1","dir":"apps/linear"},"tracked":true,"account":"colette-club"}`

func flyShowWithVercel(names ...string) string {
	show := flyShow(names...)
	return strings.Replace(show, `"targets":[`, `"targets":[`+vercelShowTarget+`,`, 1)
}

func TestRepoSetCountsEveryAppOfTheRepo(t *testing.T) {
	var got []linkRequest
	srv := severalAppsServer(t, flyAppPut, flyShowWithVercel("other-app", "colette-api-prod"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "colette-api now has 3 apps: colette-api-prod, linear, other-app. To move an app instead, pass --name <app>.\n"
	if !strings.HasSuffix(h.out.String(), "\n"+want) {
		t.Errorf("stdout = %q, want it to end with %q", h.out, want)
	}
}

func TestRepoSetSaysNothingForOneFlyAppBesideAVercelApp(t *testing.T) {
	var got []linkRequest
	srv := severalAppsServer(t, flyAppPut, flyShowWithVercel("other-app"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if strings.Contains(h.out.String(), "now has") {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestRepoSetPrintsTheStatusLinesOnOneLine(t *testing.T) {
	for name, tc := range map[string]struct {
		fields string
		want   string
	}{
		"url":  {`"status_url":"https://a\u001b[2Jpp.test/\nFake: line","status_url_note":null`, "\nStatus URL: https://a [2Jpp.test/ Fake: line\n"},
		"note": {`"status_url":null,"status_url_note":"it answers\u001b[2J 302\nFake: line"`, "\nNo status URL: it answers [2J 302 Fake: line.\n"},
	} {
		var got linkRequest
		body := `{"repo":"colette-site","platform":"vercel","name":"linear","settings":{"dir":"apps/linear","project":"prj_1"},"account":"colette-club","tracked":true,` + tc.fields + `}`
		srv := linkServer(t, 200, body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(vercelSet, h.deps); code != 0 {
			t.Fatalf("%s: code = %d, stderr %q", name, code, h.errb)
		}
		if !strings.HasSuffix(h.out.String(), tc.want) {
			t.Errorf("%s: stdout = %q, want suffix %q", name, h.out, tc.want)
		}
	}
}

func TestRepoSetPrintsTheStatusLineAfterTheSeveralAppsLine(t *testing.T) {
	var got []linkRequest
	put := `{"repo":"colette-api","platform":"fly","name":"other-app","settings":{"app":"other-app"},"tracked":true,"account":"colette","status_url":"https://app.test/up","status_url_note":null}`
	srv := severalAppsServer(t, put, flyShow("other-app", "colette-api-prod"), &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "other-app", "--status-url", "https://app.test/up"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Connected colette-api to the Fly app other-app, in the colette org. It deploys with fly.production.toml, else fly.toml.\n" +
		"colette-api now has 2 apps: colette-api-prod, other-app. To move an app instead, pass --name <app>.\n" +
		"Status URL: https://app.test/up\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
}

const (
	p8Text      = "-----BEGIN PRIVATE KEY-----\nMADEUPKEYTEXT\n-----END PRIVATE KEY-----"
	serviceJSON = `{"type":"service_account","client_email":"Cadre-Reader@colette.iam.gserviceaccount.com","private_key":"made-up"}`
)

// keyFile writes content to a temp file and returns its path.
func keyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// typePath makes the harness a terminal whose person types path when asked for it.
func (h *harness) typePath(path string, asked *int) {
	h.deps.Stdin = strings.NewReader("never read")
	h.deps.Terminal = func() bool { return true }
	h.deps.ReadPath = func() (string, error) {
		if asked != nil {
			*asked++
		}
		return path, nil
	}
	h.deps.ReadSecret = func() (string, error) { panic("a path is not read as a secret") }
}

const storeAdded = `{"platform":"app_store","name":"2X9R4HXF34","apps":3}`

func TestAppStoreKeyAddFromAPipe(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, storeAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe("  " + p8Text + "\n")

	if code := cli.Run([]string{"app-store", "key", "add", "2x9r4hxf34", "--issuer", "69a6de70-aaaa"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected the App Store Connect key 2X9R4HXF34: it reads 3 apps.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if h.errb.String() != "" {
		t.Errorf("stderr = %q", h.errb)
	}
	var body struct{ Token string }
	_ = json.Unmarshal([]byte(got.raw), &body)
	var token map[string]string
	if err := json.Unmarshal([]byte(body.Token), &token); err != nil {
		t.Fatalf("token = %q", body.Token)
	}
	if got.method != "PUT" || got.path != "/api/platforms/app_store/accounts/2X9R4HXF34" || got.auth != "Bearer cadre_abc" ||
		token["issuer_id"] != "69a6de70-aaaa" || token["private_key"] != p8Text || len(token) != 2 {
		t.Errorf("request = %+v, token %v", got, token)
	}
}

func TestAppStoreKeyAddOneApp(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, `{"platform":"app_store","name":"2X9R4HXF34","apps":1}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(p8Text)

	if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "i"}, h.deps); code != 0 || h.out.String() != "Connected the App Store Connect key 2X9R4HXF34: it reads 1 app.\n" {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}
}

func TestAppStoreKeyAddAsksForThePath(t *testing.T) {
	var got flyRequest
	asked := 0
	srv := flyServer(t, 200, storeAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.typePath(keyFile(t, p8Text+"\n"), &asked)

	if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if h.errb.String() != "Path to AuthKey_2X9R4HXF34.p8: " || asked != 1 {
		t.Errorf("stderr = %q, asked %d", h.errb, asked)
	}
	if !strings.Contains(got.raw, `MADEUPKEYTEXT`) || strings.Contains(got.raw, "never read") {
		t.Errorf("request = %+v", got)
	}
	if strings.Contains(h.out.String()+h.errb.String(), "MADEUPKEYTEXT") {
		t.Errorf("the key was printed: %q %q", h.out, h.errb)
	}
}

func TestAppStoreKeyAddExpandsAHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "AuthKey.p8"), []byte(p8Text), 0o600); err != nil {
		t.Fatal(err)
	}
	var got flyRequest
	srv := flyServer(t, 200, storeAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.typePath("~/AuthKey.p8", nil)

	if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, h.deps); code != 0 || !strings.Contains(got.raw, "MADEUPKEYTEXT") {
		t.Errorf("code = %d, stderr %q, request %+v", code, h.errb, got)
	}
}

func TestAppStoreKeyAddUnreadableFile(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, storeAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.typePath(filepath.Join(t.TempDir(), "missing.p8"), nil)

	if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), "Could not read the key file: ") || got.calls != 0 {
		t.Errorf("stderr = %q, calls %d", h.errb, got.calls)
	}
}

func TestAppStoreKeyAddWithoutAKeySendsNothing(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"empty pipe": func(h *harness) { h.pipe(" \n") },
		"empty file": func(h *harness) { h.typePath(keyFile(t, "\n"), nil) },
	} {
		var got flyRequest
		srv := flyServer(t, 200, storeAdded, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		setup(h)
		if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, h.deps); code != 1 || got.calls != 0 {
			t.Errorf("%s: code = %d, calls %d", name, code, got.calls)
		}
		if !strings.HasSuffix(h.errb.String(), "No key given. Pipe it in, or give the path of its file when asked.\n") {
			t.Errorf("%s: stderr = %q", name, h.errb)
		}
	}
}

func TestAppStoreKeyAddNeedsAnIssuer(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, storeAdded, &got)
	for _, args := range [][]string{
		{"app-store", "key", "add", "2X9R4HXF34"},
		{"app-store", "key", "add", "--issuer", "iss"},
		{"app-store", "key", "add", "2X9R4HXF34", "--issuer"},
		{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss", "extra"},
	} {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.pipe(p8Text)
		want := "Usage: cadre app-store key add <key-id> --issuer <issuer-id>\nRun cadre help app-store key add for details.\n"
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != want || got.calls != 0 {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
}

func TestAppStoreKeyAddRefused(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 422, `{"error":"token_refused"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(p8Text)

	if code := cli.Run([]string{"app-store", "key", "add", "2x9r4hxf34", "--issuer", "iss"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "Apple refused the key 2X9R4HXF34. Check its key ID, its issuer ID and its .p8 file.\n"; h.errb.String() != want || h.out.Len() != 0 {
		t.Errorf("stderr = %q, stdout %q", h.errb, h.out)
	}
}

func TestAppStoreKeyAddJSON(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, storeAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(p8Text)

	if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss", "--json"}, h.deps); code != 0 || h.out.String() != storeAdded {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}
}

func TestAppStoreKeyRemove(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, `{"platform":"app_store","name":"2X9R4HXF34"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"app-store", "key", "remove", "2x9r4hxf34"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed the App Store Connect key 2X9R4HXF34.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "DELETE" || got.path != "/api/platforms/app_store/accounts/2X9R4HXF34" {
		t.Errorf("request = %+v", got)
	}
}

func TestAppStoreKeyRemoveInUse(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 409, `{"error":"account_in_use","repos":["club-mobile","admin-mobile"]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"app-store", "key", "remove", "2X9R4HXF34"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "2X9R4HXF34 still reads club-mobile, admin-mobile. Add the key that replaces it, set each of those apps again with cadre repo set, then remove this one.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestAppStoreKeyRemoveNotFound(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 404, `{"error":"account_not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"app-store", "key", "remove", "2X9R4HXF34"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "No App Store Connect key 2X9R4HXF34 on " + srv.URL + ".\n"; h.errb.String() != want {
		t.Errorf("stderr = %q", h.errb)
	}
}

const (
	playEmail = "cadre-reader@colette.iam.gserviceaccount.com"
	playAdded = `{"platform":"google_play","name":"cadre-reader@colette.iam.gserviceaccount.com","apps":0}`
)

func TestGooglePlayAccountAdd(t *testing.T) {
	var got flyRequest
	asked := 0
	srv := flyServer(t, 200, playAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.typePath(keyFile(t, serviceJSON), &asked)

	if code := cli.Run([]string{"google-play", "account", "add"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected the Google Play account " + playEmail + ": Google accepts its key.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if h.errb.String() != "Path to the service account's JSON key: " || asked != 1 {
		t.Errorf("stderr = %q, asked %d", h.errb, asked)
	}
	var body struct{ Token string }
	_ = json.Unmarshal([]byte(got.raw), &body)
	if got.method != "PUT" || got.path != "/api/platforms/google_play/accounts/"+playEmail || body.Token != serviceJSON {
		t.Errorf("request = %+v", got)
	}
}

func TestGooglePlayAccountAddFromAPipe(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, playAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(serviceJSON + "\n")

	if code := cli.Run([]string{"google-play", "account", "add"}, h.deps); code != 0 || h.errb.Len() != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if !strings.HasSuffix(got.path, "/accounts/"+playEmail) {
		t.Errorf("path = %q", got.path)
	}
}

func TestGooglePlayAccountAddNotAKeyFile(t *testing.T) {
	for name, content := range map[string]string{
		"not json":    p8Text,
		"no email":    `{"type":"service_account"}`,
		"empty email": `{"client_email":"  "}`,
		"an array":    `[1]`,
	} {
		var got flyRequest
		srv := flyServer(t, 200, playAdded, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.pipe(content)

		if code := cli.Run([]string{"google-play", "account", "add"}, h.deps); code != 1 {
			t.Errorf("%s: code = %d", name, code)
		}
		if h.errb.String() != "That file isn't a service account's JSON key.\n" || got.calls != 0 {
			t.Errorf("%s: stderr = %q, calls %d", name, h.errb, got.calls)
		}
	}
}

func TestGooglePlayAccountAddRefused(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 422, `{"error":"token_refused"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(serviceJSON)

	if code := cli.Run([]string{"google-play", "account", "add"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := "Google refused this service account's key.\n"; h.errb.String() != want || strings.Contains(h.errb.String(), "made-up") {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestGooglePlayAccountAddJSONAndUsage(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, playAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(serviceJSON)
	if code := cli.Run([]string{"google-play", "account", "add", "--json"}, h.deps); code != 0 || h.out.String() != playAdded {
		t.Errorf("code = %d, stdout = %q, stderr %q", code, h.out, h.errb)
	}

	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	want := "Usage: cadre google-play account add\nRun cadre help google-play account add for details.\n"
	if code := cli.Run([]string{"google-play", "account", "add", playEmail}, h.deps); code != 2 || h.errb.String() != want {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestGooglePlayAccountRemove(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, `{"platform":"google_play","name":"`+playEmail+`"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"google-play", "account", "remove", playEmail}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Removed the Google Play account " + playEmail + ".\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "DELETE" || got.path != "/api/platforms/google_play/accounts/"+playEmail {
		t.Errorf("request = %+v", got)
	}
}

func TestGooglePlayAccountRemoveInUse(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 409, `{"error":"account_in_use","repos":["club-mobile"]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"google-play", "account", "remove", playEmail}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if want := playEmail + " still reads club-mobile. Add the account that replaces it, set each of those apps again with cadre repo set, then remove this one.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q", h.errb)
	}
}

const (
	appStoreStore  = `{"store":"app_store","label":"App Store","app":"6443635924","account":"2X9R4HXF34","public_build":{"version":"1.8.15","build_number":6188,"since":"2026-11-02T09:40:00Z"}}`
	googlePlayPlay = `{"store":"google_play","label":"Google Play","app":"club.colette.club","account":"cadre-reader@colette.iam.gserviceaccount.com","public_build":null}`
	mobileBoth     = `{"repo":"club-mobile","platform":"mobile","name":"club-mobile","settings":{"app_store_app":"6443635924","play_package":"club.colette.club"},"tracked":false,"account":null,"last_deploy":null,"stores":[` + appStoreStore + `,` + googlePlayPlay + `]}`
	mobileOneStore = `{"repo":"club-mobile","platform":"mobile","name":"club-mobile","settings":{"app_store_app":"6443635924"},"tracked":false,"account":null,"stores":[` + appStoreStore + `]}`
)

func TestRepoSetMobileApp(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, mobileBoth, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	args := []string{"repo", "set", "club-mobile", "--app-store-app", "6443635924", "--play-package", "club.colette.club"}
	if code := cli.Run(args, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Connected club-mobile to its stores: App Store (key 2X9R4HXF34), Google Play (cadre-reader@colette.iam.gserviceaccount.com).\n" +
		"Cadre asks each store which build is public every 15 minutes.\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
	settings, _ := got.body["settings"].(map[string]any)
	if got.method != "PUT" || got.path != "/api/repositories/club-mobile/targets/mobile" || len(got.body) != 1 ||
		len(settings) != 2 || settings["app_store_app"] != "6443635924" || settings["play_package"] != "club.colette.club" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoSetMobileAppOneStore(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, mobileOneStore, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "club-mobile", "--app-store-app", "6443635924"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Connected club-mobile to its stores: App Store (key 2X9R4HXF34).\nCadre asks each store which build is public every 15 minutes.\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
	if settings, _ := got.body["settings"].(map[string]any); len(settings) != 1 || settings["app_store_app"] != "6443635924" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoSetMobileAppWithName(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, mobileOneStore, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "club-mobile", "--play-package", "club.colette.club", "--name", "club"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	settings, _ := got.body["settings"].(map[string]any)
	if got.body["name"] != "club" || len(settings) != 1 || settings["play_package"] != "club.colette.club" {
		t.Errorf("request = %+v", got)
	}
}

func TestRepoSetMobileAppRefusesAStatusURL(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, mobileBoth, &got)
	cases := [][]string{
		{"repo", "set", "x", "--app-store-app", "1", "--status-url", "https://app.test/up"},
		{"repo", "set", "x", "--play-package", "a.b", "--no-status-url"},
		{"repo", "set", "x", "--app-store-app", "1", "--fly-app", "x"},
		{"repo", "set", "x", "--play-package", "a.b", "--vercel-project", "prj_1", "--name", "web", "--vercel-dir", "web"},
		{"repo", "set", "x", "--app-store-app", "1", "--platform", "render"},
		{"repo", "set", "x", "--app-store-app", "1", "--fly-config", "fly.toml"},
		{"repo", "set", "x", "--app-store-app", "1", "--vercel-dir", "web"},
		{"repo", "set", "x", "--app-store-app"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageRepoSet {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestRepoSetPlatformMobileHasItsOwnFlags(t *testing.T) {
	for _, platform := range []string{"mobile", "Mobile", "MOBILE"} {
		var got linkRequest
		srv := linkServer(t, 200, mobileBoth, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"repo", "set", "club-mobile", "--platform", platform}, h.deps); code != 1 {
			t.Errorf("%s: code = %d", platform, code)
		}
		if want := "A mobile app has its own flags: cadre repo set club-mobile --app-store-app <id> --play-package <name>.\n"; h.errb.String() != want {
			t.Errorf("%s: stderr = %q, want %q", platform, h.errb, want)
		}
		if got.calls != 0 {
			t.Errorf("%s: the server was called: %+v", platform, got)
		}
	}
}

func TestRepoSetMobileAppNobodyReadsIt(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 422, `{"errors":[{"path":"settings.app_store_app","message":"no App Store key reads app 6443635924"}]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "club-mobile", "--app-store-app", "6443635924"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "settings.app_store_app: no App Store key reads app 6443635924\n"; h.errb.String() != want || h.out.Len() != 0 {
		t.Errorf("stderr = %q, stdout = %q", h.errb, h.out)
	}
}

func TestRepoShowMobileApp(t *testing.T) {
	var got linkRequest
	body := `{"name":"club-mobile","github_full_name":"colette-club/club-mobile","targets":[` +
		`{"platform":"mobile","name":"club-mobile","settings":{"app_store_app":"6443635924","play_package":"club.colette.club"},"tracked":false,"account":null,"stores":[` +
		appStoreStore + `,` + googlePlayPlay + `]}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "club-mobile"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "club-mobile (colette-club/club-mobile)\n" +
		"  Mobile  club-mobile, app_store_app 6443635924, play_package club.colette.club\n" +
		"    App Store     1.8.15 (6188) public since Nov 2, 09:40 · key 2X9R4HXF34\n" +
		"    Google Play   no public build seen yet · cadre-reader@colette.iam.gserviceaccount.com\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
}

func TestRepoShowMobileAppBuildWithNoVersion(t *testing.T) {
	var got linkRequest
	store := `{"store":"google_play","label":"Google Play","app":"club.colette.club","account":"cadre-reader@colette.iam.gserviceaccount.com","public_build":{"version":null,"build_number":6185,"since":"2026-11-02T10:05:00+01:00"}}`
	body := `{"name":"club-mobile","github_full_name":"colette-club/club-mobile","targets":[{"platform":"mobile","name":"club-mobile","settings":{},"tracked":false,"account":null,"stores":[` + store + `]}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "club-mobile"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "    Google Play   6185 public since Nov 2, 09:05 · cadre-reader@colette.iam.gserviceaccount.com\n"; !strings.Contains(h.out.String(), want) {
		t.Errorf("stdout = %q, want it to hold %q", h.out, want)
	}
}

func TestRepoShowMobileAppJSON(t *testing.T) {
	var got linkRequest
	body := `{"name":"club-mobile","github_full_name":"colette-club/club-mobile","targets":[` + mobileBoth + `]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "club-mobile", "--json"}, h.deps); code != 0 || h.out.String() != body {
		t.Errorf("code = %d, stdout = %q", code, h.out)
	}
}

const (
	usageAppStoreKeyAdd          = "Usage: cadre app-store key add <key-id> --issuer <issuer-id>\nRun cadre help app-store key add for details.\n"
	usageAppStoreKeyRemove       = "Usage: cadre app-store key remove <key-id>\nRun cadre help app-store key remove for details.\n"
	usageGooglePlayAccountRemove = "Usage: cadre google-play account remove <email>\nRun cadre help google-play account remove for details.\n"
)

func TestMobileAccountsBlankArgumentIsUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		usage string
	}{
		"key add":        {[]string{"app-store", "key", "add", " ", "--issuer", "iss"}, usageAppStoreKeyAdd},
		"key remove":     {[]string{"app-store", "key", "remove", " "}, usageAppStoreKeyRemove},
		"account remove": {[]string{"google-play", "account", "remove", " "}, usageGooglePlayAccountRemove},
	} {
		t.Run(name, func(t *testing.T) {
			var got flyRequest
			srv := flyServer(t, 200, `{}`, &got)
			h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
			h.pipe(p8Text)

			if code := cli.Run(tc.args, h.deps); code != 2 {
				t.Errorf("code = %d", code)
			}
			if h.errb.String() != tc.usage || got.method != "" {
				t.Errorf("stderr = %q, request %+v", h.errb, got)
			}
		})
	}
}

func TestRepoSetMobileAppListingNoStore(t *testing.T) {
	var got linkRequest
	srv := linkServer(t, 200, `{"repo":"club-mobile","platform":"mobile","name":"club-mobile","settings":{},"tracked":false,"account":null,"stores":[]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "club-mobile", "--app-store-app", "6443635924"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Connected club-mobile to its stores.\nCadre asks each store which build is public every 15 minutes.\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
}

func TestGooglePlayAccountRemoveFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   func(server string) string
	}{
		"not found": {404, `{"error":"account_not_found"}`, func(server string) string {
			return "No Google Play account " + playEmail + " on " + server + ".\n"
		}},
		"platform unreachable": {502, `{"error":"platform_unreachable","platform":"google_play"}`, func(string) string {
			return "Cadre couldn't reach Google Play. Try again in a minute.\n"
		}},
		"signed out": {401, `{"error":"unauthorized"}`, func(server string) string {
			return "Your key for " + server + " is no longer valid. Run cadre login --url " + server + ".\n"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var got flyRequest
			srv := flyServer(t, tc.status, tc.body, &got)
			h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

			if code := cli.Run([]string{"google-play", "account", "remove", playEmail}, h.deps); code != 1 {
				t.Errorf("code = %d", code)
			}
			if want := tc.want(srv.URL); h.errb.String() != want || h.out.Len() != 0 {
				t.Errorf("stderr = %q, want %q, stdout %q", h.errb, want, h.out)
			}
		})
	}
}

func TestMobileAccountsWhenCadreCannotBeReached(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		stdin string
	}{
		"google play remove": {[]string{"google-play", "account", "remove", playEmail}, ""},
		"app store remove":   {[]string{"app-store", "key", "remove", "2X9R4HXF34"}, ""},
		"google play add":    {[]string{"google-play", "account", "add"}, serviceJSON},
		"app store add":      {[]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, p8Text},
	} {
		t.Run(name, func(t *testing.T) {
			var got flyRequest
			srv := flyServer(t, 200, `{}`, &got)
			h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
			h.pipe(tc.stdin)
			srv.Close()

			if code := cli.Run(tc.args, h.deps); code != 1 {
				t.Errorf("code = %d", code)
			}
			if !strings.HasPrefix(h.errb.String(), "Could not complete the request to "+srv.URL+": ") || h.out.Len() != 0 {
				t.Errorf("stderr = %q, stdout %q", h.errb, h.out)
			}
			if strings.Contains(h.errb.String(), "MADEUPKEYTEXT") || strings.Contains(h.errb.String(), "made-up") {
				t.Errorf("the key was printed: %q", h.errb)
			}
		})
	}
}

func TestMobileAccountsNotSignedIn(t *testing.T) {
	for name, args := range map[string][]string{
		"app store add":   {"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"},
		"google play add": {"google-play", "account", "add"},
	} {
		t.Run(name, func(t *testing.T) {
			var got flyRequest
			srv := flyServer(t, 200, storeAdded, &got)
			h := newHarness(t, srv, nil)
			asked := 0
			h.typePath(keyFile(t, p8Text), &asked)

			if code := cli.Run(args, h.deps); code != 1 {
				t.Errorf("code = %d", code)
			}
			if h.errb.String() != "Not signed in. Run cadre login --url <url>.\n" || asked != 0 || got.calls != 0 {
				t.Errorf("stderr = %q, asked %d, calls %d", h.errb, asked, got.calls)
			}
		})
	}
}

func TestMobileAccountsUnknownSubcommand(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, `{}`, &got)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"app-store", "token", "add"}, "cadre app-store has no token command.\nRun cadre help app-store to see its commands.\n"},
		{[]string{"app-store", "key", "ad", "x"}, "cadre app-store key has no ad command.\nDid you mean cadre app-store key add?\nRun cadre help app-store key to see its commands.\n"},
		{[]string{"google-play", "key", "add"}, "cadre google-play has no key command.\nRun cadre help google-play to see its commands.\n"},
		{[]string{"google-play", "account", "ad"}, "cadre google-play account has no ad command.\nDid you mean cadre google-play account add?\nRun cadre help google-play account to see its commands.\n"},
	} {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(tc.args, h.deps); code != 2 || h.errb.String() != tc.want || got.calls != 0 {
			t.Errorf("%q: code = %d, stderr %q, calls %d", tc.args, code, h.errb, got.calls)
		}
	}
}

// A key file's path that cannot be read, or none typed, sends nothing and says why.
func TestMobileAccountsKeyPathProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		read func() (string, error)
		home string
		want string
	}{
		"end of input at the prompt": {[]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"},
			func() (string, error) { return "", io.EOF }, "/h", msgNoPathText},
		"end of input at the prompt, google play": {[]string{"google-play", "account", "add"},
			func() (string, error) { return "", io.EOF }, "/h", msgNoPathText},
		"the terminal fails": {[]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"},
			func() (string, error) { return "", errors.New("no tty") }, "/h", "Could not read the key file: no tty\n"},
		"the terminal fails, google play": {[]string{"google-play", "account", "add"},
			func() (string, error) { return "", errors.New("no tty") }, "/h", "Could not read the key file: no tty\n"},
		"a home path without a home": {[]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"},
			func() (string, error) { return "~/AuthKey.p8", nil }, "", "Could not read the key file: $HOME is not defined\n"},
		"nothing typed": {[]string{"google-play", "account", "add"},
			func() (string, error) { return "  \n", nil }, "/h", "No key given. Pipe it in, or give the path of its file when asked.\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", tc.home)
			var got flyRequest
			srv := flyServer(t, 200, storeAdded, &got)
			h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
			h.typePath("", nil)
			h.deps.ReadPath = tc.read

			if code := cli.Run(tc.args, h.deps); code != 1 {
				t.Errorf("code = %d", code)
			}
			if !strings.HasSuffix(h.errb.String(), tc.want) || got.calls != 0 || h.out.Len() != 0 {
				t.Errorf("stderr = %q, want suffix %q, calls %d, stdout %q", h.errb, tc.want, got.calls, h.out)
			}
		})
	}
}

const msgNoPathText = "No path given. Type the path of the key file when asked, or pipe the key in.\n"

func TestGooglePlayAccountAddWithoutAKeySendsNothing(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, playAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.pipe(" \n")

	if code := cli.Run([]string{"google-play", "account", "add"}, h.deps); code != 1 || got.calls != 0 {
		t.Errorf("code = %d, calls %d", code, got.calls)
	}
	if want := "No key given. Pipe it in, or give the path of its file when asked.\n"; h.errb.String() != want {
		t.Errorf("stderr = %q", h.errb)
	}
}

// dragTarget writes content to a file named like a second download, with a space and parentheses.
func dragTarget(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "AuthKey_2X9R4HXF34 (1).p8")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// typedForms is how a terminal hands over path: as is, escaped like a dragged file, and in each kind of quotes.
func typedForms(path string) map[string]string {
	escaped := strings.NewReplacer(" ", `\ `, "(", `\(`, ")", `\)`).Replace(path)
	return map[string]string{
		"as is":         path,
		"escaped":       escaped,
		"single quotes": "'" + path + "'",
		"double quotes": `"` + path + `"`,
		"padded quotes": "  '" + path + "' ",
	}
}

func TestAppStoreKeyAddUnderstandsADraggedOrQuotedPath(t *testing.T) {
	path := dragTarget(t, p8Text+"\n")
	for name, typed := range typedForms(path) {
		var got flyRequest
		srv := flyServer(t, 200, storeAdded, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.typePath(typed, nil)

		if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, h.deps); code != 0 {
			t.Errorf("%s: code = %d, stderr %q", name, code, h.errb)
			continue
		}
		if !strings.Contains(got.raw, "MADEUPKEYTEXT") {
			t.Errorf("%s: request = %+v", name, got)
		}
	}
}

func TestGooglePlayAccountAddUnderstandsADraggedOrQuotedPath(t *testing.T) {
	path := dragTarget(t, serviceJSON)
	for name, typed := range typedForms(path) {
		var got flyRequest
		srv := flyServer(t, 200, playAdded, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.typePath(typed, nil)

		if code := cli.Run([]string{"google-play", "account", "add"}, h.deps); code != 0 {
			t.Errorf("%s: code = %d, stderr %q", name, code, h.errb)
			continue
		}
		var body struct{ Token string }
		_ = json.Unmarshal([]byte(got.raw), &body)
		if body.Token != serviceJSON {
			t.Errorf("%s: request = %+v", name, got)
		}
	}
}

func TestAKeyPathWithABackslashInsideQuotesIsKept(t *testing.T) {
	// Inside quotes a backslash is a character of the name, not an escape.
	path := filepath.Join(t.TempDir(), `odd\name.p8`)
	if err := os.WriteFile(path, []byte(p8Text), 0o600); err != nil {
		t.Skip("this file system refuses a backslash in a name")
	}
	var got flyRequest
	srv := flyServer(t, 200, storeAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.typePath("'"+path+"'", nil)

	if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, h.deps); code != 0 || !strings.Contains(got.raw, "MADEUPKEYTEXT") {
		t.Errorf("code = %d, stderr %q, request %+v", code, h.errb, got)
	}
}

func TestAKeyPathEndingInABackslashIsKeptAsTyped(t *testing.T) {
	var got flyRequest
	srv := flyServer(t, 200, storeAdded, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	h.typePath(`/no/such/folder\`, nil)

	if code := cli.Run([]string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, h.deps); code != 1 {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), `open /no/such/folder\: `) || got.calls != 0 {
		t.Errorf("stderr = %q, calls %d", h.errb, got.calls)
	}
}

// recordingTransport answers every request with body and keeps the URLs it was asked for.
type recordingTransport struct {
	body string
	urls []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

const msgHTTPS = "Cadre sends a key or a token over https only. Sign in to an https address first: cadre login --url https://<your cadre>.\n"

// credentialCommands are the four commands that send a key or a token, each with a body the server would accept.
var credentialCommands = []struct {
	name  string
	args  []string
	input string
	reply string
}{
	{"fly org add", []string{"fly", "org", "add", "acme"}, "fly_secret_token", addedBody},
	{"vercel team add", []string{"vercel", "team", "add", "colette-club"}, "vercel_secret_token", vercelAddedBody},
	{"app-store key add", []string{"app-store", "key", "add", "2X9R4HXF34", "--issuer", "iss"}, p8Text, storeAdded},
	{"google-play account add", []string{"google-play", "account", "add"}, serviceJSON, playAdded},
}

func TestCredentialsAreSentOverHTTPSOrToALoopbackOnly(t *testing.T) {
	refused := []string{
		"http://cadre.example.com",
		"http://cadre.example.com:4000",
		"http://localhost.example.com",
		"http://127.0.0.1.example.com",
		"http://128.0.0.1",
		"http://[::2]:4000",
		"http://10.0.0.5",
		"ftp://127.0.0.1",
		"cadre.example.com",
		"http://[::1",
	}
	sent := []string{
		"https://cadre.example.com",
		"HTTPS://cadre.example.com",
		"http://127.0.0.1:4000",
		"http://127.8.9.10",
		"http://localhost:4000",
		"http://LOCALHOST",
		"http://[::1]:4000",
		"https://127.0.0.1",
	}
	for _, c := range credentialCommands {
		for _, url := range refused {
			for _, asJSON := range []bool{false, true} {
				rt := &recordingTransport{body: c.reply}
				h := newHarness(t, httptest.NewServer(http.NotFoundHandler()), &config.Config{URL: url, APIKey: "cadre_abc"})
				h.deps.HTTP = &http.Client{Transport: rt}
				h.pipe(c.input)
				args := c.args
				if asJSON {
					args = append(slices.Clone(args), "--json")
				}

				if code := cli.Run(args, h.deps); code != 1 {
					t.Errorf("%s %s json=%v: code = %d", c.name, url, asJSON, code)
				}
				if h.errb.String() != msgHTTPS || h.out.Len() != 0 {
					t.Errorf("%s %s json=%v: stderr = %q, stdout = %q", c.name, url, asJSON, h.errb, h.out)
				}
				if len(rt.urls) != 0 {
					t.Errorf("%s %s json=%v: requests were sent: %v", c.name, url, asJSON, rt.urls)
				}
			}
		}
		for _, url := range sent {
			rt := &recordingTransport{body: c.reply}
			h := newHarness(t, httptest.NewServer(http.NotFoundHandler()), &config.Config{URL: url, APIKey: "cadre_abc"})
			h.deps.HTTP = &http.Client{Transport: rt}
			h.pipe(c.input)

			if code := cli.Run(c.args, h.deps); code != 0 {
				t.Errorf("%s %s: code = %d, stderr %q", c.name, url, code, h.errb)
			}
			if len(rt.urls) != 1 {
				t.Errorf("%s %s: requests = %v", c.name, url, rt.urls)
			}
		}
	}
}

func TestRemovingAnAccountNeedsNoHTTPS(t *testing.T) {
	for name, args := range map[string][]string{
		"fly":         {"fly", "org", "remove", "acme"},
		"vercel":      {"vercel", "team", "remove", "colette-club"},
		"app-store":   {"app-store", "key", "remove", "2X9R4HXF34"},
		"google-play": {"google-play", "account", "remove", playEmail},
	} {
		rt := &recordingTransport{body: `{"platform":"fly","name":"x","apps":0}`}
		h := newHarness(t, httptest.NewServer(http.NotFoundHandler()), &config.Config{URL: "http://cadre.example.com", APIKey: "cadre_abc"})
		h.deps.HTTP = &http.Client{Transport: rt}

		if code := cli.Run(args, h.deps); code != 0 || len(rt.urls) != 1 || strings.Contains(h.errb.String(), "https") {
			t.Errorf("%s: code = %d, requests %v, stderr %q", name, code, rt.urls, h.errb)
		}
	}
}
