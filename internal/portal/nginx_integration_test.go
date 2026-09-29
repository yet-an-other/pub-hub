package portal

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise the shipped nginx block against the Artifacts published by the Portal.
func testReaderNginx(t *testing.T, endpoint, bucket string) {
	t.Helper()
	if _, err := exec.LookPath("nginx"); err != nil {
		t.Skip("install nginx to run Reader integration checks")
	}
	var mu sync.Mutex
	var requests []http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Header.Clone())
		mu.Unlock()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint+r.URL.RequestURI(), nil)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Copy the forwarded headers to RGW to test weak validators and ranges.
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("x-amz-request-id", "private-id")
		w.Header().Set("x-rgw-id", "private-id")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer backend.Close()
	host := strings.TrimPrefix(backend.URL, "http://")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	source, err := os.ReadFile("../../deploy/pub.bdgn.me.conf")
	if err != nil {
		t.Fatal(err)
	}
	block := string(source)
	block = strings.ReplaceAll(block, "127.0.0.1:7480", host)
	block = strings.ReplaceAll(block, "pubhub-artifacts", bucket)
	block = strings.Replace(block, "listen 443 ssl;", "listen "+address+";", 1)
	block = strings.Replace(block, "    listen [::]:443 ssl;", "", 1)
	block = strings.Replace(block, "    http2 on;", "", 1)
	dir := t.TempDir()
	config := filepath.Join(dir, "nginx.conf")
	text := fmt.Sprintf("pid %s;\nerror_log stderr;\nevents {}\nhttp { access_log off; %s\n}", filepath.Join(dir, "nginx.pid"), block)
	if err := os.WriteFile(config, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	check := exec.Command("nginx", "-t", "-c", config, "-p", dir)
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v: %s", err, out)
	}
	cmd := exec.Command("nginx", "-c", config, "-p", dir, "-g", "daemon off;")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	base := "http://" + address
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	var response *http.Response
	for i := 0; i < 50; i++ {
		response, err = client.Get(base + "/xform/notes/plan.html")
		if err == nil {
			response.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("nginx not ready: %v", err)
	}
	test := func(method, path string, headers map[string]string, status int, contentType string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != status {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, status, body)
		}
		if contentType != "" && resp.Header.Get("Content-Type") != contentType {
			t.Errorf("%s Content-Type = %q", path, resp.Header.Get("Content-Type"))
		}
		for k, want := range map[string]string{"Cache-Control": "no-cache, no-transform", "X-Robots-Tag": "noindex, nofollow", "X-Content-Type-Options": "nosniff"} {
			if got := resp.Header.Get(k); got != want {
				t.Errorf("%s %s = %q, want %q", path, k, got, want)
			}
		}
		for k := range resp.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-") || strings.HasPrefix(strings.ToLower(k), "x-rgw-") {
				t.Errorf("%s leaked %s", path, k)
			}
		}
		return resp
	}
	page := test("GET", "/xform/notes/plan.html?ignored=1", map[string]string{"Cookie": "secret=1", "Authorization": "Bearer secret", "X-Probe": "not-forwarded"}, 200, "text/html")
	etag := page.Header.Get("ETag")
	if etag == "" {
		t.Error("missing S3 ETag")
	} else {
		test("GET", "/xform/notes/plan.html", map[string]string{"If-None-Match": "W/" + etag}, 304, "")
	}
	test("HEAD", "/xform/demo/", nil, 200, "text/html")
	test("GET", "/xform/demo/", nil, 200, "text/html")
	test("GET", "/xform/demo/index.html", nil, 200, "text/html")
	test("GET", "/xform/demo/asset%20name.txt", nil, 200, "text/plain")
	test("GET", "/xform/demo/old.css", map[string]string{"Range": "bytes=0-3"}, 206, "text/css")
	redirect := test("GET", "/xform/demo?foo=bar", nil, 301, "")
	if got := redirect.Header.Get("Location"); got != "/xform/demo/?foo=bar" {
		t.Errorf("Location = %q", got)
	}
	for _, path := range []string{"/", "/xform/", "/xform/notes/", "/robots.txt", "/xform/notes/missing.html"} {
		test("GET", path, nil, 404, "")
	}
	test("GET", "/xform/demo/", map[string]string{"Service-Worker": "script"}, 403, "")
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		test(method, "/xform/demo/", nil, 405, "")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) == 0 {
		t.Fatal("no RGW requests")
	}
	var sawCleanETag bool
	for _, h := range requests {
		for _, name := range []string{"Cookie", "Authorization", "X-Probe"} {
			if h.Get(name) != "" {
				t.Errorf("RGW received %s", name)
			}
		}
		if etag != "" && h.Get("If-None-Match") == etag {
			sawCleanETag = true
		}
	}
	if etag != "" && !sawCleanETag {
		t.Error("RGW did not receive the strong ETag")
	}
}
