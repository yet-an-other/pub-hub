package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fake(t *testing.T, handler http.HandlerFunc) (*cli, *bytes.Buffer, *bytes.Buffer, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Setenv("PUBHUB_URL", server.URL)
	t.Setenv("PUBHUB_TOKEN", "test-pat")
	out, stderr := new(bytes.Buffer), new(bytes.Buffer)
	return &cli{in: strings.NewReader(""), out: out, err: stderr, client: server.Client(), sleep: func(time.Duration) {}, configDir: t.TempDir()}, out, stderr, server.Close
}
func TestPublish(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<title>Demo</title><link href="/absolute.css">`), 0600)
	os.WriteFile(filepath.Join(dir, "style.css"), []byte("css"), 0600)
	os.WriteFile(filepath.Join(dir, ".secret"), []byte("secret"), 0600)
	os.Symlink("index.html", filepath.Join(dir, "symlink.html"))
	calls := 0
	c, out, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/artifacts/proj/demo/" || r.Header.Get("If-None-Match") != "*" || r.Header.Get("Authorization") != "Bearer test-pat" {
			t.Errorf("request: %s %v", r.URL, r.Header)
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		fields := map[string]string{}
		for {
			part, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Error(e)
				break
			}
			b, _ := io.ReadAll(part)
			fields[part.FormName()] = string(b)
		}
		if len(fields) != 2 || fields["style.css"] != "css" || fields["index.html"] == "" {
			t.Errorf("parts: %v", fields)
		}
		fmt.Fprint(w, `{"url":"https://pub.bdgn.me/proj/demo/","file_count":2}`)
	})
	defer done()
	if exit := c.run([]string{"publish", dir, "proj/demo", "--no-overwrite"}); exit != 0 {
		t.Fatalf("exit %d: %s", exit, stderr)
	}
	if strings.TrimSpace(out.String()) != "https://pub.bdgn.me/proj/demo/" {
		t.Fatal(out.String())
	}
	if !strings.Contains(stderr.String(), "Skipped 1") || !strings.Contains(stderr.String(), "relative base") {
		t.Fatal(stderr.String())
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestDryRunAndValidation(t *testing.T) {
	calls := 0
	c, _, _, done := fake(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	defer done()
	file := filepath.Join(t.TempDir(), "test.html")
	os.WriteFile(file, []byte("<title>A</title>"), 0600)
	for _, tc := range [][]string{{"publish", file, "proj/UPPER"}, {"publish", file, "proj/demo/"}, {"publish", file, "proj/demo", "--dry-run"}} {
		exit := c.run(tc)
		if tc[len(tc)-1] == "--dry-run" {
			if exit != 0 {
				t.Fatal(exit)
			}
		} else if exit != 2 {
			t.Fatal(tc, exit)
		}
	}
	if calls != 0 {
		t.Fatal(calls)
	}
}
func TestErrorsAndRetries(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a.html")
	os.WriteFile(file, []byte("hi"), 0600)
	attempts := 0
	c, _, _, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"code":"storage_unavailable","message":"down"}}`)
			return
		}
		w.WriteHeader(412)
		fmt.Fprint(w, `{"error":{"code":"exists","message":"already published"}}`)
	})
	defer done()
	if exit := c.run([]string{"publish", file, "proj/demo", "--no-overwrite"}); exit != 3 || attempts != 3 {
		t.Fatal(exit, attempts)
	}
}
func TestWhoamiAndLogin(t *testing.T) {
	c, out, stderr, done := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" && r.Header.Get("Authorization") != "Bearer test-pat" {
			t.Error("bad token")
		}
		fmt.Fprint(w, `{"label":"owner"}`)
	})
	defer done()
	if c.run([]string{"whoami"}) != 0 || out.String() != "owner\n" {
		t.Fatal(out.String(), stderr.String())
	}
	c.in = strings.NewReader("secret\n")
	if c.run([]string{"login"}) != 0 {
		t.Fatal(stderr.String())
	}
	path, _ := c.configPath()
	stat, e := os.Stat(path)
	if e != nil || stat.Mode().Perm() != 0600 {
		t.Fatal(e, stat)
	}
	t.Setenv("PUBHUB_TOKEN", "")
	out.Reset()
	if c.run([]string{"whoami"}) != 0 || out.String() != "owner\n" {
		t.Fatal(out.String(), stderr.String())
	}
	os.Chmod(path, 0644)
	if exit := c.run([]string{"whoami"}); exit != 2 {
		t.Fatal(exit)
	}
}
func TestLocalLimitsAndMissingEntry(t *testing.T) {
	dir := t.TempDir()
	if _, err := inspect(dir, "proj/demo", ""); err == nil || !strings.Contains(err.Error(), "index_missing") {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "index.html")
	if err := os.WriteFile(file, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect(dir, "proj/demo", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(file, maxBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect(dir, "proj/demo", ""); err == nil || !strings.Contains(err.Error(), "too_large") {
		t.Fatal(err)
	}
}
func TestJSON(t *testing.T) {
	var out bytes.Buffer
	if err := jsonOutput(&out, []byte(`{"url":"x", "state":"published"}`)); err != nil {
		t.Fatal(err)
	}
	var value map[string]string
	if err := json.Unmarshal(out.Bytes(), &value); err != nil || value["state"] != "published" {
		t.Fatal(err)
	}
}
