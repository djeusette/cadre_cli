// Package config reads and writes the cadre CLI's per-user configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultURL is the Cadre server used when neither CADRE_URL nor the config sets one.
const DefaultURL = "https://colette-cadre.fly.dev"

// Config is the content of the config file.
type Config struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
	Email  string `json:"email"`
}

// Path returns the config file location: $XDG_CONFIG_HOME/cadre/config.json,
// falling back to $HOME/.config/cadre/config.json.
func Path(getenv func(string) string) (string, error) {
	if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "cadre", "config.json"), nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("cannot locate the config directory: neither XDG_CONFIG_HOME nor HOME is set")
	}
	return filepath.Join(home, ".config", "cadre", "config.json"), nil
}

// Load reads the config at path. A missing file gives a zero Config.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// Save writes c to path atomically, with the directory at 0700 and the file at 0600.
func Save(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// BaseURL resolves the server URL: CADRE_URL, then c.URL, then DefaultURL.
// A trailing slash is trimmed.
func BaseURL(c Config, getenv func(string) string) string {
	url := getenv("CADRE_URL")
	if url == "" {
		url = c.URL
	}
	if url == "" {
		url = DefaultURL
	}
	return strings.TrimRight(url, "/")
}
