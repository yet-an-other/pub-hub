package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestDescribeProjectFillsOnlyMissingDescription(t *testing.T) {
	var description string
	var present bool
	gets, patches := 0, 0
	c, out, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-pat" {
			t.Error("missing Publisher credentials")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects":
			gets++
			if !present {
				fmt.Fprint(w, `[]`)
			} else {
				json.NewEncoder(w).Encode([]map[string]string{{"name": "demo", "description": description}})
			}
		case r.Method == http.MethodPatch && r.URL.Path == "/api/projects/demo":
			patches++
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type = %q", r.Header.Get("Content-Type"))
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 {
				t.Errorf("invalid body: %v, %v", body, err)
			}
			description, present = body["description"], true
			json.NewEncoder(w).Encode(map[string]string{"name": "demo", "description": description})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	defer done()

	for _, initial := range []struct {
		present bool
		name    string
	}{{false, "missing"}, {true, "empty"}} {
		present, description = initial.present, ""
		before := patches
		if exit := c.run([]string{"describe-project", "demo", "A café guide."}); exit != 0 || description != "A café guide." || patches != before+1 {
			t.Fatalf("%s: exit %d, description %q, patches %d: %s", initial.name, exit, description, patches-before, stderr)
		}
		if exit := c.run([]string{"describe-project", "demo", "A different guide."}); exit != 0 || description != "A café guide." || patches != before+1 {
			t.Fatalf("existing: exit %d, description %q, patches %d: %s", exit, description, patches-before, stderr)
		}
	}
	if gets != 4 || out.Len() != 0 {
		t.Fatalf("gets %d, stdout %q", gets, out)
	}
}

func TestDescribeProjectValidation(t *testing.T) {
	calls := 0
	c, _, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	defer done()
	for _, args := range [][]string{
		{"describe-project"},
		{"describe-project", "BAD", "Valid description"},
		{"describe-project", "demo", "  "},
		{"describe-project", "demo", strings.Repeat("a", 1001)},
	} {
		if exit := c.run(args); exit != 2 {
			t.Fatalf("%v: exit %d: %s", args, exit, stderr)
		}
	}
	if calls != 0 {
		t.Fatalf("made %d requests for invalid input", calls)
	}
}

func TestDescribeProjectDoesNotPatchOnListFailure(t *testing.T) {
	c, _, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("patched without reading Projects")
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"forbidden","message":"denied"}}`)
	})
	defer done()
	if exit := c.run([]string{"describe-project", "demo", "A description."}); exit != 4 || !strings.Contains(stderr.String(), "forbidden") {
		t.Fatalf("exit %d: %s", exit, stderr)
	}
}
