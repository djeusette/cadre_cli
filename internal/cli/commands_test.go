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

// usageLine is what every usage error prints on stderr.
const usageLine = "Usage: cadre <login --url <url>|whoami|project create <file>|project pull <slug> [-o <file>]|project update <file>|project archive <slug>|project unarchive <slug>|spec pull <project>/<n> [-o <file>]|spec push <project>/<n> <file> [--base <version>]|pr link <pr-url> (<project>/<n>|--none)|repo set <repo> (--fly-app <app>|--platform <name>)|repo unset <repo> --platform <name>|repo show <repo>|upgrade|version> [--json]\n"

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
	usage := usageLine

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
		{"spec", "push", "wish-reminders/1", file, "--base", "-v1"},
		{"spec", "push"},
		{"spec", "pull", "wish-reminders/1", file},
		{"spec"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		want := usageLine
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
		{"spec", "pull", "wish-reminders/1", "-o", "-x"},
		{"spec", "pull", "wish-reminders/1", "-o=-x"},
		{"spec", "pull", "wish-reminders/1", "-o", "a.md", "-o", "b.md"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		want := usageLine
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
	cases := [][]string{
		{"project", "pull"},
		{"project", "pull", "-o", "p.json"},
		{"project", "pull", "wish-reminders", "-o"},
		{"project", "pull", "wish-reminders", "-o="},
		{"project", "pull", "wish-reminders", "extra"},
		{"project", "update"},
		{"project", "update", file, file},
		{"project", "archive"},
		{"project", "archive", "a", "b"},
		{"project", "unarchive"},
		{"project", "unarchive", "a", "b"},
		{"project"},
		{"project", "destroy", "a"},
	}
	want := usageLine
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != want {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
		}
	}
	if got.calls != 0 {
		t.Errorf("the server was called: %+v", got)
	}
}

type linkRequest struct {
	calls              int
	method, path, auth string
	body               map[string]any
}

func linkServer(t *testing.T, status int, body string, got *linkRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.calls++
		got.method, got.path, got.auth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
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
		{"pr"},
		{"pr", "link"},
		{"pr", "link", prURL},
		{"pr", "link", "--none"},
		{"pr", "link", prURL, "wish-reminders/2", "--none"},
		{"pr", "link", prURL, "nope"},
		{"pr", "link", prURL, "wish-reminders/0"},
		{"pr", "link", prURL, "wish-reminders/2", "extra"},
		{"pr", "unlink", prURL},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageLine {
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

const repoShowBody = `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true},{"platform":"render","settings":{},"tracked":false}]}`

func TestRepoSetFly(t *testing.T) {
	var got linkRequest
	body := `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "set", "colette-api", "--fly-app", "colette-api-prod"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Connected colette-api to the Fly app colette-api-prod.\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	settings, _ := got.body["settings"].(map[string]any)
	if got.method != "PUT" || got.path != "/api/repositories/colette-api/targets/fly" || got.auth != "Bearer cadre_abc" || len(got.body) != 1 || len(settings) != 1 || settings["app"] != "colette-api-prod" {
		t.Errorf("request = %+v", got)
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
	if want := "Removed colette-site's Render target.\n"; h.out.String() != want {
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
	body := `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod","port":8080},"tracked":true}]}`
	srv := linkServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"repo", "show", "colette-api"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, port 8080\n"; h.out.String() != want {
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
	if want := "colette-api (colette-club/colette-api)\n  Fly     app colette-api-prod\n  Render  not tracked\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got.method != "GET" || got.path != "/api/repositories/colette-api" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
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
	if want := "colette-api (colette-club/colette-api)\n  Fly  app colette-api-prod, region cdg\n"; h.out.String() != want {
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
			return "Fly isn't connected on " + server + " yet. Set it up on its Setup page.\n"
		}},
		{set, 502, `{"error":"platform_unreachable","platform":"fly"}`, func(string) string { return "Cadre couldn't reach Fly. Try again in a minute.\n" }},
		{set, 422, `{"errors":[{"path":"settings.app","message":"is not an app in the org"}]}`, func(string) string { return "settings.app: is not an app in the org\n" }},
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
	cases := [][]string{
		{"repo"},
		{"repo", "nope", "colette-api"},
		{"repo", "set"},
		{"repo", "set", "--fly-app", "colette-api-prod"},
		{"repo", "set", "colette-api"},
		{"repo", "set", "colette-api", "--fly-app", "colette-api-prod", "--platform", "render"},
		{"repo", "set", "colette-api", "--fly-app"},
		{"repo", "set", "colette-api", "--platform", "--fly-app"},
		{"repo", "set", "colette-api", "other", "--fly-app", "colette-api-prod"},
		{"repo", "unset", "colette-site"},
		{"repo", "unset", "--platform", "render"},
		{"repo", "unset", "colette-site", "--fly-app", "x"},
		{"repo", "show"},
		{"repo", "show", "colette-api", "colette-site"},
	}
	for _, args := range cases {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(args, h.deps); code != 2 || h.errb.String() != usageLine {
			t.Errorf("%q: code = %d, stderr %q", args, code, h.errb)
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
