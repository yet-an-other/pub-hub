package portal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yet-an-other/pub-hub/internal/portal"
)

// syncBuffer lets the test read the log while Run is still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type runningPortal struct {
	socket string
	logs   *syncBuffer
	stop   func() error
}

// start runs the Portal from a fresh portal.toml and waits for its socket.
func start(t *testing.T) runningPortal {
	t.Helper()
	return startWithIssuer(t, "https://zitadel.example.test")
}

func startWithIssuer(t *testing.T, issuer string) runningPortal {
	t.Helper()
	// Unix socket paths are limited to ~108 bytes, and t.TempDir can exceed that.
	dir, err := os.MkdirTemp("", "pubhub")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "portal.sock")
	configPath := filepath.Join(dir, "portal.toml")
	credentialsDir := filepath.Join(dir, "credentials")
	if err := os.Mkdir(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"hub-api-client-secret": "client-secret\n",
		"rgw-access-key-id":     "access-key\n",
		"rgw-secret-access-key": "secret-key\n",
	} {
		if err := os.WriteFile(filepath.Join(credentialsDir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentialsDir)
	config := fmt.Appendf(nil, "socket = %q\nzitadel_issuer_url = %q\nhub_api_client_id = \"hub-api-client\"\nzitadel_project_id = \"project-123\"\nzitadel_publisher_role = \"publisher\"\nzitadel_admin_role = \"hub-admin\"\ns3_endpoint = \"http://127.0.0.1:1\"\nartifact_bucket = \"pubhub-artifacts\"\nmetadata_bucket = \"pubhub-meta\"\npublic_base_url = \"https://pub.bdgn.me\"\nspool_directory = %q\n", socket, issuer, filepath.Join(dir, "spool"))
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- portal.Run(ctx, configPath, logs) }()

	stop := sync.OnceValue(func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("Run did not return after cancel")
		}
	})
	t.Cleanup(func() { _ = stop() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run returned early: %v\nlogs:\n%s", err, logs)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s never came up\nlogs:\n%s", socket, logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runningPortal{socket: socket, logs: logs, stop: stop}
}

func (r runningPortal) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", r.socket)
		},
	}}
}

func (r runningPortal) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := r.httpClient().Get("http://hub.bdgn.me" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (r runningPortal) getWithAuthorization(t *testing.T, path, token string) *http.Response {
	t.Helper()
	client := r.httpClient()
	request, err := http.NewRequest(http.MethodGet, "http://hub.bdgn.me"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHealthzAnswers200WhileUp(t *testing.T) {
	p := start(t)

	resp := p.get(t, "/healthz")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
}

func TestWhoamiReturnsTheZitadelPublisherName(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/v2/introspect" {
			t.Errorf("introspection path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if got := r.Form.Get("token"); got != "pat" {
			t.Errorf("introspection token = %q, want pat", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true,"sub":"user-123","name":"owner","urn:zitadel:iam:org:project:project-123:roles":{"publisher":{"org-456":"example.test"}}}`))
	}))
	defer idp.Close()
	p := startWithIssuer(t, idp.URL)

	resp := p.getWithAuthorization(t, "/api/whoami", "pat")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/whoami = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Label != "owner" {
		t.Errorf("label = %q, want owner", body.Label)
	}
}

func TestSocketIsReadableAndWritableByOwnerAndGroupOnly(t *testing.T) {
	p := start(t)

	info, err := os.Stat(p.socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Errorf("socket mode = %#o, want 0660", got)
	}
}

// logLines decodes every log line, failing the test on any that is not JSON.
func logLines(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		lines = append(lines, entry)
	}
	return lines
}

func TestLogsJSONNamingTheSocketItListensOn(t *testing.T) {
	p := start(t)
	if err := p.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := logLines(t, p.logs.String())
	for _, line := range lines {
		if line["socket"] == p.socket {
			return
		}
	}
	t.Errorf("no log line names socket %s:\n%s", p.socket, p.logs)
}

func TestRefusesToStartWithoutAValidConfig(t *testing.T) {
	cases := map[string]string{
		"missing": "",
		"invalid": `socket = "relative.sock"`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "portal.toml")
			if content != "" {
				if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			logs := &syncBuffer{}

			err := portal.Run(context.Background(), configPath, logs)

			if err == nil {
				t.Fatal("Run succeeded, want it to refuse to start")
			}
			lines := logLines(t, logs.String())
			last := lines[len(lines)-1]
			if last["level"] != "ERROR" || !strings.Contains(fmt.Sprint(last["error"]), configPath) {
				t.Errorf("last log line = %v, want an ERROR naming %s", last, configPath)
			}
		})
	}
}
