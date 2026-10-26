package release_test

import (
	"testing"

	"github.com/djeusette/cadre_cli/internal/release"
)

func TestAssetNameMatchesTheInstallCommand(t *testing.T) {
	// The names GoReleaser gives the archives, which are also `cadre_$(uname -s)_$(uname -m).tar.gz`.
	cases := []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "cadre_Darwin_arm64.tar.gz"},
		{"darwin", "amd64", "cadre_Darwin_x86_64.tar.gz"},
		{"linux", "arm64", "cadre_Linux_aarch64.tar.gz"},
		{"linux", "amd64", "cadre_Linux_x86_64.tar.gz"},
	}
	for _, c := range cases {
		if got := release.AssetName(c.goos, c.goarch); got != c.want {
			t.Errorf("AssetName(%q, %q) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestChecksumsNameIsGoReleasersDefault(t *testing.T) {
	if got := release.ChecksumsName("0.5.0"); got != "cadre_0.5.0_checksums.txt" {
		t.Errorf("ChecksumsName = %q", got)
	}
}
