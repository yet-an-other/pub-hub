package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/yet-an-other/pub-hub/internal/naming"
)

// describeProject fills a Project description when the Catalogue shows it empty.
func (c *cli) describeProject(args []string) error {
	if len(args) != 2 {
		return local("usage", "describe-project <project> <description>")
	}
	project, description := args[0], args[1]
	if err := naming.ValidateProject(project); err != nil {
		return nameError(err)
	}
	if strings.TrimSpace(description) == "" || len([]rune(description)) > 1000 {
		return local("request_invalid", "description must be nonempty and at most 1,000 characters")
	}
	base, token, err := c.credentials()
	if err != nil {
		return err
	}
	data, err := c.call(http.MethodGet, "/api/projects", token, base, "", nil, false)
	if err != nil {
		return err
	}
	var projects []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &projects); err != nil {
		return err
	}
	for _, existing := range projects {
		if existing.Name == project && existing.Description != "" {
			return nil
		}
	}
	body, err := json.Marshal(map[string]string{"description": description})
	if err != nil {
		return err
	}
	_, err = c.call(http.MethodPatch, "/api/projects/"+project, token, base, "application/json", func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}, false)
	return err
}
