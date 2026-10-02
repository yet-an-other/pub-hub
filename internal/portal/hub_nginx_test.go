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
	"testing"
	"time"
)

// Run the shipped hub server block against a stub auth endpoint and a header
// echoing Portal. This catches header forwarding mistakes in the nginx config.
func TestHubNginxNeverForwardsForgedIdentity(t *testing.T) {
	if _, err := exec.LookPath("nginx"); err != nil {
		t.Skip("nginx not installed")
	}
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s|%s|%s", r.Header.Get("X-Auth-Request-Email"), r.Header.Get("X-Auth-Request-User"), r.Header.Get("X-Auth-Request-Access-Token"), r.Host)
	}))
	defer portal.Close()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/auth" {
			if r.Header.Get("Cookie") != "session=valid" && r.Header.Get("Cookie") != "session=split" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("X-Auth-Request-Email", "owner@example.test")
			w.Header().Set("X-Auth-Request-User", "user-123")
			w.Header().Set("X-Auth-Request-Access-Token", "trusted-token")
			w.Header().Add("Set-Cookie", "__Host-pubhub=refreshed; Expires=Wed, 21 Oct 2030 07:28:00 GMT; Path=/; Secure; HttpOnly; SameSite=Lax")
			if r.Header.Get("Cookie") == "session=split" {
				w.Header().Add("Set-Cookie", "__Host-pubhub_1=part2; Path=/; Secure; HttpOnly; SameSite=Lax")
			}
			w.WriteHeader(202)
			return
		}
		w.WriteHeader(200)
	}))
	defer auth.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	source, err := os.ReadFile("../../deploy/hub.bdgn.me.conf")
	if err != nil {
		t.Fatal(err)
	}
	block := string(source)
	block = strings.Replace(block, "    deny  all;", "    allow 127.0.0.1;\n    deny  all;", 1)
	block = strings.ReplaceAll(block, "unix:/run/pubhub/portal.sock", strings.TrimPrefix(portal.URL, "http://"))
	block = strings.ReplaceAll(block, "unix:/run/oauth2-proxy/o2p.sock", strings.TrimPrefix(auth.URL, "http://"))
	block = strings.Replace(block, "listen 443 ssl;", "listen "+address+";", 1)
	block = strings.Replace(block, "    listen [::]:443 ssl;", "", 1)
	block = strings.Replace(block, "    http2 on;", "", 1)
	dir := t.TempDir()
	config := filepath.Join(dir, "nginx.conf")
	text := fmt.Sprintf("pid %s;\nerror_log stderr;\nevents {}\nhttp { access_log off; client_body_temp_path %s; proxy_temp_path %s; fastcgi_temp_path %s; uwsgi_temp_path %s; scgi_temp_path %s; %s\n}", filepath.Join(dir, "nginx.pid"), filepath.Join(dir, "body"), filepath.Join(dir, "proxy"), filepath.Join(dir, "fastcgi"), filepath.Join(dir, "uwsgi"), filepath.Join(dir, "scgi"), block)
	if err := os.WriteFile(config, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("nginx", "-t", "-c", config, "-p", dir).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v: %s", err, out)
	}
	cmd := exec.Command("nginx", "-c", config, "-p", dir, "-g", "daemon off;")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + address
	for i := 0; i < 50; i++ {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
		if i == 49 {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path, cookie, want string
		status             int
	}{
		{"/api/whoami", "", "|||hub.bdgn.me", 200},
		{"/healthz", "", "|||hub.bdgn.me", 200},
		{"/readyz", "", "|||hub.bdgn.me", 200},
		{"/ui/api/whoami", "session=valid", "owner@example.test|user-123|trusted-token|hub.bdgn.me", 200},
		{"/ui/api/whoami", "session=split", "owner@example.test|user-123|trusted-token|hub.bdgn.me", 200},
		{"/ui/api/whoami", "", `"code":"unauthenticated"`, 401},
		{"/", "session=valid", "owner@example.test|user-123|trusted-token|hub.bdgn.me", 200},
		{"/", "session=split", "owner@example.test|user-123|trusted-token|hub.bdgn.me", 200},
		{"/", "", "/oauth2/sign_in", 302},
		{"/robots.txt", "", "404", 404},
	} {
		req, _ := http.NewRequest("GET", base+tc.path, nil)
		req.Host = "hub.bdgn.me"
		req.Header.Set("X-Auth-Request-Email", "forged@example.test")
		req.Header.Set("X-Auth-Request-User", "forged")
		req.Header.Set("X-Auth-Request-Access-Token", "forged")
		req.Header.Set("X-Forwarded-Uri", "/oauth2/auth")
		if tc.cookie != "" {
			req.Header.Set("Cookie", tc.cookie)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := string(body)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d want %d: %s", tc.path, resp.StatusCode, tc.status, got)
		}
		if tc.status == 302 {
			got = resp.Header.Get("Location")
		}
		if !strings.Contains(got, tc.want) || strings.Contains(got, "forged") {
			t.Errorf("%s: response %q, want %q without forged identity", tc.path, got, tc.want)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: unexpected CORS header", tc.path)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing security headers", tc.path)
		}
		cookies := resp.Header.Values("Set-Cookie")
		switch tc.cookie {
		case "session=valid":
			if len(cookies) != 1 || !strings.Contains(cookies[0], "__Host-pubhub=refreshed") {
				t.Errorf("%s: refreshed cookies = %q, want one", tc.path, cookies)
			}
		case "session=split":
			if len(cookies) != 2 || !strings.Contains(cookies[1], "__Host-pubhub_1=part2; Path=/; Secure; HttpOnly; SameSite=Lax") {
				t.Errorf("%s: split cookies = %q, want two with attributes", tc.path, cookies)
			}
		default:
			if len(cookies) != 0 {
				t.Errorf("%s: unexpected cookies = %q", tc.path, cookies)
			}
		}
	}
}
