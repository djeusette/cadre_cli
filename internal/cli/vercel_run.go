package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

// buildThenPromote builds every app at once, then promotes each in name order once all are ready, so they go live
// together on one commit however long each build took. No build output reaches the terminal unless the build fails. A
// failed build or a signal promotes nothing, so production stays as it was.
func (s *session) buildThenPromote(ctx context.Context, v *vercelDeployment) int {
	w := watchSignals()
	defer w.stop()

	builds := make([]*vercelCall, len(v.apps))
	for i, app := range v.apps {
		builds[i] = v.call(ctx, app, "deploy", "--prod", "--skip-domain", "--yes", "--non-interactive", "--json",
			"--project", app.project, "--scope", app.team)
	}
	w.run(builds...)

	urls := make([]string, len(builds))
	built := true
	for i, b := range builds {
		if urls[i] = deploymentURL(b.stdout.String()); b.err != nil || urls[i] == "" {
			fmt.Fprintf(s.env.Stderr, "%s's build failed:\n%s\n", oneLine(v.apps[i].name), b.buildFailure())
			built = false
		}
	}
	if !built || w.signalled() {
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
// prints nothing else, or empty when stdout isn't that JSON or the URL isn't https. Only that URL goes to vercel promote,
// so nothing else vercel prints can stand in for it.
func deploymentURL(stdout string) string {
	var answer struct {
		Deployment struct {
			URL string `json:"url"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(stdout), &answer); err != nil || !strings.HasPrefix(answer.Deployment.URL, "https://") {
		return ""
	}
	return answer.Deployment.URL
}

// promoteAll promotes each app's build in name order, one at a time, and reports whether every app went live. The first
// that fails stops the rest, so the apps after it stay as they were, and running cadre deploy again builds and promotes
// them all. A signal stops the promotions not started yet.
func (s *session) promoteAll(ctx context.Context, v *vercelDeployment, urls []string, w *signalWatch) bool {
	live := true
	for i, app := range v.apps {
		name := oneLine(app.name)
		if w.signalled() {
			fmt.Fprintf(s.env.Stderr, "%s built but didn't go live: cadre stopped before promoting it.\n", name)
			live = false
			continue
		}
		promote := v.call(ctx, app, "promote", urls[i], "--yes", "--non-interactive", "--scope", app.team)
		if w.run(promote); promote.err != nil {
			fmt.Fprintf(s.env.Stderr, "%s built but didn't go live: %s\n", name, promote.said())
			return false
		}
	}
	return live
}

// reportVercelDeployed names every app on one line, and with --json prints one object per app.
func (s *session) reportVercelDeployed(v *vercelDeployment, urls []string) {
	names := make([]string, len(v.apps))
	for i, app := range v.apps {
		names[i] = oneLine(app.name)
	}
	fmt.Fprintf(s.progress(), "Deployed %s to %s. Cadre shows each on its next check.\n", v.commit[:7], strings.Join(names, ", "))
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

// buildFailure is what a failed build shows: what vercel printed, and for a build that ended well, that it gave no URL.
func (c *vercelCall) buildFailure() string {
	if c.err != nil {
		return c.said()
	}
	return strings.TrimPrefix(c.said()+"\nvercel printed no deployment URL.", "\n")
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
// them once all have. A call that can't start ends with why.
func (w *signalWatch) run(calls ...*vercelCall) {
	w.mu.Lock()
	for _, c := range calls {
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
