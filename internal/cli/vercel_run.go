package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// errStopped is a vercel call that a caught signal kept from starting.
var errStopped = errors.New("cadre stopped before running vercel")

// buildThenPromote builds every app at once, then promotes each in name order once all are ready, so they go live
// together on one commit however long each build took. No build output reaches the terminal unless the build fails. A
// failed build or a signal promotes nothing, so production stays as it was. A signal during the builds says only that:
// the builds it ended didn't fail.
func (s *session) buildThenPromote(ctx context.Context, v *vercelDeployment) int {
	w := watchSignals()
	defer w.stop()

	builds := make([]*vercelCall, len(v.apps))
	for i, app := range v.apps {
		builds[i] = v.call(ctx, app, "deploy", "--prod", "--skip-domain", "--yes", "--non-interactive", "--json",
			"--project", app.project, "--scope", app.team)
	}
	w.run(builds...)
	if w.signalled() {
		fmt.Fprintln(s.env.Stderr, msgNotDeployed)
		return 1
	}

	urls := make([]string, len(builds))
	built := true
	for i, b := range builds {
		if urls[i] = deploymentURL(b.stdout.String()); b.err != nil || urls[i] == "" {
			fmt.Fprintf(s.env.Stderr, "%s's build failed:\n%s\n", oneLine(v.apps[i].name), b.buildFailure())
			built = false
		}
	}
	if !built {
		fmt.Fprintln(s.env.Stderr, msgNotDeployed)
		return 1
	}
	if !s.promoteAll(ctx, v, urls, w) {
		return 1
	}
	s.reportVercelDeployed(v, urls)
	return 0
}

// deploymentURL is deployment.url in the JSON that vercel deploy --json --non-interactive prints on stdout, where it
// prints nothing else, when that JSON's status is ok and its deployment's target is production. It is empty when stdout
// isn't that JSON, the build isn't an ok production one, or the URL isn't https: the build failed. Only that URL goes to
// vercel promote, so nothing else vercel prints can stand in for it.
func deploymentURL(stdout string) string {
	var answer struct {
		Status     string `json:"status"`
		Deployment struct {
			URL    string `json:"url"`
			Target string `json:"target"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(stdout), &answer); err != nil || answer.Status != "ok" || answer.Deployment.Target != "production" {
		return ""
	}
	if !strings.HasPrefix(answer.Deployment.URL, "https://") {
		return ""
	}
	return answer.Deployment.URL
}

// promoteAll promotes each app's build in name order, one at a time, and reports whether every app went live. The first
// that fails or that a signal stops ends the rest, so the apps after it stay as they were, and running cadre deploy
// again builds and promotes them all.
func (s *session) promoteAll(ctx context.Context, v *vercelDeployment, urls []string, w *signalWatch) bool {
	for i, app := range v.apps {
		promote := v.call(ctx, app, "promote", urls[i], "--yes", "--non-interactive", "--scope", app.team)
		if w.run(promote); promote.err == nil && !w.signalled() {
			continue
		}
		s.reportPromotionStopped(v, i, promote, w)
		return false
	}
	return true
}

// reportPromotionStopped says how the promotion of v.apps[i] ended the promotions. A signal before it started leaves it
// and the apps after it untried. One during it may have let it go through, so the person checks, and the apps after it
// are untried. A failure names what vercel printed, then where every app stands.
func (s *session) reportPromotionStopped(v *vercelDeployment, i int, promote *vercelCall, w *signalWatch) {
	switch {
	case errors.Is(promote.err, errStopped):
		s.reportUntried(v.apps[i:])
	case w.signalled():
		fmt.Fprintf(s.env.Stderr, "cadre stopped while promoting %s. Check Vercel to see whether it went live.\n", oneLine(v.apps[i].name))
		s.reportUntried(v.apps[i+1:])
	default:
		fmt.Fprintf(s.env.Stderr, "%s built but didn't go live: %s\n", oneLine(v.apps[i].name), promote.said())
		fmt.Fprintln(s.env.Stderr, standing(v, i))
	}
}

// reportUntried names each app whose promotion a signal kept from starting.
func (s *session) reportUntried(apps []vercelApp) {
	for _, app := range apps {
		fmt.Fprintf(s.env.Stderr, "%s built but didn't go live: cadre stopped before promoting it.\n", oneLine(app.name))
	}
}

// standing is where the apps stand once the promotion of v.apps[failed] failed: the ones before it went live, and it and
// the ones after it weren't promoted. Each part shows only when it has apps.
func standing(v *vercelDeployment, failed int) string {
	notPromoted := appNames(v.apps[failed:]) + " weren't promoted."
	if len(v.apps)-failed == 1 {
		notPromoted = appNames(v.apps[failed:]) + " wasn't promoted."
	}
	if failed == 0 {
		return notPromoted
	}
	return appNames(v.apps[:failed]) + " went live on " + v.commit[:7] + ". " + notPromoted
}

// appNames is the apps' names, each on one line, joined by commas.
func appNames(apps []vercelApp) string {
	names := make([]string, len(apps))
	for i, app := range apps {
		names[i] = oneLine(app.name)
	}
	return strings.Join(names, ", ")
}

// reportVercelDeployed names every app on one line, and with --json prints one object per app.
func (s *session) reportVercelDeployed(v *vercelDeployment, urls []string) {
	fmt.Fprintf(s.progress(), "Deployed %s to %s. Cadre shows each on its next check.\n", v.commit[:7], appNames(v.apps))
	if !s.asJSON {
		return
	}
	out := json.NewEncoder(s.env.Stdout)
	for i, app := range v.apps {
		_ = out.Encode(struct {
			Repo   string `json:"repo"`
			App    string `json:"app"`
			Commit string `json:"commit"`
			URL    string `json:"url"`
		}{v.repository.Name, app.name, v.commit, urls[i]})
	}
}

// vercelCall is one vercel child. It keeps all it printed, in order, for when it fails, and its stdout alone, where the
// deployment URL is.
type vercelCall struct {
	cmd     *exec.Cmd
	stdout  bytes.Buffer
	printed lockedBuffer
	err     error
}

// call is vercel with args, run in app's folder. Its stdin is nil, the null device, so nothing can wait on an answer.
func (v *vercelDeployment) call(ctx context.Context, app vercelApp, args ...string) *vercelCall {
	c := &vercelCall{cmd: exec.CommandContext(ctx, v.vercel, args...)}
	c.cmd.Dir = filepath.Join(v.checkout.Dir, app.dir)
	c.cmd.Stdin = nil
	c.cmd.Stdout = io.MultiWriter(&c.stdout, &c.printed)
	c.cmd.Stderr = &c.printed
	return c
}

// said is what vercel printed or, when it printed nothing, how it ended.
func (c *vercelCall) said() string {
	if printed := strings.TrimRight(c.printed.String(), " \t\r\n"); printed != "" || c.err == nil {
		return printed
	}
	return c.err.Error()
}

// buildFailure is what a failed build shows: what vercel printed, and for a build that ended well, that it gave no ok
// production deployment with a URL.
func (c *vercelCall) buildFailure() string {
	if c.err != nil {
		return c.said()
	}
	return strings.TrimPrefix(c.said()+"\nvercel gave no production deployment to promote.", "\n")
}

// lockedBuffer is a buffer that a child's stdout and stderr, each copied on its own goroutine, can both write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// signalWatch keeps cadre up through SIGINT and SIGTERM while vercel runs, so it can say how the deploy ended. It
// forwards each SIGTERM to every live child through forwardSignals, as vercel wouldn't otherwise receive one, and
// remembers that a signal came, so nothing is promoted after one.
type signalWatch struct {
	sigs, caught chan os.Signal
	done         chan struct{}
	mu           sync.Mutex
	live         []*exec.Cmd
}

func watchSignals() *signalWatch {
	w := &signalWatch{sigs: make(chan os.Signal, 2), caught: make(chan os.Signal, 1), done: make(chan struct{})}
	signal.Notify(w.sigs, os.Interrupt, syscall.SIGTERM)
	signal.Notify(w.caught, os.Interrupt, syscall.SIGTERM)
	go forwardSignals(w.sigs, w.done, w.forward)
	return w
}

func (w *signalWatch) stop() {
	signal.Stop(w.sigs)
	signal.Stop(w.caught)
	close(w.done)
}

// signalled reports whether a signal came since the watch began. The first one stays in caught, which nothing reads.
func (w *signalWatch) signalled() bool { return len(w.caught) > 0 }

func (w *signalWatch) forward(sig os.Signal) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, cmd := range w.live {
		_ = cmd.Process.Signal(sig)
	}
}

// run starts every call, then waits for each, so they all run at once. A signal that comes while they start reaches
// them once all have. A call that can't start ends with why. Right before each start, under the lock that forwarding
// takes, it checks for a signal again: one caught since the caller last looked keeps the call from starting, as
// errStopped.
func (w *signalWatch) run(calls ...*vercelCall) {
	w.mu.Lock()
	for _, c := range calls {
		if w.signalled() {
			c.err = errStopped
			continue
		}
		if err := c.cmd.Start(); err != nil {
			c.err = fmt.Errorf("Could not run vercel: %w", err)
			continue
		}
		w.live = append(w.live, c.cmd)
	}
	w.mu.Unlock()

	for _, c := range calls {
		if c.err == nil {
			c.err = c.cmd.Wait()
		}
	}
	w.mu.Lock()
	w.live = nil
	w.mu.Unlock()
}
