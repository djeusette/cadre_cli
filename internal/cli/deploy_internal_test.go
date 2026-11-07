package cli

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// forwarding runs forwardSignals until the test ends, and returns its channel of signals and what it forwarded.
func forwarding(t *testing.T) (chan<- os.Signal, <-chan os.Signal) {
	t.Helper()
	sigs := make(chan os.Signal)
	forwarded := make(chan os.Signal, 2)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go forwardSignals(sigs, done, func(sig os.Signal) { forwarded <- sig })
	return sigs, forwarded
}

func nextForwarded(t *testing.T, forwarded <-chan os.Signal) os.Signal {
	t.Helper()
	select {
	case sig := <-forwarded:
		return sig
	case <-time.After(2 * time.Second):
		t.Fatal("nothing forwarded")
		return nil
	}
}

func TestForwardSignalsForwardsEverySIGTERM(t *testing.T) {
	sigs, forwarded := forwarding(t)

	for range 2 {
		sigs <- syscall.SIGTERM
		if sig := nextForwarded(t, forwarded); sig != syscall.SIGTERM {
			t.Errorf("forwarded %v", sig)
		}
	}
}

// The SIGTERM behind the interrupt shows the interrupt was read, and dropped.
func TestForwardSignalsNeverForwardsAnInterrupt(t *testing.T) {
	sigs, forwarded := forwarding(t)

	sigs <- os.Interrupt
	sigs <- syscall.SIGTERM

	if sig := nextForwarded(t, forwarded); sig != syscall.SIGTERM {
		t.Errorf("forwarded %v", sig)
	}
}

func TestForwardSignalsReturnsWhenDoneCloses(t *testing.T) {
	done := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		forwardSignals(make(chan os.Signal), done, func(os.Signal) { t.Error("forwarded without a signal") })
		close(returned)
	}()
	close(done)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("never returned")
	}
}

// vercelPayload is the JSON vercel deploy --json --non-interactive prints on stdout, as vercel 58.5.1 writes it.
const vercelPayload = `{
  "status": "ok",
  "deployment": {
    "id": "dpl_1",
    "url": "https://web-1a2b.vercel.app",
    "inspectorUrl": "https://vercel.com/colette/web/dpl_1",
    "readyState": "READY",
    "target": "production"
  },
  "message": "Deployment web-1a2b.vercel.app ready.",
  "next": [
    {
      "command": "vercel curl https://web-1a2b.vercel.app",
      "when": "Verify deployment, including when Deployment Protection is enabled"
    }
  ]
}
`

func TestDeploymentURLIsDeploymentURLInVercelsJSON(t *testing.T) {
	for _, tc := range []struct{ name, stdout, want string }{
		{"the payload", vercelPayload, "https://web-1a2b.vercel.app"},
		{"on one line", `{"deployment":{"url":"https://web-1a2b.vercel.app"}}`, "https://web-1a2b.vercel.app"},
		{"no deployment.url", `{"status":"ok","deployment":{"id":"dpl_1"}}`, ""},
		{"no deployment", `{"status":"ok","message":"https://web-1a2b.vercel.app"}`, ""},
		{"the bare deployment, as --json alone or a build error prints it", `{"id":"dpl_1","url":"https://web-1a2b.vercel.app"}`, ""},
		{"a URL that isn't https", `{"deployment":{"url":"web-1a2b.vercel.app"}}`, ""},
		{"a URL that reads as a flag", `{"deployment":{"url":"--yes"}}`, ""},
		{"a URL that isn't a string", `{"deployment":{"url":7}}`, ""},
		{"text before the JSON, which vercel never prints on stdout", "Deploying\n" + vercelPayload, ""},
		{"two payloads", vercelPayload + vercelPayload, ""},
		{"a URL line", "https://web-1a2b.vercel.app\n", ""},
		{"nothing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploymentURL(tc.stdout); got != tc.want {
				t.Errorf("deploymentURL(%q) = %q, want %q", tc.stdout, got, tc.want)
			}
		})
	}
}
