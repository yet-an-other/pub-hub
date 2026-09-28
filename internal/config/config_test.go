package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yet-an-other/pub-hub/internal/config"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "portal.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadReadsTheSocketPath(t *testing.T) {
	path := writeConfig(t, `socket = "/run/pubhub/portal.sock"`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Socket != "/run/pubhub/portal.sock" {
		t.Errorf("Socket = %q, want /run/pubhub/portal.sock", cfg.Socket)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"not TOML", `socket = `, "portal.toml"},
		{"socket missing", ``, "socket"},
		{"socket relative", `socket = "portal.sock"`, "socket"},
		{"unknown key", "socket = \"/run/pubhub/portal.sock\"\nsockett = \"x\"", "sockett"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tc.content))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.toml")
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("Load error = %v, want one naming %s", err, path)
	}
}
