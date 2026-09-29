// Package config loads the Portal's non-secret config from portal.toml.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the Portal's non-secret config. Secrets never live here; they
// arrive through systemd's LoadCredential=.
type Config struct {
	// Socket is the unix socket the Portal listens on. Only nginx can reach it.
	Socket string `toml:"socket"`

	// ZitadelIssuerURL is the canonical issuer URL used for token
	// introspection.
	ZitadelIssuerURL string `toml:"zitadel_issuer_url"`
	// HubAPIClientID identifies the confidential Zitadel app used by the
	// Portal when it introspects PATs. Its secret arrives through
	// systemd's LoadCredential=.
	HubAPIClientID string `toml:"hub_api_client_id"`
	// Publishers maps Zitadel user IDs to the Publisher labels recorded by the
	// Portal.
	Publishers map[string]string `toml:"publishers"`
	// OwnerEmail is the only browser identity admitted under /ui/api/.
	OwnerEmail string `toml:"owner_email"`

	// S3Endpoint is the RGW endpoint. Credentials arrive through
	// systemd's LoadCredential=.
	S3Endpoint string `toml:"s3_endpoint"`
	// ArtifactBucket holds Reader-visible Artifact bytes.
	ArtifactBucket string `toml:"artifact_bucket"`
	// MetadataBucket holds private Artifact records.
	MetadataBucket string `toml:"metadata_bucket"`
	// PublicBaseURL is the origin used to construct Reader URLs.
	PublicBaseURL string `toml:"public_base_url"`
	// SpoolDirectory holds uploads until they are validated and published.
	SpoolDirectory string `toml:"spool_directory"`
}

// Load reads and validates the config at path. Unknown keys are an error, so
// a typo cannot silently fall back to a default.
func Load(path string) (Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if cfg.SpoolDirectory == "" {
		cfg.SpoolDirectory = "/var/cache/pubhub/spool"
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch {
	case c.Socket == "":
		return errors.New("socket is required")
	case !filepath.IsAbs(c.Socket):
		return fmt.Errorf("socket must be an absolute path, got %q", c.Socket)
	case c.ZitadelIssuerURL == "":
		return errors.New("zitadel issuer URL is required")
	case c.HubAPIClientID == "":
		return errors.New("hub API client id is required")
	}

	issuer, err := url.Parse(c.ZitadelIssuerURL)
	if err != nil || issuer.Scheme == "" || issuer.Host == "" {
		return fmt.Errorf("zitadel issuer URL must be an absolute URL, got %q", c.ZitadelIssuerURL)
	}
	if c.OwnerEmail == "" || strings.TrimSpace(c.OwnerEmail) != c.OwnerEmail || strings.ContainsAny(c.OwnerEmail, " \t\r\n") || !strings.Contains(c.OwnerEmail, "@") {
		return errors.New("valid owner email is required")
	}
	if c.S3Endpoint == "" || c.ArtifactBucket == "" || c.MetadataBucket == "" || c.PublicBaseURL == "" {
		return errors.New("s3 endpoint, artifact bucket, metadata bucket, and public base URL are required")
	}
	endpoint, err := url.Parse(c.S3Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("s3 endpoint must be an absolute http(s) URL without credentials or query, got %q", c.S3Endpoint)
	}
	endpointIP := net.ParseIP(endpoint.Hostname())
	if endpointIP == nil || !endpointIP.IsLoopback() {
		return fmt.Errorf("s3 endpoint must use a loopback IP address, got %q", c.S3Endpoint)
	}
	if c.ArtifactBucket == c.MetadataBucket {
		return errors.New("artifact and metadata buckets must be different")
	}
	if !filepath.IsAbs(c.SpoolDirectory) || filepath.Clean(c.SpoolDirectory) == string(filepath.Separator) {
		return fmt.Errorf("spool directory must be an absolute non-root path, got %q", c.SpoolDirectory)
	}
	publicURL, err := url.Parse(c.PublicBaseURL)
	if err != nil || publicURL.Scheme != "https" || publicURL.Host == "" || publicURL.User != nil || (publicURL.Path != "" && publicURL.Path != "/") || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return fmt.Errorf("public base URL must be an https origin, got %q", c.PublicBaseURL)
	}
	for userID, label := range c.Publishers {
		if strings.TrimSpace(userID) == "" {
			return errors.New("publisher user id must not be empty")
		}
		if strings.TrimSpace(userID) != userID {
			return fmt.Errorf("publisher user id must not have surrounding whitespace, got %q", userID)
		}
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("publisher label for %q must not be empty", userID)
		}
	}
	return nil
}
