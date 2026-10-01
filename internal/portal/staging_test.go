package portal

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestArtifactStagingThroughPublish(t *testing.T) {
	for _, shape := range []struct {
		name, path, entry, object string
	}{
		{"HTML file", "/api/artifacts/xform/demo.html", "demo.html", "xform/demo.html"},
		{"Bundle", "/api/artifacts/xform/demo/", "index.html", "xform/demo/index.html"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			store := newMemoryArtifactStore()
			spool := t.TempDir()
			handler := newArtifactHandlerWithSpool(t, store, http.StatusOK, spool)
			request := func(fields ...string) (int, string) {
				t.Helper()
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				part, err := writer.CreateFormFile(shape.entry, shape.entry)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(part, "<title>Demo</title>"); err != nil {
					t.Fatal(err)
				}
				for _, field := range fields {
					if err := writer.WriteField("description", field); err != nil {
						t.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				response := artifactRequest(t, handler, http.MethodPut, shape.path, body.Bytes(), writer.FormDataContentType())
				entries, err := os.ReadDir(spool)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Errorf("spool has %d entries after publish", len(entries))
				}
				return response.Code, response.Body.String()
			}

			status, body := request("first")
			if status != http.StatusCreated || !strings.Contains(body, `"description":"first"`) || !strings.Contains(body, `"title":"Demo"`) {
				t.Fatalf("publish = %d %s", status, body)
			}
			before, _ := store.record("xform/demo.json")
			for _, fields := range [][]string{{"first", "second"}, {strings.Repeat("x", 1001)}} {
				status, body = request(fields...)
				if status != http.StatusBadRequest || !strings.Contains(body, `"code":"request_invalid"`) {
					t.Errorf("invalid description = %d %s", status, body)
				}
				if after, _ := store.record("xform/demo.json"); after != before {
					t.Errorf("invalid description changed record: before=%+v after=%+v", before, after)
				}
				if content, _, ok := store.object(shape.object); !ok || string(content) != "<title>Demo</title>" {
					t.Errorf("invalid description changed live Artifact: %q", content)
				}
			}
			store.objectWriteError = errors.New("storage offline")
			status, body = request("retry")
			if status != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"storage_unavailable"`) {
				t.Errorf("storage failure = %d %s", status, body)
			}
		})
	}
}
