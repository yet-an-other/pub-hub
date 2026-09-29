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

func TestCreateOnlyBundle(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("index.html", "index.html")
	_, _ = io.WriteString(part, "<title>Original</title>")
	_ = writer.Close()
	publish := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "http://hub.bdgn.me/api/artifacts/xform/demo/", bytes.NewReader(body.Bytes()))
		request.Header.Set("Authorization", "Bearer test-pat")
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("If-None-Match", "*")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if first := publish(); first.Code != http.StatusCreated {
		t.Fatalf("create-only new Bundle = %d %s", first.Code, first.Body)
	}
	store.mu.Lock()
	writes := len(store.recordWriteOrder)
	store.mu.Unlock()
	if second := publish(); second.Code != http.StatusPreconditionFailed || !strings.Contains(second.Body.String(), `"code":"exists"`) {
		t.Errorf("create-only existing Bundle = %d %s", second.Code, second.Body)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.recordWriteOrder) != writes {
		t.Errorf("rejected Bundle caused writes: %v", store.recordWriteOrder)
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

func TestBundleDescriptionCanBeSetPreservedAndCleared(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	publish := func(description *string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if description != nil {
			_ = writer.WriteField("description", *description)
		}
		part, _ := writer.CreateFormFile("index.html", "index.html")
		_, _ = io.WriteString(part, "<title>Demo</title>")
		_ = writer.Close()
		return artifactRequest(t, handler, http.MethodPut, "/api/artifacts/xform/demo/", body.Bytes(), writer.FormDataContentType())
	}
	text, empty := "First description", ""
	if got := publish(&text); got.Code != 201 || decodeArtifact(t, got).Description != text {
		t.Fatalf("set description: %d %s", got.Code, got.Body)
	}
	if got := publish(nil); got.Code != 200 || decodeArtifact(t, got).Description != text {
		t.Errorf("preserve description: %d %s", got.Code, got.Body)
	}
	if got := publish(&empty); got.Code != 200 || decodeArtifact(t, got).Description != "" {
		t.Errorf("clear description: %d %s", got.Code, got.Body)
	}
	text = strings.Repeat("a", 1001)
	if got := publish(&text); got.Code != 400 || !strings.Contains(got.Body.String(), `"code":"request_invalid"`) {
		t.Errorf("long description: %d %s", got.Code, got.Body)
	}
}

func TestBundleRejectsFullObjectKeyOverflowWithoutChangingLiveArtifact(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	if response := bundleRequest(t, handler, map[string]string{"index.html": "old"}); response.Code != http.StatusCreated {
		t.Fatal(response.Body)
	}
	before, _ := store.record("xform/demo.json")
	// Each segment is valid, but the Artifact prefix pushes the S3 key over 1,024 bytes.
	name := strings.Repeat("x", 255) + "/" + strings.Repeat("y", 255) + "/" + strings.Repeat("z", 255) + "/" + strings.Repeat("w", 255)
	response := bundleRequest(t, handler, map[string]string{"index.html": "new", name: "bad"})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"path_invalid"`) {
		t.Errorf("oversized key: %d %s", response.Code, response.Body)
	}
	content, _, _ := store.object("xform/demo/index.html")
	if string(content) != "old" {
		t.Errorf("live entry changed: %q", content)
	}
	if after, _ := store.record("xform/demo.json"); after != before {
		t.Errorf("invalid key changed record: before=%+v after=%+v", before, after)
	}
}

func TestBundlePathValidationAndContentTypes(t *testing.T) {
	for _, test := range []struct {
		key   string
		valid bool
	}{
		{"assets/Café.PNG", true},
		{"LICENSE", true},
		{strings.Repeat("a", 255), true},
		{strings.Repeat("a", 256), false},
		{"", false},
		{"./index.html", false},
		{"a/../index.html", false},
		{"a//index.html", false},
		{"a/.hidden", false},
		{"a\\b", false},
		{"a/\x00b", false},
		{"a/\x7fb", false},
		{string([]byte{0xff}), false},
	} {
		if got := validBundleKey(test.key); got != test.valid {
			t.Errorf("validBundleKey(%q) = %t, want %t", test.key, got, test.valid)
		}
	}
	for key, want := range map[string]string{
		"INDEX.HTML": "text/html", "assets/Café.PNG": "image/png",
		"app.mjs": "text/javascript", "LICENSE": "application/octet-stream",
		"unknown.zzz": "application/octet-stream",
	} {
		if got := bundleContentType(key); got != want {
			t.Errorf("bundleContentType(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestBundleRequestLimitsAndMalformedBodyLeaveLiveArtifactUntouched(t *testing.T) {
	store := newMemoryArtifactStore()
	spool := t.TempDir()
	handler := newArtifactHandlerWithSpool(t, store, http.StatusOK, spool)
	if r := bundleRequest(t, handler, map[string]string{"index.html": "old"}); r.Code != 201 {
		t.Fatal(r.Body)
	}
	before, _ := store.record("xform/demo.json")

	oversized := httptest.NewRequest(http.MethodPut, "/api/artifacts/xform/demo/", strings.NewReader("x"))
	oversized.Header.Set("Content-Type", "multipart/form-data; boundary=unused")
	oversized.Header.Set("Authorization", "Bearer test-pat")
	oversized.ContentLength = maxPublishBytes + 1
	oversizedResult := httptest.NewRecorder()
	handler.ServeHTTP(oversizedResult, oversized)
	if oversizedResult.Code != 413 || !strings.Contains(oversizedResult.Body.String(), `"code":"too_large"`) {
		t.Errorf("oversized Bundle: %d %s", oversizedResult.Code, oversizedResult.Body)
	}

	malformed := artifactRequest(t, handler, http.MethodPut, "/api/artifacts/xform/demo/", []byte("--broken\r\nContent-Disposition: form-data; name=\"index.html\"; filename=\"index.html\"\r\n\r\nunfinished"), "multipart/form-data; boundary=broken")
	if malformed.Code != 400 || !strings.Contains(malformed.Body.String(), `"code":"request_invalid"`) {
		t.Errorf("malformed Bundle: %d %s", malformed.Code, malformed.Body)
	}
	content, _, ok := store.object("xform/demo/index.html")
	if !ok || string(content) != "old" {
		t.Errorf("live entry changed after rejected requests: %q", content)
	}
	if after, _ := store.record("xform/demo.json"); after != before {
		t.Errorf("rejected requests changed record: before=%+v after=%+v", before, after)
	}
}

func TestBundleSpoolIsEmptyAfterSuccessAndFailure(t *testing.T) {
	spool := t.TempDir()
	handler := newArtifactHandlerWithSpool(t, newMemoryArtifactStore(), http.StatusOK, spool)
	checkEmpty := func() {
		t.Helper()
		entries, err := os.ReadDir(spool)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("spool has %d entries after request", len(entries))
		}
	}
	if response := bundleRequest(t, handler, map[string]string{"index.html": "ok", "a.txt": "a"}); response.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", response.Code, response.Body)
	}
	checkEmpty()

	// Order the parts so validation fails only after a file has been spooled.
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, name := range []string{"index.html", ".secret"} {
		part, err := writer.CreateFormFile(name, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := artifactRequest(t, handler, http.MethodPut, "/api/artifacts/xform/demo/", body.Bytes(), writer.FormDataContentType())
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"path_invalid"`) {
		t.Fatalf("invalid Bundle = %d %s", response.Code, response.Body)
	}
	checkEmpty()
}

func TestInvalidBundleNeverTouchesLiveArtifact(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	if r := bundleRequest(t, handler, map[string]string{"index.html": "old"}); r.Code != 201 {
		t.Fatal(r.Body)
	}
	before, _ := store.record("xform/demo.json")
	// The validator's other malformed-path cases are covered in TestBundlePathValidationAndContentTypes.
	r := bundleRequest(t, handler, map[string]string{"index.html": "new", "a/.hidden": "bad"})
	if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), `"code":"path_invalid"`) {
		t.Errorf("dot-file: %d %s", r.Code, r.Body)
	}
	r = bundleRequest(t, handler, map[string]string{"other.txt": "no entry"})
	if r.Code != 422 || !strings.Contains(r.Body.String(), `"code":"index_missing"`) {
		t.Errorf("missing index: %d %s", r.Code, r.Body)
	}
	content, _, _ := store.object("xform/demo/index.html")
	if string(content) != "old" {
		t.Errorf("live entry changed: %q", content)
	}
	if after, _ := store.record("xform/demo.json"); after != before {
		t.Errorf("invalid Bundle changed record: before=%+v after=%+v", before, after)
	}
}
