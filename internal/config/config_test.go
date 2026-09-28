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

func validConfig() string {
	return `socket = "/run/pubhub/portal.sock"
zitadel_issuer_url = "https://zitadel.example.test"
hub_api_client_id = "hub-api"

[publishers]
"user-123" = "owner"
`
}

func TestLoadReadsThePortalSettings(t *testing.T) {
	path := writeConfig(t, validConfig())

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Socket != "/run/pubhub/portal.sock" {
		t.Errorf("Socket = %q, want /run/pubhub/portal.sock", cfg.Socket)
	}
	if cfg.ZitadelIssuerURL != "https://zitadel.example.test" {
		t.Errorf("ZitadelIssuerURL = %q", cfg.ZitadelIssuerURL)
	}
	if cfg.HubAPIClientID != "hub-api" {
		t.Errorf("HubAPIClientID = %q", cfg.HubAPIClientID)
	}
	if got := cfg.Publishers["user-123"]; got != "owner" {
		t.Errorf("Publishers[user-123] = %q, want owner", got)
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

func TestLoadRejectsInvalidAuthenticationSettings(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"issuer missing", "socket = \"/run/pubhub/portal.sock\"\nhub_api_client_id = \"hub-api\"", "zitadel issuer URL"},
		{"issuer relative", "socket = \"/run/pubhub/portal.sock\"\nzitadel_issuer_url = \"zitadel\"\nhub_api_client_id = \"hub-api\"", "absolute URL"},
		{"client id missing", "socket = \"/run/pubhub/portal.sock\"\nzitadel_issuer_url = \"https://zitadel.example.test\"", "hub API client id"},
		{"empty publisher id", strings.Replace(validConfig(), "\"user-123\" = \"owner\"", "\"\" = \"owner\"", 1), "publisher user id"},
		{"empty publisher label", "socket = \"/run/pubhub/portal.sock\"\nzitadel_issuer_url = \"https://zitadel.example.test\"\nhub_api_client_id = \"hub-api\"\n\n[publishers]\n\"user-123\" = \"\"", "publisher label"},
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
