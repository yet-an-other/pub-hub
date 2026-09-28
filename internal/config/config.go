// Package config loads the Portal's non-secret config from portal.toml.
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the Portal's non-secret config. Secrets never live here; they
// arrive through systemd's LoadCredential=.
type Config struct {
	// Socket is the unix socket the Portal listens on. Only nginx can reach it.
	Socket string `toml:"socket"`
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
	}
	return nil
}
