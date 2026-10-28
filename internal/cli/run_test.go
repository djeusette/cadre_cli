package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/djeusette/cadre_cli/internal/cli"
)

func newEnv() (cli.Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return cli.Env{
		Stdout:  &out,
		Stderr:  &errb,
		Getenv:  func(string) string { return "" },
		Version: "dev",
	}, &out, &errb
}

func TestRunVersion(t *testing.T) {
	env, out, _ := newEnv()
	if code := cli.Run([]string{"version"}, env); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if got := out.String(); got != "cadre dev\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	env, _, errb := newEnv()
	if code := cli.Run([]string{"nope"}, env); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Usage: cadre") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunUsageNamesPrLink(t *testing.T) {
	env, _, errb := newEnv()
	if code := cli.Run(nil, env); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "|pr link <pr-url> (<project>/<n>|--none)|") {
		t.Errorf("stderr = %q", errb.String())
	}
}
