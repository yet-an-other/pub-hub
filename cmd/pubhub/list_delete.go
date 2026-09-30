package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yet-an-other/pub-hub/internal/naming"
)

type listedArtifact struct {
	Path      string    `json:"path"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
	TotalSize int64     `json:"total_size"`
}

func (c *cli) list(args []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "print full API metadata")
	var options, positionals []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			options = append(options, arg)
		} else {
			positionals = append(positionals, arg)
		}
	}
	if err := flags.Parse(append(options, positionals...)); err != nil {
		return local("usage", err.Error())
	}
	if flags.NArg() > 1 {
		return local("usage", "list [prefix] [--json]")
	}
	prefix := ""
	if flags.NArg() == 1 {
		prefix = flags.Arg(0)
	}
	if err := naming.ValidatePrefix(prefix); err != nil {
		return nameError(err)
	}
	base, token, err := c.credentials()
	if err != nil {
		return err
	}
	path := "/api/artifacts"
	if prefix != "" {
		path += "?prefix=" + url.QueryEscape(prefix)
	}
	data, err := c.call(http.MethodGet, path, token, base, "", nil, false)
	if err != nil {
		return err
	}
	if *asJSON {
		return jsonOutput(c.out, data)
	}
	var artifacts []listedArtifact
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if _, err := fmt.Fprintf(c.out, "%s\t%s\t%s\t%s\t%d\n", artifact.Path, artifact.Title, artifact.State, artifact.UpdatedAt.Format(time.RFC3339), artifact.TotalSize); err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) delete(args []string) error {
	flags := flag.NewFlagSet("delete", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	yes := flags.Bool("yes", false, "skip confirmation")
	var options, positionals []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			options = append(options, arg)
		} else {
			positionals = append(positionals, arg)
		}
	}
	if err := flags.Parse(append(options, positionals...)); err != nil {
		return local("usage", err.Error())
	}
	if flags.NArg() != 1 {
		return local("usage", "delete <path> [--yes]")
	}
	path, err := naming.ParseArtifactPath(flags.Arg(0))
	if errors.Is(err, naming.ErrSuffixRequired) {
		return local("name_invalid", "Artifact path needs a .html or / suffix")
	}
	if err != nil {
		return nameError(err)
	}
	if !*yes {
		fmt.Fprintf(c.err, "Delete %s? [y/N] ", path.PublicPath())
		answer, err := bufio.NewReader(c.in).ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		answer = strings.TrimSpace(answer)
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			return failure{"cancelled", "Artifact not deleted", 1}
		}
	}
	base, token, err := c.credentials()
	if err != nil {
		return err
	}
	_, err = c.call(http.MethodDelete, "/api/artifacts/"+path.PublicPath(), token, base, "", nil, false)
	return err
}

func nameError(err error) error {
	code := "name_invalid"
	if errors.Is(err, naming.ErrNameReserved) {
		code = "name_reserved"
	}
	return local(code, err.Error())
}
