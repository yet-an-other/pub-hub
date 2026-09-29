package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yet-an-other/pub-hub/internal/auth"
)

type memoryArtifactStore struct {
	mu                    sync.Mutex
	records               map[string]json.RawMessage
	objects               map[string][]byte
	contentTypes          map[string]string
	recordWriteOrder      []string
	objectKeys            []string
	loadRecordsError      error
	incompleteBeforeWrite bool
	objectWriteError      error
	artifactBucketError   error
	metadataBucketError   error
	slowBucketChecks      bool
	blockObjectWrites     bool
	objectWriteStarted    chan struct{}
	releaseObjectWrites   chan struct{}
}

func newMemoryArtifactStore() *memoryArtifactStore {
	return &memoryArtifactStore{
		records:      make(map[string]json.RawMessage),
		objects:      make(map[string][]byte),
		contentTypes: make(map[string]string),
	}
}

func (s *memoryArtifactStore) LoadRecords(context.Context) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadRecordsError != nil {
		return nil, s.loadRecordsError
	}
	out := make([]json.RawMessage, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, append(json.RawMessage(nil), record...))
	}
	return out, nil
}

func (s *memoryArtifactStore) PutRecord(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[key] = append(json.RawMessage(nil), value...)
	s.recordWriteOrder = append(s.recordWriteOrder, "record")
	return nil
}

func (s *memoryArtifactStore) PutArtifact(_ context.Context, key string, body io.Reader, _ int64, contentType string) error {
	content, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	record := s.records[strings.TrimSuffix(key, ".html")+".json"]
	var metadata artifactRecord
	if len(record) > 0 {
		_ = json.Unmarshal(record, &metadata)
	}
	s.incompleteBeforeWrite = metadata.State == "incomplete"
	s.recordWriteOrder = append(s.recordWriteOrder, "object")
	s.objectKeys = append(s.objectKeys, key)
	started := s.objectWriteStarted
	release := s.releaseObjectWrites
	block := s.blockObjectWrites
	writeErr := s.objectWriteError
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block {
		<-release
	}
	if writeErr != nil {
		return writeErr
	}
	s.mu.Lock()
	s.objects[key] = content
	s.contentTypes[key] = contentType
	s.mu.Unlock()
	return nil
}

func (s *memoryArtifactStore) DeleteLeftovers(_ context.Context, prefix string, keep map[string]struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			if _, ok := keep[key]; !ok {
				delete(s.objects, key)
				delete(s.contentTypes, key)
			}
		}
	}
	return nil
}

func (s *memoryArtifactStore) CheckBuckets(ctx context.Context) (error, error) {
	s.mu.Lock()
	artifactErr, metadataErr := s.artifactBucketError, s.metadataBucketError
	slow := s.slowBucketChecks
	s.mu.Unlock()
	if slow {
		<-ctx.Done()
		return ctx.Err(), ctx.Err()
	}
	return artifactErr, metadataErr
}

func (s *memoryArtifactStore) record(key string) (artifactRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.records[key]
	var record artifactRecord
	if ok {
		_ = json.Unmarshal(data, &record)
	}
	return record, ok
}

func (s *memoryArtifactStore) object(key string) ([]byte, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.objects[key]
	return append([]byte(nil), content...), s.contentTypes[key], ok
}

func newArtifactHandler(t *testing.T, store *memoryArtifactStore) http.Handler {
	return newArtifactHandlerWithDiscoveryStatus(t, store, http.StatusOK)
}

func newArtifactHandlerWithDiscoveryStatus(t *testing.T, store *memoryArtifactStore, discoveryStatus int) http.Handler {
	return newArtifactHandlerWithSpool(t, store, discoveryStatus, t.TempDir())
}

func newArtifactHandlerWithSpool(t *testing.T, store *memoryArtifactStore, discoveryStatus int, spool string) http.Handler {
	t.Helper()
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v2/introspect":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"active":true,"sub":"user-123"}`)
		case "/.well-known/openid-configuration":
			w.WriteHeader(discoveryStatus)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	authenticator, err := auth.NewAuthenticator(idp.URL, "hub-api", "secret", map[string]string{"user-123": "owner"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	app := newApplication(store, "https://pub.bdgn.me", spool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := app.prepareSpool(); err != nil {
		t.Fatalf("prepareSpool: %v", err)
	}
	return routes(authenticator, app)
}

func multipartFile(t *testing.T, fileName, contents string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func artifactRequest(t *testing.T, handler http.Handler, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://hub.bdgn.me"+path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-pat")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func multipartRequest(t *testing.T, handler http.Handler, artifactPath, fileName, contents string) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartFile(t, fileName, contents)
	return artifactRequest(t, handler, http.MethodPut, "/api/artifacts/"+artifactPath, body, contentType)
}

func decodeArtifact(t *testing.T, response *httptest.ResponseRecorder) artifactView {
	t.Helper()
	var view artifactView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode artifact response: %v; body=%s", err, response.Body)
	}
	return view
}

func TestSingleFilePublishReadAndReplace(t *testing.T) {
	store := newMemoryArtifactStore()
	baseTime := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	app := newApplication(store, "https://pub.bdgn.me", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := app.prepareSpool(); err != nil {
		t.Fatal(err)
	}
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/introspect" {
			_, _ = io.WriteString(w, `{"active":true,"sub":"user-123"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer idp.Close()
	authenticator, err := auth.NewAuthenticator(idp.URL, "hub-api", "secret", map[string]string{"user-123": "owner"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler := routes(authenticator, app)
	app.now = func() time.Time {
		baseTime = baseTime.Add(time.Second)
		return baseTime
	}

	created := multipartRequest(t, handler, "xform/notes/plan.html", "plan.html", "<html><head><title> Plan &amp; Notes </title></head><body>first</body></html>")
	if created.Code != http.StatusCreated {
		t.Fatalf("PUT create status = %d, want 201; body=%s", created.Code, created.Body)
	}
	first := decodeArtifact(t, created)
	if first.Path != "xform/notes/plan.html" || first.URL != "https://pub.bdgn.me/xform/notes/plan.html" || first.Title != "Plan & Notes" || first.State != "published" || first.LastPublisher != "owner" || first.FileCount != 1 || first.TotalSize != int64(len("<html><head><title> Plan &amp; Notes </title></head><body>first</body></html>")) {
		t.Errorf("created metadata = %+v", first)
	}
	if first.CreatedAt.IsZero() || first.CreatedAt != first.UpdatedAt {
		t.Errorf("create timestamps = created %s, updated %s", first.CreatedAt, first.UpdatedAt)
	}
	content, contentType, exists := store.object("xform/notes/plan.html")
	if !exists || string(content) != "<html><head><title> Plan &amp; Notes </title></head><body>first</body></html>" || contentType != "text/html" {
		t.Errorf("stored object = (%q, %q, exists %t)", content, contentType, exists)
	}
	store.mu.Lock()
	order := append([]string(nil), store.recordWriteOrder...)
	incompleteBeforeWrite := store.incompleteBeforeWrite
	store.mu.Unlock()
	if len(order) != 3 || order[0] != "record" || order[1] != "object" || order[2] != "record" || !incompleteBeforeWrite {
		t.Errorf("publish order = %v, incomplete before object write = %t", order, incompleteBeforeWrite)
	}

	got := artifactRequest(t, handler, http.MethodGet, "/api/artifacts/xform/notes/plan.html", nil, "")
	if got.Code != http.StatusOK || decodeArtifact(t, got) != first {
		t.Errorf("GET metadata = %d %+v, want %+v", got.Code, decodeArtifact(t, got), first)
	}
	listed := artifactRequest(t, handler, http.MethodGet, "/api/artifacts?prefix=xform/notes", nil, "")
	var list []artifactView
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, listed.Body)
	}
	if listed.Code != http.StatusOK || len(list) != 1 || list[0] != first {
		t.Errorf("prefix list = %d %+v, want one result %+v", listed.Code, list, first)
	}

	replaced := multipartRequest(t, handler, "xform/notes/plan.html", "renamed.html", "<html><title>   </title><body>second</body></html>")
	if replaced.Code != http.StatusOK {
		t.Fatalf("PUT replace status = %d, want 200; body=%s", replaced.Code, replaced.Body)
	}
	second := decodeArtifact(t, replaced)
	if second.CreatedAt != first.CreatedAt || second.UpdatedAt.Equal(first.UpdatedAt) || second.Title != "plan" || second.State != "published" {
		t.Errorf("replacement metadata = %+v, original = %+v", second, first)
	}
}

func TestArtifactNamesAndSuffixesAreRejectedBeforeStorageWrites(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	for _, test := range []struct {
		path   string
		status int
		code   string
	}{
		{"xform/notes/plan", http.StatusNotFound, "not_found"},
		{"xform/notes/bad_name.html", http.StatusBadRequest, "name_invalid"},
		{"xform/notes/index.html", http.StatusBadRequest, "name_reserved"},
		{"xform//notes/plan.html", http.StatusBadRequest, "name_invalid"},
		{"xform%2Fnotes%2Fencoded.html", http.StatusBadRequest, "name_invalid"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := multipartRequest(t, handler, test.path, "file.html", "<title>Nothing</title>")
			if response.Code != test.status {
				t.Fatalf("PUT status = %d, want %d; body=%s", response.Code, test.status, response.Body)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != test.code {
				t.Errorf("error body = %s, decode err %v; want code %q", response.Body, err, test.code)
			}
		})
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) != 0 || len(store.objects) != 0 {
		t.Errorf("invalid paths caused storage writes: %d records, %d objects", len(store.records), len(store.objects))
	}
}

func TestCreateOnlyRejectsExistingAndIncompleteArtifactsWithoutWrites(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%t", incomplete), func(t *testing.T) {
			store := newMemoryArtifactStore()
			handler := newArtifactHandler(t, store)
			first := multipartRequest(t, handler, "xform/plan.html", "plan.html", "<title>First</title>")
			if first.Code != http.StatusCreated {
				t.Fatalf("initial publish = %d %s", first.Code, first.Body)
			}
			if incomplete {
				store.mu.Lock()
				var record artifactRecord
				_ = json.Unmarshal(store.records["xform/plan.json"], &record)
				record.State = "incomplete"
				store.records["xform/plan.json"], _ = json.Marshal(record)
				store.mu.Unlock()
				// Reload the incomplete record as a restarted Portal would.
				handler = newArtifactHandler(t, store)
			}
			store.mu.Lock()
			writes := len(store.recordWriteOrder)
			store.mu.Unlock()
			body, contentType := multipartFile(t, "plan.html", "<title>Second</title>")
			request := httptest.NewRequest(http.MethodPut, "http://hub.bdgn.me/api/artifacts/xform/plan.html", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer test-pat")
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("If-None-Match", "*")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusPreconditionFailed || !strings.Contains(response.Body.String(), `"code":"exists"`) {
				t.Errorf("create-only replacement = %d %s, want 412 exists", response.Code, response.Body)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.recordWriteOrder) != writes || string(store.objects["xform/plan.html"]) != "<title>First</title>" {
				t.Errorf("rejected publish changed storage: writes=%v objects=%v", store.recordWriteOrder, store.objects)
			}
		})
	}
}

func TestPathConflictsLeaveRecordsAndBytesUntouched(t *testing.T) {
	bundle := func(handler http.Handler, path string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("index.html", "index.html")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(part, "<title>Bundle</title>")
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return artifactRequest(t, handler, http.MethodPut, "/api/artifacts/"+path, body.Bytes(), writer.FormDataContentType())
	}
	for _, tc := range []struct {
		name, first, second, code string
	}{
		{"file above file", "a/b.html", "a/b/c.html", "nesting_conflict"},
		{"bundle above file", "a/b/", "a/b/c.html", "nesting_conflict"},
		{"file below file", "a/b/c.html", "a/b.html", "nesting_conflict"},
		{"file below bundle", "a/b/c.html", "a/b/", "nesting_conflict"},
		{"file before bundle", "a/plan.html", "a/plan/", "shape_conflict"},
		{"bundle before file", "a/plan/", "a/plan.html", "shape_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryArtifactStore()
			handler := newArtifactHandler(t, store)
			publish := func(path string) *httptest.ResponseRecorder {
				if strings.HasSuffix(path, "/") {
					return bundle(handler, path)
				}
				return multipartRequest(t, handler, path, "file.html", "<title>File</title>")
			}
			if first := publish(tc.first); first.Code != http.StatusCreated {
				t.Fatalf("initial PUT = %d %s", first.Code, first.Body)
			}
			store.mu.Lock()
			writes := len(store.recordWriteOrder)
			store.mu.Unlock()
			second := publish(tc.second)
			if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("conflicting PUT = %d %s, want 409 %s", second.Code, second.Body, tc.code)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.recordWriteOrder) != writes || len(store.records) != 1 || len(store.objects) != 1 {
				t.Errorf("conflict touched storage: order=%v records=%v objects=%v", store.recordWriteOrder, store.records, store.objects)
			}
		})
	}
}

func TestConcurrentPublishesCannotNest(t *testing.T) {
	store := newMemoryArtifactStore()
	store.blockObjectWrites = true
	store.objectWriteStarted = make(chan struct{}, 1)
	store.releaseObjectWrites = make(chan struct{})
	handler := newArtifactHandler(t, store)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- multipartRequest(t, handler, "a/b.html", "b.html", "<title>Parent</title>")
	}()
	select {
	case <-store.objectWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not reach object storage")
	}
	child := multipartRequest(t, handler, "a/b/c.html", "c.html", "<title>Child</title>")
	if child.Code != http.StatusConflict || !strings.Contains(child.Body.String(), `"code":"nesting_conflict"`) {
		t.Errorf("child = %d %s, want nesting_conflict", child.Code, child.Body)
	}
	close(store.releaseObjectWrites)
	if first := <-firstDone; first.Code != http.StatusCreated {
		t.Errorf("parent = %d %s", first.Code, first.Body)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) != 1 || len(store.objects) != 1 {
		t.Errorf("rejected child touched storage: records=%v objects=%v", store.records, store.objects)
	}
}

func TestSingleFileUploadRequiresExactlyOneFile(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("one", "one.html")
	_, _ = io.WriteString(part, "<title>one</title>")
	part, _ = writer.CreateFormFile("two", "two.html")
	_, _ = io.WriteString(part, "<title>two</title>")
	_ = writer.Close()
	response := artifactRequest(t, handler, http.MethodPut, "/api/artifacts/xform/plan.html", body.Bytes(), writer.FormDataContentType())
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"code":"file_count"`) {
		t.Errorf("multiple-file publish = %d %s, want 422 file_count", response.Code, response.Body)
	}

	body.Reset()
	writer = multipart.NewWriter(&body)
	if err := writer.WriteField("description", "not a file"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	response = artifactRequest(t, handler, http.MethodPut, "/api/artifacts/xform/empty.html", body.Bytes(), writer.FormDataContentType())
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"code":"file_count"`) {
		t.Errorf("zero-file publish = %d %s, want 422 file_count", response.Code, response.Body)
	}
}

func TestPublishRequestSizeLimit(t *testing.T) {
	handler := newArtifactHandler(t, newMemoryArtifactStore())
	request := httptest.NewRequest(http.MethodPut, "http://hub.bdgn.me/api/artifacts/xform/plan.html", strings.NewReader("small"))
	request.ContentLength = maxPublishBytes + 1
	request.Header.Set("Authorization", "Bearer test-pat")
	request.Header.Set("Content-Type", "multipart/form-data; boundary=unused")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"too_large"`) {
		t.Errorf("oversized publish = %d %s, want 413 too_large", response.Code, response.Body)
	}
}

func TestFailedByteUploadLeavesAnIncompleteRecordVisible(t *testing.T) {
	store := newMemoryArtifactStore()
	store.objectWriteError = errors.New("disk unavailable")
	handler := newArtifactHandler(t, store)
	response := multipartRequest(t, handler, "xform/plan.html", "plan.html", "<title>Plan</title>")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"storage_unavailable"`) {
		t.Fatalf("PUT with failed object write = %d %s, want 503 storage_unavailable", response.Code, response.Body)
	}
	record, exists := store.record("xform/plan.json")
	if !exists || record.State != "incomplete" {
		t.Fatalf("record after failed publish = %+v, exists %t; want incomplete", record, exists)
	}
	got := artifactRequest(t, handler, http.MethodGet, "/api/artifacts/xform/plan.html", nil, "")
	if got.Code != http.StatusOK || decodeArtifact(t, got).State != "incomplete" {
		t.Errorf("GET incomplete record = %d %s", got.Code, got.Body)
	}
	list := artifactRequest(t, handler, http.MethodGet, "/api/artifacts", nil, "")
	var artifacts []artifactView
	if err := json.Unmarshal(list.Body.Bytes(), &artifacts); err != nil || list.Code != http.StatusOK || len(artifacts) != 1 || artifacts[0].State != "incomplete" {
		t.Errorf("list after failed publish = %d %s; decode err %v", list.Code, list.Body, err)
	}
}

func TestConcurrentMutationOfSameArtifactGetsBusy(t *testing.T) {
	store := newMemoryArtifactStore()
	store.blockObjectWrites = true
	store.objectWriteStarted = make(chan struct{}, 1)
	store.releaseObjectWrites = make(chan struct{})
	handler := newArtifactHandler(t, store)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- multipartRequest(t, handler, "xform/plan.html", "plan.html", "<title>Plan</title>")
	}()
	select {
	case <-store.objectWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first publish did not reach object storage")
	}
	second := multipartRequest(t, handler, "xform/plan.html", "plan.html", "<title>Replacement</title>")
	if second.Code != http.StatusConflict || second.Header().Get("Retry-After") != "5" || !strings.Contains(second.Body.String(), `"code":"busy"`) {
		t.Errorf("concurrent PUT = %d headers=%v body=%s, want 409 busy and Retry-After: 5", second.Code, second.Header(), second.Body)
	}
	close(store.releaseObjectWrites)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusCreated {
			t.Errorf("first publish = %d %s, want 201", first.Code, first.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first publish did not finish")
	}
}

func TestDifferentArtifactsPublishInParallel(t *testing.T) {
	store := newMemoryArtifactStore()
	store.blockObjectWrites = true
	store.objectWriteStarted = make(chan struct{}, 2)
	store.releaseObjectWrites = make(chan struct{})
	handler := newArtifactHandler(t, store)
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- multipartRequest(t, handler, "xform/one.html", "one.html", "<title>One</title>") }()
	select {
	case <-store.objectWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first publish did not reach object storage")
	}
	go func() { results <- multipartRequest(t, handler, "xform/two.html", "two.html", "<title>Two</title>") }()
	select {
	case <-store.objectWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second Artifact did not reach object storage while first was blocked")
	}
	close(store.releaseObjectWrites)
	for range 2 {
		select {
		case response := <-results:
			if response.Code != http.StatusCreated {
				t.Errorf("parallel publish = %d %s, want 201", response.Code, response.Body)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("parallel publish did not finish")
		}
	}
}

func TestReadyzDoesNotFailWhenIDPIsUnreachable(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandlerWithDiscoveryStatus(t, store, http.StatusServiceUnavailable)
	response := artifactRequest(t, handler, http.MethodGet, "/readyz", nil, "")
	var body readinessResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Zitadel.Reachable {
		t.Errorf("readyz with unavailable IDP = %d %+v, want 200 and reachable=false", response.Code, body)
	}
}

func TestReadyzIDPProbeIsIndependentFromSlowStorageChecks(t *testing.T) {
	store := newMemoryArtifactStore()
	store.slowBucketChecks = true
	handler := newArtifactHandler(t, store)
	response := artifactRequest(t, handler, http.MethodGet, "/readyz", nil, "")
	var body readinessResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || body.Zitadel.Reachable == false {
		t.Errorf("readyz after slow bucket check = %d %+v, want 503 and reachable IDP", response.Code, body)
	}
}

func TestReadyzReturns200WhenBucketsAreReachableButRecordLoadingFails(t *testing.T) {
	store := newMemoryArtifactStore()
	store.loadRecordsError = errors.New("metadata could not be decoded")
	handler := newArtifactHandler(t, store)
	response := artifactRequest(t, handler, http.MethodGet, "/readyz", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("readyz = %d %s, want 200 because both buckets are reachable", response.Code, response.Body)
	}
}

func TestReadyzFailsOnlyForStorageAndReportsIDPReachability(t *testing.T) {
	store := newMemoryArtifactStore()
	store.metadataBucketError = errors.New("metadata bucket unavailable")
	handler := newArtifactHandler(t, store)
	response := artifactRequest(t, handler, http.MethodGet, "/readyz", nil, "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", response.Code)
	}
	var body readinessResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "not_ready" || !body.Storage.Artifacts || body.Storage.Metadata || !body.Zitadel.Reachable {
		t.Errorf("readyz body = %+v", body)
	}
	store.mu.Lock()
	store.metadataBucketError = nil
	store.mu.Unlock()
	response = artifactRequest(t, handler, http.MethodGet, "/readyz", nil, "")
	if response.Code != http.StatusOK {
		t.Errorf("readyz when storage recovers = %d %s, want 200", response.Code, response.Body)
	}
}

func TestListPrefixIsValidated(t *testing.T) {
	handler := newArtifactHandler(t, newMemoryArtifactStore())
	response := artifactRequest(t, handler, http.MethodGet, "/api/artifacts?prefix=xform/index", nil, "")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"name_reserved"`) {
		t.Errorf("invalid prefix response = %d %s, want 400 name_reserved", response.Code, response.Body)
	}
}
