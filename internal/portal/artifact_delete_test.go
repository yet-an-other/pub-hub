package portal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func publishDeleteFixture(t *testing.T, handler http.Handler, path string) {
	t.Helper()
	var result *httptest.ResponseRecorder
	if strings.HasSuffix(path, "/") {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for _, name := range []string{"index.html", "assets/site.css"} {
			part, err := writer.CreateFormFile(name, name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(part, name)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		result = artifactRequest(t, handler, http.MethodPut, "/api/artifacts/"+path, body.Bytes(), writer.FormDataContentType())
	} else {
		result = multipartRequest(t, handler, path, "plan.html", "<title>Plan</title>")
	}
	if result.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", result.Code, result.Body)
	}
}

func TestDeleteArtifactRemovesEntryBeforeFilesAndRecord(t *testing.T) {
	for _, tc := range []struct {
		path  string
		steps []string
	}{
		{"xform/plan.html", []string{"entry:xform/plan.html:incomplete", "record:xform/plan.json"}},
		{"xform/plan/", []string{"entry:xform/plan/index.html:incomplete", "leftovers:xform/plan/", "record:xform/plan.json"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			store := newMemoryArtifactStore()
			handler := newArtifactHandler(t, store)
			publishDeleteFixture(t, handler, tc.path)
			store.mu.Lock()
			store.deleteSteps = nil
			store.mu.Unlock()
			response := artifactRequest(t, handler, http.MethodDelete, "/api/artifacts/"+tc.path, nil, "")
			if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
				t.Fatalf("DELETE = %d %s", response.Code, response.Body)
			}
			store.mu.Lock()
			steps := append([]string(nil), store.deleteSteps...)
			objects, records := len(store.objects), len(store.records)
			store.mu.Unlock()
			if !reflect.DeepEqual(steps, tc.steps) || objects != 0 || records != 0 {
				t.Errorf("steps=%v objects=%d records=%d", steps, objects, records)
			}
			listed := artifactRequest(t, handler, http.MethodGet, "/api/artifacts", nil, "")
			var views []artifactView
			if err := json.Unmarshal(listed.Body.Bytes(), &views); err != nil || len(views) != 0 {
				t.Errorf("list = %s; err=%v", listed.Body, err)
			}
			if got := artifactRequest(t, handler, http.MethodGet, "/api/artifacts/"+tc.path, nil, ""); got.Code != http.StatusNotFound {
				t.Errorf("GET = %d", got.Code)
			}
			publishDeleteFixture(t, handler, tc.path)
		})
	}
}

func TestDeleteMissingRecordAndResumeIncompleteBundle(t *testing.T) {
	store := newMemoryArtifactStore()
	handler := newArtifactHandler(t, store)
	path := "/api/artifacts/xform/plan/"
	if missing := artifactRequest(t, handler, http.MethodDelete, path, nil, ""); missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"not_found"`) {
		t.Fatalf("missing = %d %s", missing.Code, missing.Body)
	}
	publishDeleteFixture(t, handler, "xform/plan/")
	store.leftoversError = errors.New("storage failed")
	if failed := artifactRequest(t, handler, http.MethodDelete, path, nil, ""); failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed = %d %s", failed.Code, failed.Body)
	}
	store.mu.Lock()
	store.leftoversError = nil
	store.mu.Unlock()
	if _, _, ok := store.object("xform/plan/index.html"); ok {
		t.Fatal("entry survived failed delete")
	}
	if record, ok := store.record("xform/plan.json"); !ok || record.State != "incomplete" {
		t.Fatalf("after failure = %+v %t", record, ok)
	}
	handler = newArtifactHandler(t, store) // new Portal loads the incomplete record
	if resumed := artifactRequest(t, handler, http.MethodDelete, path, nil, ""); resumed.Code != http.StatusNoContent {
		t.Fatalf("resume = %d %s", resumed.Code, resumed.Body)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.objects) != 0 || len(store.records) != 0 {
		t.Errorf("after resume: objects=%v records=%v", store.objects, store.records)
	}
}

func TestDeleteBlocksConcurrentMutation(t *testing.T) {
	store := newMemoryArtifactStore()
	var logs bytes.Buffer
	app := newApplication(store, "https://pub.bdgn.me", t.TempDir(), slog.New(slog.NewJSONHandler(&logs, nil)))
	handler := newHandlerForApplication(t, app, http.StatusOK)
	publishDeleteFixture(t, handler, "xform/plan.html")
	store.deleteStarted = make(chan struct{}, 1)
	store.blockDeletes = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- artifactRequest(t, handler, http.MethodDelete, "/api/artifacts/xform/plan.html", nil, "")
	}()
	select {
	case <-store.deleteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("delete did not start")
	}
	busy := multipartRequest(t, handler, "xform/plan.html", "plan.html", "<title>Other</title>")
	if busy.Code != http.StatusConflict || busy.Header().Get("Retry-After") != "5" || !strings.Contains(busy.Body.String(), `"code":"busy"`) {
		t.Errorf("concurrent publish = %d %s", busy.Code, busy.Body)
	}
	if concurrentDelete := artifactRequest(t, handler, http.MethodDelete, "/api/artifacts/xform/plan.html", nil, ""); concurrentDelete.Code != http.StatusConflict {
		t.Errorf("concurrent delete = %d %s", concurrentDelete.Code, concurrentDelete.Body)
	}
	if !strings.Contains(logs.String(), `"msg":"artifact mutation busy"`) || !strings.Contains(logs.String(), `"path":"xform/plan.html"`) {
		t.Errorf("busy log = %s", logs.String())
	}
	close(store.blockDeletes)
	if result := <-done; result.Code != http.StatusNoContent {
		t.Errorf("delete = %d %s", result.Code, result.Body)
	}
}

func TestDeleteLogsPublisherPathAndDuration(t *testing.T) {
	store := newMemoryArtifactStore()
	var logs bytes.Buffer
	app := newApplication(store, "https://pub.bdgn.me", t.TempDir(), slog.New(slog.NewJSONHandler(&logs, nil)))
	// Authentication stays at the existing HTTP test seam; capture the application's log.
	handler := newHandlerForApplication(t, app, http.StatusOK)
	publishDeleteFixture(t, handler, "xform/plan.html")
	response := artifactRequest(t, handler, http.MethodDelete, "/api/artifacts/xform/plan.html", nil, "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", response.Code, response.Body)
	}
	if !strings.Contains(logs.String(), `"msg":"artifact deleted"`) || !strings.Contains(logs.String(), `"path":"xform/plan.html"`) || !strings.Contains(logs.String(), `"publisher":"owner"`) || !strings.Contains(logs.String(), `"duration_ms":`) {
		t.Errorf("logs = %s", logs.String())
	}
}
