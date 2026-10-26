package cli_test

import (
	"encoding/json"
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
	usage := "Usage: cadre <login --url <url>|whoami|project create <file>|spec pull <project>/<n> [-o <file>]|spec push <project>/<n> <file> [--base <version>]|version> [--json]\n"

	for _, args := range [][]string{{"login"}, {"login", "--url"}, {"login", srv.URL}, {"login", "--url", srv.URL, "extra"}} {
		h := newHarness(t, srv, nil)
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usage {
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
		{"spec", "push"},
		{"spec", "pull", "wish-reminders/1", file},
		{"spec"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		want := "Usage: cadre <login --url <url>|whoami|project create <file>|spec pull <project>/<n> [-o <file>]|spec push <project>/<n> <file> [--base <version>]|version> [--json]\n"
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != want {
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
	cases := [][]string{
		{"spec", "pull"},
		{"spec", "pull", "wish-reminders"},
		{"spec", "pull", "wish-reminders/0"},
		{"spec", "pull", "Wish/1"},
		{"spec", "pull", "wish-reminders/1", "extra"},
		{"spec", "pull", "wish-reminders/1", "-o"},
		{"spec", "pull", "wish-reminders/1", "-o="},
		{"spec", "pull", "wish-reminders/1", "-o", ""},
		{"spec", "pull", "wish-reminders/1", "-o", "a.md", "-o", "b.md"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		want := "Usage: cadre <login --url <url>|whoami|project create <file>|spec pull <project>/<n> [-o <file>]|spec push <project>/<n> <file> [--base <version>]|version> [--json]\n"
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != want {
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
