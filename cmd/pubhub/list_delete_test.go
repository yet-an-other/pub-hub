package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestListArtifacts(t *testing.T) {
	calls := 0
	payload := `[ {"path":"xform/notes/plan.html","url":"https://pub.bdgn.me/xform/notes/plan.html","title":"Plan","state":"published","updated_at":"2025-09-30T12:34:56Z","total_size":1024,"file_count":1,"description":"private"} ]`
	c, out, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/api/artifacts" || r.URL.Query().Get("prefix") != "xform/" || r.Header.Get("Authorization") != "Bearer test-pat" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, payload)
	})
	defer done()
	if exit := c.run([]string{"list", "xform/"}); exit != 0 {
		t.Fatalf("list exit %d: %s", exit, stderr)
	}
	for _, field := range []string{"xform/notes/plan.html", "Plan", "published", "2025-09-30T12:34:56Z", "1024"} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("list output missing %q: %q", field, out.String())
		}
	}
	out.Reset()
	if exit := c.run([]string{"list", "xform/", "--json"}); exit != 0 {
		t.Fatalf("json exit %d: %s", exit, stderr)
	}
	if !strings.Contains(out.String(), `"description":"private"`) || !strings.Contains(out.String(), `"file_count":1`) {
		t.Errorf("json omitted metadata: %q", out.String())
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestDeleteArtifact(t *testing.T) {
	calls := 0
	c, out, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/api/artifacts/xform/notes/plan.html" || r.Header.Get("Authorization") != "Bearer test-pat" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if calls == 1 {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"Artifact not found"}}`)
		}
	})
	defer done()
	path := "xform/notes/plan.html"
	if exit := c.run([]string{"delete", "xform/notes/plan"}); exit != 2 || calls != 0 {
		t.Fatalf("suffixless path: exit %d calls %d", exit, calls)
	}
	stderr.Reset()
	c.in = strings.NewReader("no\n")
	if exit := c.run([]string{"delete", path}); exit == 0 || calls != 0 {
		t.Fatalf("declined delete: exit %d calls %d", exit, calls)
	}
	stderr.Reset()
	c.in = strings.NewReader("yes\n")
	if exit := c.run([]string{"delete", path}); exit != 0 || out.Len() != 0 || calls != 1 {
		t.Fatalf("confirmed delete: exit %d out %q calls %d stderr %q", exit, out, calls, stderr)
	}
	stderr.Reset()
	if exit := c.run([]string{"delete", path, "--yes"}); exit != 1 || calls != 2 || !strings.Contains(stderr.String(), "error: not_found: Artifact not found") {
		t.Fatalf("missing delete: exit %d calls %d stderr %q", exit, calls, stderr)
	}
}

func TestListAndDeleteUsePublishErrorsAndRetries(t *testing.T) {
	for _, command := range [][]string{{"list", "--json"}, {"delete", "xform/demo/", "--yes"}} {
		t.Run(command[0], func(t *testing.T) {
			calls := 0
			c, _, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"error":{"code":"storage_unavailable","message":"try later"}}`)
			})
			defer done()
			if exit := c.run(command); exit != 5 || calls != 4 || !strings.Contains(stderr.String(), "error: storage_unavailable: try later") {
				t.Fatalf("exit %d calls %d stderr %q", exit, calls, stderr)
			}
		})
	}
}

func TestListAndDeleteValidateBeforeCallingPortal(t *testing.T) {
	calls := 0
	c, _, _, done := fake(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	defer done()
	for _, args := range [][]string{{"list", "xform/index"}, {"list", "xform/UPPER"}, {"delete", "xform/demo"}, {"delete", "xform/index.html", "--yes"}} {
		if exit := c.run(args); exit != 2 {
			t.Errorf("%v: exit %d, want 2", args, exit)
		}
	}
	if calls != 0 {
		t.Fatalf("made %d invalid requests", calls)
	}
}
