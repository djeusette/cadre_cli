package cli_test

import (
	"bytes"
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
