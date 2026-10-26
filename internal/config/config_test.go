package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/djeusette/cadre_cli/internal/config"
)

func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSaveWritesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cadre", "config.json")
	want := config.Config{URL: "https://x.test", APIKey: "cadre_abc", Email: "a@b.c"}

	if err := config.Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 700", got)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != (config.Config{}) {
		t.Errorf("got %+v, want zero Config", got)
	}
}

func TestPathHonorsXDG(t *testing.T) {
	got, err := config.Path(getenvFrom(map[string]string{"XDG_CONFIG_HOME": "/x", "HOME": "/h"}))
	if err != nil || got != "/x/cadre/config.json" {
		t.Errorf("got %q, %v", got, err)
	}

	got, err = config.Path(getenvFrom(map[string]string{"HOME": "/h"}))
	if err != nil || got != "/h/.config/cadre/config.json" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestBaseURLIsTheStoredURL(t *testing.T) {
	if got := config.BaseURL(config.Config{URL: "https://cfg.test/"}); got != "https://cfg.test" {
		t.Errorf("stored: got %q", got)
	}
	if got := config.BaseURL(config.Config{}); got != "" {
		t.Errorf("none stored: got %q", got)
	}
}
