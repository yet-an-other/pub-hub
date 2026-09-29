package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func describedPublish(t *testing.T, h http.Handler, path string, description *string) int {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if description != nil {
		if err := writer.WriteField("description", *description); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("file", "plan.html")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "<title>Plan</title>"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := artifactRequest(t, h, http.MethodPut, "/api/artifacts/"+path, body.Bytes(), writer.FormDataContentType())
	if response.Code == http.StatusCreated || response.Code == http.StatusOK {
		return response.Code
	}
	return response.Code
}

func TestArtifactDescriptions(t *testing.T) {
	store := newMemoryArtifactStore()
	h := newArtifactHandler(t, store)
	path := "xform/plan.html"
	text := "Private note"
	empty := ""
	if code := describedPublish(t, h, path, &text); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if code := describedPublish(t, h, path, nil); code != 200 {
		t.Fatalf("replace: %d", code)
	}
	if record, _ := store.record("xform/plan.json"); record.Description != text {
		t.Fatalf("description lost: %+v", record)
	}
	if code := describedPublish(t, h, path, &empty); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if record, _ := store.record("xform/plan.json"); record.Description != "" {
		t.Fatalf("description not cleared: %+v", record)
	}
	if code := describedPublish(t, h, path, &text); code != 200 {
		t.Fatalf("reset: %d", code)
	}
	before, _ := store.record("xform/plan.json")
	bytesBefore, _, _ := store.object(path)
	response := artifactRequest(t, h, http.MethodPatch, "/api/artifacts/"+path, []byte(`{"description":"edited"}`), "application/json")
	if response.Code != 200 {
		t.Fatalf("PATCH: %d %s", response.Code, response.Body)
	}
	after, _ := store.record("xform/plan.json")
	bytesAfter, _, _ := store.object(path)
	if after.Description != "edited" || after.UpdatedAt != before.UpdatedAt || after.CreatedAt != before.CreatedAt || after.State != before.State || !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatalf("PATCH changed more than description: before=%+v after=%+v", before, after)
	}
	escaped := `{"description":"` + strings.Repeat(`\ud83d\ude00`, 1000) + `"}`
	valid := artifactRequest(t, h, http.MethodPatch, "/api/artifacts/"+path, []byte(escaped), "application/json; charset=utf-8")
	if valid.Code != 200 || decodeArtifact(t, valid).Description != strings.Repeat("😀", 1000) {
		t.Errorf("escaped 1000-character PATCH: %d %s", valid.Code, valid.Body)
	}
	for _, payload := range []string{`{}`, `{"description":null}`, `{"description":13}`, `{"description":"ok","extra":1}`, `{"description":"` + strings.Repeat("x", 1001) + `"}`} {
		r := artifactRequest(t, h, http.MethodPatch, "/api/artifacts/"+path, []byte(payload), "application/json")
		if r.Code != 400 || !strings.Contains(r.Body.String(), "request_invalid") {
			t.Errorf("invalid %q: %d %s", payload[:min(len(payload), 50)], r.Code, r.Body)
		}
	}
	if code := describedPublish(t, h, path, &[]string{strings.Repeat("x", 1001)}[0]); code != 400 {
		t.Errorf("long publish: %d", code)
	}
}

func TestLoadProjectRecord(t *testing.T) {
	store := newMemoryArtifactStore()
	// LoadRecords supplies the Project name from the metadata object's key.
	store.records["xform.json"] = json.RawMessage(`{"project":"xform","description":"Private"}`)
	h := newArtifactHandler(t, store)
	r := artifactRequest(t, h, http.MethodGet, "/api/projects", nil, "")
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"artifact_count":0`) || !strings.Contains(r.Body.String(), `"description":"Private"`) {
		t.Fatalf("loaded Project: %d %s", r.Code, r.Body)
	}
}

func TestProjectDescriptions(t *testing.T) {
	store := newMemoryArtifactStore()
	h := newArtifactHandler(t, store)
	patch := func(path, payload string) *string {
		r := artifactRequest(t, h, http.MethodPatch, "/api/projects/"+path, []byte(payload), "application/json")
		s := r.Body.String()
		if r.Code != 200 {
			t.Errorf("PATCH %s: %d %s", path, r.Code, s)
		}
		return &s
	}
	patch("xform", `{"description":"A private project"}`)
	if string(store.records["xform.json"]) != `{"description":"A private project"}` {
		t.Errorf("record: %s", store.records["xform.json"])
	}
	list := func() []struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		ArtifactCount int    `json:"artifact_count"`
	} {
		r := artifactRequest(t, h, http.MethodGet, "/api/projects", nil, "")
		if r.Code != 200 {
			t.Fatalf("list: %d %s", r.Code, r.Body)
		}
		var projects []struct {
			Name          string `json:"name"`
			Description   string `json:"description"`
			ArtifactCount int    `json:"artifact_count"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &projects); err != nil {
			t.Fatal(err)
		}
		return projects
	}
	if got := list(); len(got) != 1 || got[0].Name != "xform" || got[0].Description != "A private project" || got[0].ArtifactCount != 0 {
		t.Errorf("empty described project: %+v", got)
	}
	if code := describedPublish(t, h, "xform/plan.html", nil); code != 201 {
		t.Fatalf("publish: %d", code)
	}
	if got := list(); len(got) != 1 || got[0].ArtifactCount != 1 {
		t.Errorf("count: %+v", got)
	}
	patch("xform", `{"description":""}`)
	if _, ok := store.records["xform.json"]; ok {
		t.Error("cleared record remains")
	}
	if got := list(); len(got) != 1 || got[0].Description != "" {
		t.Errorf("project with Artifact: %+v", got)
	}
	if r := artifactRequest(t, h, http.MethodDelete, "/api/artifacts/xform/plan.html", nil, ""); r.Code != 204 {
		t.Fatalf("delete: %d", r.Code)
	}
	if got := list(); len(got) != 0 {
		t.Errorf("empty undescribed project: %+v", got)
	}
	for _, name := range []string{"index", "cdn-cgi", "bad_name", "bad/child"} {
		r := artifactRequest(t, h, http.MethodPatch, "/api/projects/"+name, []byte(`{"description":"ok"}`), "application/json")
		code := "name_invalid"
		if name == "index" || name == "cdn-cgi" {
			code = "name_reserved"
		}
		if r.Code != 400 || !strings.Contains(r.Body.String(), code) {
			t.Errorf("name %q: %d %s", name, r.Code, r.Body)
		}
	}
}

// heldRecordDeleteStore pauses after delete has claimed the Artifact and
// acquired the shared metadata-write lock, before either index changes.
type heldRecordDeleteStore struct {
	*memoryArtifactStore
	started chan struct{}
	release chan struct{}
}

func (s *heldRecordDeleteStore) DeleteRecord(ctx context.Context, key string) error {
	if key == "xform/plan.json" {
		close(s.started)
		<-s.release
	}
	return s.memoryArtifactStore.DeleteRecord(ctx, key)
}

func TestProjectSnapshotDuringArtifactDeleteAndProjectPatch(t *testing.T) {
	store := &heldRecordDeleteStore{memoryArtifactStore: newMemoryArtifactStore(), started: make(chan struct{}), release: make(chan struct{})}
	app := newApplication(store, "https://pub.bdgn.me", t.TempDir(), nil)
	h := newHandlerForApplication(t, app, http.StatusOK)
	if code := describedPublish(t, h, "xform/plan.html", nil); code != http.StatusCreated {
		t.Fatalf("publish: %d", code)
	}
	if r := artifactRequest(t, h, http.MethodPatch, "/api/projects/xform", []byte(`{"description":"Private"}`), "application/json"); r.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", r.Code, r.Body)
	}
	deleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { deleted <- artifactRequest(t, h, http.MethodDelete, "/api/artifacts/xform/plan.html", nil, "") }()
	select {
	case <-store.started:
	case <-time.After(3 * time.Second):
		close(store.release)
		t.Fatal("delete did not reach metadata store")
	}
	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		patched <- artifactRequest(t, h, http.MethodPatch, "/api/projects/xform", []byte(`{"description":"Updated"}`), "application/json")
	}()
	listed := make(chan *httptest.ResponseRecorder, 1)
	go func() { listed <- artifactRequest(t, h, http.MethodGet, "/api/projects", nil, "") }()
	select {
	case got := <-listed:
		close(store.release)
		t.Fatalf("list returned during metadata delete: %d %s", got.Code, got.Body)
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)
	if got := <-deleted; got.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", got.Code, got.Body)
	}
	got := <-listed
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"artifact_count":0`) || (!strings.Contains(got.Body.String(), `"description":"Private"`) && !strings.Contains(got.Body.String(), `"description":"Updated"`)) {
		t.Fatalf("list after deletion: %d %s", got.Code, got.Body)
	}
	if response := <-patched; response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"description":"Updated"`) || !strings.Contains(response.Body.String(), `"artifact_count":0`) {
		t.Fatalf("patch after deletion: %d %s", response.Code, response.Body)
	}
}
