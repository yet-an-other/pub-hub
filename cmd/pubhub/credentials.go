package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/term"
)

type config struct {
	Token string `toml:"token"`
	URL   string `toml:"url"`
}

func (c *cli) configPath() (string, error) {
	dir := c.configDir
	if dir == "" {
		dir = os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			home, e := os.UserHomeDir()
			if e != nil {
				return "", e
			}
			dir = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(dir, "pubhub", "config.toml"), nil
}
func (c *cli) credentials() (string, string, error) {
	base := os.Getenv("PUBHUB_URL")
	token := os.Getenv("PUBHUB_TOKEN")
	if base != "" && token != "" {
		return base, token, nil
	}
	path, err := c.configPath()
	if err != nil {
		return "", "", err
	}
	stat, err := os.Stat(path)
	if err == nil {
		if stat.Mode().Perm()&0o077 != 0 {
			return "", "", local("config_permissions", "config.toml must not be readable by group or others")
		}
		var cfg config
		if _, err = toml.DecodeFile(path, &cfg); err != nil {
			return "", "", local("config_invalid", err.Error())
		}
		if token == "" {
			token = cfg.Token
		}
		if base == "" {
			base = cfg.URL
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if base == "" {
		base = "https://hub.bdgn.me"
	}
	if token == "" {
		return "", "", local("auth_missing", "run pubhub login or set PUBHUB_TOKEN")
	}
	return base, token, nil
}
func (c *cli) login() error {
	path, err := c.configPath()
	if err != nil {
		return err
	}
	fmt.Fprint(c.err, "Zitadel PAT: ")
	var token string
	if file, ok := c.in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		b, e := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(c.err)
		if e != nil {
			return e
		}
		token = strings.TrimSpace(string(b))
	} else { // Piped input is useful for automation; an interactive terminal is always hidden.
		b, e := bufio.NewReader(c.in).ReadString('\n')
		if e != nil && e != io.EOF {
			return e
		}
		token = strings.TrimSpace(b)
	}
	if token == "" {
		return local("auth_missing", "empty PAT")
	}
	base := os.Getenv("PUBHUB_URL")
	if base == "" {
		base = "https://hub.bdgn.me"
	}
	if _, err = c.call("GET", "/api/whoami", token, base, "", nil, false); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write atomically, so a failed login or interrupted write leaves old credentials intact.
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err = fmt.Fprintf(file, "token = %q\nurl = %q\n", token, base); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	fmt.Fprintln(c.err, "Saved credentials to", path)
	return nil
}
