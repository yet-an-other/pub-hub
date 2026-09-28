// Package config loads the Portal's non-secret config from portal.toml.
package config

import (
	"errors"
	"fmt"
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
