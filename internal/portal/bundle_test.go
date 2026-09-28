package portal

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func bundleRequest(t *testing.T, handler http.Handler, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, content := range files {
		part, err := writer.CreateFormFile(name, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return artifactRequest(t, handler, http.MethodPut, "/api/artifacts/xform/demo/", body.Bytes(), writer.FormDataContentType())
}

func TestBundlePublishAndReplace(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	first := bundleRequest(t, handler, map[string]string{"index.html": "<title>Demo</title>", "app.js": "alert(1)", "images/logo.svg": "<svg/>"})
	if first.Code != 201 {
		t.Fatalf("publish: %d %s", first.Code, first.Body)
	}
	store.mu.Lock()
	order := append([]string(nil), store.recordWriteOrder...)
	keys := append([]string(nil), store.objectKeys...)
	store.mu.Unlock()
	if len(order) != 5 || order[0] != "record" || order[1] != "object" || order[2] != "object" || order[3] != "object" || order[4] != "record" {
		t.Errorf("publish order = %v", order)
	}
	if len(keys) != 3 || keys[2] != "xform/demo/index.html" {
		t.Errorf("entry was not uploaded last: %v", keys)
	}
	view := decodeArtifact(t, first)
	if view.Title != "Demo" || view.FileCount != 3 || view.TotalSize != int64(len("<title>Demo</title><svg/>alert(1)")) {
		t.Errorf("metadata: %+v", view)
	}
	for key, typ := range map[string]string{"index.html": "text/html", "app.js": "text/javascript", "images/logo.svg": "image/svg+xml"} {
		_, contentType, ok := store.object("xform/demo/" + key)
		if !ok || contentType != typ {
			t.Errorf("%s: exists=%t type=%s", key, ok, contentType)
		}
	}
	second := bundleRequest(t, handler, map[string]string{"index.html": "<title>New</title>", "LICENSE": "text"})
	if second.Code != 200 {
		t.Fatalf("replace: %d %s", second.Code, second.Body)
	}
	if decodeArtifact(t, second).CreatedAt != view.CreatedAt {
		t.Error("created timestamp changed")
	}
	for _, key := range []string{"app.js", "images/logo.svg"} {
		if _, _, ok := store.object("xform/demo/" + key); ok {
			t.Errorf("leftover %s", key)
		}
	}
	if _, typ, ok := store.object("xform/demo/LICENSE"); !ok || typ != "application/octet-stream" {
		t.Errorf("extensionless file missing or wrong type: %q", typ)
	}
}

func TestBundleRejectsDuplicateAndTooManyFiles(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	for _, count := range []int{2, 2001} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for i := 0; i < count; i++ {
			key := "index.html"
			if count == 2001 && i > 0 {
				key = strconv.Itoa(i)
			}
			part, err := writer.CreateFormFile(key, key)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(part, "x")
		}
		_ = writer.Close()
		response := artifactRequest(t, handler, http.MethodPut, "/api/artifacts/xform/demo/", body.Bytes(), writer.FormDataContentType())
		code, status := "path_invalid", 400
		if count == 2001 {
			code, status = "too_many_files", 413
		}
		if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("%d parts: %d %s", count, response.Code, response.Body)
		}
	}
}

func TestBundleSpoolIsEmptyAfterSuccessAndFailure(t *testing.T) {
	spool := t.TempDir()
	handler := newArtifactHandlerWithSpool(t, newMemoryArtifactStore(), http.StatusOK, spool)
	for _, files := range []map[string]string{{"index.html": "ok", "a.txt": "a"}, {"index.html": "bad", ".secret": "x"}} {
		_ = bundleRequest(t, handler, files)
		entries, err := os.ReadDir(spool)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("spool has %d entries after request", len(entries))
		}
	}
}

func TestInvalidBundleNeverTouchesLiveArtifact(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	if r := bundleRequest(t, handler, map[string]string{"index.html": "old"}); r.Code != 201 {
		t.Fatal(r.Body)
	}
	for _, key := range []string{".hidden", "a/.hidden", "a//b", "a/../b", "bad\\name", strings.Repeat("x", 256)} {
		r := bundleRequest(t, handler, map[string]string{"index.html": "new", key: "bad"})
		if r.Code != 400 || !strings.Contains(r.Body.String(), `"code":"path_invalid"`) {
			t.Errorf("%q: %d %s", key, r.Code, r.Body)
		}
	}
	r := bundleRequest(t, handler, map[string]string{"other.txt": "no entry"})
	if r.Code != 422 || !strings.Contains(r.Body.String(), `"code":"index_missing"`) {
		t.Errorf("missing index: %d %s", r.Code, r.Body)
	}
	content, _, _ := store.object("xform/demo/index.html")
	if string(content) != "old" {
		t.Errorf("live entry changed: %q", content)
	}
}
