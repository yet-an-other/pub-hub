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
owner_email = "owner@example.test"
s3_endpoint = "http://127.0.0.1:7480"
artifact_bucket = "pubhub-artifacts"
metadata_bucket = "pubhub-meta"
public_base_url = "https://pub.bdgn.me"
spool_directory = "/var/cache/pubhub/spool"

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
	if cfg.OwnerEmail != "owner@example.test" {
		t.Errorf("OwnerEmail = %q", cfg.OwnerEmail)
	}
	if got := cfg.Publishers["user-123"]; got != "owner" {
		t.Errorf("Publishers[user-123] = %q, want owner", got)
	}
	if cfg.S3Endpoint != "http://127.0.0.1:7480" || cfg.ArtifactBucket != "pubhub-artifacts" || cfg.MetadataBucket != "pubhub-meta" || cfg.PublicBaseURL != "https://pub.bdgn.me" || cfg.SpoolDirectory != "/var/cache/pubhub/spool" {
		t.Errorf("storage config = (%q, %q, %q, %q)", cfg.S3Endpoint, cfg.ArtifactBucket, cfg.MetadataBucket, cfg.PublicBaseURL)
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
		{"owner missing", strings.Replace(validConfig(), "owner_email = \"owner@example.test\"\n", "", 1), "owner email"},
		{"owner invalid", strings.Replace(validConfig(), "owner@example.test", "owner example.test", 1), "owner email"},
		{"empty publisher id", strings.Replace(validConfig(), "\"user-123\" = \"owner\"", "\"\" = \"owner\"", 1), "publisher user id"},
		{"empty publisher label", strings.Replace(validConfig(), `"user-123" = "owner"`, `"user-123" = ""`, 1), "publisher label"},
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

func TestLoadDefaultsTheUploadSpoolDirectory(t *testing.T) {
	content := strings.Replace(validConfig(), "spool_directory = \"/var/cache/pubhub/spool\"\n", "", 1)
	cfg, err := config.Load(writeConfig(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SpoolDirectory != "/var/cache/pubhub/spool" {
		t.Errorf("SpoolDirectory = %q, want /var/cache/pubhub/spool", cfg.SpoolDirectory)
	}
}

func TestLoadRejectsInvalidStorageSettings(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"endpoint relative", strings.Replace(validConfig(), `"http://127.0.0.1:7480"`, `"rgw"`, 1), "s3 endpoint"},
		{"endpoint remote", strings.Replace(validConfig(), `"http://127.0.0.1:7480"`, `"https://s3.bdgn.me"`, 1), "loopback"},
		{"same buckets", strings.Replace(validConfig(), `metadata_bucket = "pubhub-meta"`, `metadata_bucket = "pubhub-artifacts"`, 1), "different"},
		{"public URL not HTTPS", strings.Replace(validConfig(), `"https://pub.bdgn.me"`, `"http://pub.bdgn.me"`, 1), "https origin"},
		{"spool directory relative", strings.Replace(validConfig(), `spool_directory = "/var/cache/pubhub/spool"`, `spool_directory = "spool"`, 1), "non-root path"},
		{"spool directory root", strings.Replace(validConfig(), `spool_directory = "/var/cache/pubhub/spool"`, `spool_directory = "/"`, 1), "non-root path"},
		{"missing bucket", strings.Replace(validConfig(), "artifact_bucket = \"pubhub-artifacts\"\n", "", 1), "required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %v, want it to mention %q", err, tc.want)
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
