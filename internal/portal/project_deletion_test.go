package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failingProjectStore struct {
	*memoryArtifactStore
	operation string
	failed    bool
}

func (s *failingProjectStore) fail(operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failed && s.operation == operation {
		s.failed = true
		return errors.New("injected storage failure")
	}
	return nil
}

func (s *failingProjectStore) PutRecord(ctx context.Context, key string, value []byte) error {
	var record artifactRecord
	_ = json.Unmarshal(value, &record)
	if record.State == "incomplete" {
		if err := s.fail("incomplete"); err != nil {
			return err
		}
	}
	return s.memoryArtifactStore.PutRecord(ctx, key, value)
}

func (s *failingProjectStore) DeleteArtifact(ctx context.Context, key string) error {
	if err := s.fail("entry"); err != nil {
		return err
	}
	return s.memoryArtifactStore.DeleteArtifact(ctx, key)
}

func (s *failingProjectStore) DeleteLeftovers(ctx context.Context, prefix string, keep map[string]struct{}) error {
	if err := s.fail("leftovers"); err != nil {
		return err
	}
	return s.memoryArtifactStore.DeleteLeftovers(ctx, prefix, keep)
}

func (s *failingProjectStore) DeleteRecord(ctx context.Context, key string) error {
	operation := "record"
	if !strings.Contains(key, "/") {
		operation = "description"
	}
	if err := s.fail(operation); err != nil {
		return err
	}
	return s.memoryArtifactStore.DeleteRecord(ctx, key)
}

func projectHandler(t *testing.T, store artifactStore) http.Handler {
	t.Helper()
	app := newApplication(store, "https://pub.bdgn.me", t.TempDir(), nil)
	return newHandlerForApplication(t, app, http.StatusOK)
}

func TestDeleteProjectFailureCanResumeAfterRestart(t *testing.T) {
	for _, operation := range []string{"incomplete", "entry", "leftovers", "record", "description"} {
		t.Run(operation, func(t *testing.T) {
			base := newMemoryArtifactStore()
			store := &failingProjectStore{memoryArtifactStore: base}
			handler := projectHandler(t, store)
			path := "xform/plan.html"
			if operation == "leftovers" {
				path = "xform/site/"
			}
			publishDeleteFixture(t, handler, path)
			if response := artifactRequest(t, handler, http.MethodPatch, "/api/projects/xform", []byte(`{"description":"Private"}`), "application/json"); response.Code != http.StatusOK {
				t.Fatalf("description = %d %s", response.Code, response.Body)
			}
			store.operation = operation
			failed := artifactRequest(t, handler, http.MethodDelete, "/api/projects/xform", nil, "")
			if failed.Code != http.StatusServiceUnavailable || !strings.Contains(failed.Body.String(), `"code":"storage_unavailable"`) {
				t.Fatalf("failure = %d %s", failed.Code, failed.Body)
			}
			if _, ok := base.records["xform.json"]; !ok {
				t.Fatal("Project description was removed before completion")
			}
			store.operation = ""
			handler = projectHandler(t, store)
			if response := artifactRequest(t, handler, http.MethodDelete, "/api/projects/xform", nil, ""); response.Code != http.StatusNoContent {
				t.Fatalf("retry after restart = %d %s", response.Code, response.Body)
			}
			base.mu.Lock()
			defer base.mu.Unlock()
			if len(base.objects) != 0 || len(base.records) != 0 {
				t.Errorf("remaining objects=%v records=%v", base.objects, base.records)
			}
		})
	}
}

func TestDeleteProjectWithoutDescriptionDoesNotDeleteDescriptionRecord(t *testing.T) {
	base := newMemoryArtifactStore()
	store := &failingProjectStore{memoryArtifactStore: base, operation: "description"}
	handler := projectHandler(t, store)
	publishDeleteFixture(t, handler, "xform/plan.html")

	response := artifactRequest(t, handler, http.MethodDelete, "/api/projects/xform", nil, "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete Project without description = %d %s", response.Code, response.Body)
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if len(base.records) != 0 || len(base.objects) != 0 {
		t.Errorf("remaining records=%v objects=%v", base.records, base.objects)
	}
}

func TestDeleteProjectBusyBeforeStorageAndBlocksNewMutations(t *testing.T) {
	store := newMemoryArtifactStore()
	store.blockDeletes = make(chan struct{})
	store.deleteStarted = make(chan struct{}, 1)
	handler := newArtifactHandler(t, store)
	publishDeleteFixture(t, handler, "xform/plan.html")
	publishDeleteFixture(t, handler, "other/keep.html")
	started := make(chan *httptest.ResponseRecorder, 1)
	go func() { started <- artifactRequest(t, handler, http.MethodDelete, "/api/projects/xform", nil, "") }()
	select {
	case <-store.deleteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Project delete did not begin")
	}
	for _, request := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder {
			return multipartRequest(t, handler, "xform/new.html", "new.html", "new")
		},
		func() *httptest.ResponseRecorder {
			return artifactRequest(t, handler, http.MethodPatch, "/api/projects/xform", []byte(`{"description":"late"}`), "application/json")
		},
		func() *httptest.ResponseRecorder {
			return artifactRequest(t, handler, http.MethodDelete, "/api/projects/xform", nil, "")
		},
	} {
		response := request()
		if response.Code != http.StatusConflict || response.Header().Get("Retry-After") != "5" || !strings.Contains(response.Body.String(), `"code":"busy"`) {
			t.Errorf("concurrent mutation = %d %s", response.Code, response.Body)
		}
	}
	if response := multipartRequest(t, handler, "other/new.html", "new.html", "other"); response.Code != http.StatusCreated {
		t.Errorf("other Project publish = %d %s", response.Code, response.Body)
	}
	close(store.blockDeletes)
	if response := <-started; response.Code != http.StatusNoContent {
		t.Fatalf("Project delete = %d %s", response.Code, response.Body)
	}
}

type blockedBody struct {
	reader  *bytes.Reader
	started chan struct{}
	release chan struct{}
	once    bool
}

func (b *blockedBody) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.started)
		<-b.release
	}
	return b.reader.Read(p)
}
func (b *blockedBody) Close() error { return nil }

func TestDeleteProjectBusyBeforeStagedUploadHasRecord(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	body, contentType := multipartFile(t, "staged.html", "body")
	blocked := &blockedBody{reader: bytes.NewReader(body), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPut, "https://hub.bdgn.me/api/artifacts/xform/staged.html", blocked)
		request.Header.Set("Authorization", "Bearer test-pat")
		request.Header.Set("Content-Type", contentType)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		done <- response
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not enter staging")
	}
	response := artifactRequest(t, handler, http.MethodDelete, "/api/projects/xform", nil, "")
	if response.Code != http.StatusConflict || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("Project delete during staging = %d %s", response.Code, response.Body)
	}
	close(blocked.release)
	if published := <-done; published.Code != http.StatusCreated {
		t.Fatalf("staged publish = %d %s", published.Code, published.Body)
	}
}

func TestDeleteProjectBusyForStagedPublish(t *testing.T) {
	store := newMemoryArtifactStore()
	store.blockObjectWrites = true
	store.objectWriteStarted = make(chan struct{}, 1)
	store.releaseObjectWrites = make(chan struct{})
	handler := newArtifactHandler(t, store)
	publishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { publishDone <- multipartRequest(t, handler, "xform/staged.html", "staged.html", "body") }()
	select {
	case <-store.objectWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("staged publish did not reach object write")
	}
	response := artifactRequest(t, handler, http.MethodDelete, "/api/projects/xform", nil, "")
	if response.Code != http.StatusConflict || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("Project delete = %d %s", response.Code, response.Body)
	}
	close(store.releaseObjectWrites)
	if published := <-publishDone; published.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", published.Code, published.Body)
	}
}

func TestDeleteProjectSupportsBothAPIAuthenticationPrefixesAndValidation(t *testing.T) {
	for _, prefix := range []string{"/api", "/ui/api"} {
		store := newMemoryArtifactStore()
		handler := newArtifactHandler(t, store)
		publishDeleteFixture(t, handler, "xform/a.html")
		request := httptest.NewRequest(http.MethodDelete, "https://hub.bdgn.me"+prefix+"/projects/xform", nil)
		if prefix == "/api" {
			request.Header.Set("Authorization", "Bearer test-pat")
		} else {
			request.Header.Set("X-Auth-Request-Email", "owner@example.test")
			request.Header.Set("Origin", "https://hub.bdgn.me")
			request.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Errorf("%s delete = %d %s", prefix, response.Code, response.Body)
		}
	}
	for _, name := range []string{"bad_name", "index", "cdn-cgi"} {
		response := artifactRequest(t, newArtifactHandler(t, newMemoryArtifactStore()), http.MethodDelete, "/api/projects/"+name, nil, "")
		want := "name_invalid"
		if name == "index" || name == "cdn-cgi" {
			want = "name_reserved"
		}
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+want+`"`) {
			t.Errorf("%s = %d %s", name, response.Code, response.Body)
		}
	}
}
