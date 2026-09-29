package portal

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"

	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/naming"
)

// descriptionInput requires a single JSON string field, rather than silently
// accepting missing fields, null, or unknown edits.
func descriptionInput(r *http.Request) (string, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", errors.New("expected JSON")
	}
	var fields map[string]json.RawMessage
	// Escaped characters can be twelve bytes each in JSON even though the
	// decoded description is limited to 1,000 characters.
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := decoder.Decode(&fields); err != nil || len(fields) != 1 || fields["description"] == nil {
		return "", errors.New("invalid description request")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errors.New("trailing JSON")
	}
	var value string
	if err := json.Unmarshal(fields["description"], &value); err != nil || string(fields["description"]) == "null" || !validDescription(value) {
		return "", errors.New("invalid description")
	}
	return value, nil
}

func writeDescriptionError(w http.ResponseWriter) {
	auth.WriteError(w, http.StatusBadRequest, "request_invalid", "Invalid description request")
}

func (a *application) patchArtifact(w http.ResponseWriter, r *http.Request, path naming.ArtifactPath) {
	value, err := descriptionInput(r)
	if err != nil {
		writeDescriptionError(w)
		return
	}
	publisher, ok := auth.PublisherFromContext(r.Context())
	if !ok {
		auth.WriteError(w, 500, "internal_error", "publisher identity unavailable")
		return
	}
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	stem := path.Stem()
	if !a.claim(stem) {
		w.Header().Set("Retry-After", "5")
		auth.WriteError(w, 409, "busy", "Artifact mutation already in progress")
		return
	}
	defer a.release(stem)
	a.mu.RLock()
	record, exists := a.records[stem]
	a.mu.RUnlock()
	if !exists || record.Path != path.PublicPath() {
		auth.WriteError(w, 404, "not_found", "Artifact not found")
		return
	}
	record.Description = value
	// Keep the existing state, timestamps, bytes, and publisher.
	if err := a.putRecord(r.Context(), stem, record, false); err != nil {
		a.storageUnavailable(w, "edit Artifact description", err)
		return
	}
	a.log.Info("artifact description edited", "path", record.Path, "publisher", publisher.Label)
	writeJSON(w, 200, a.view(record))
}

func (a *application) listProjects(w http.ResponseWriter, r *http.Request) {
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	a.mu.RLock()
	projects := a.projectViewsLocked()
	a.mu.RUnlock()
	result := make([]projectView, 0, len(projects))
	for _, project := range projects {
		result = append(result, project)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	writeJSON(w, 200, result)
}

// projectViewsLocked reads both metadata indexes under a.mu.
func (a *application) projectViewsLocked() map[string]projectView {
	projects := make(map[string]projectView, len(a.projects))
	for name, record := range a.projects {
		projects[name] = projectView{Name: name, Description: record.Description}
	}
	for _, record := range a.records {
		name, _, _ := strings.Cut(record.Path, "/")
		project := projects[name]
		project.Name = name
		project.ArtifactCount++
		projects[name] = project
	}
	return projects
}

func (a *application) patchProject(w http.ResponseWriter, r *http.Request, name string) {
	if err := naming.ValidateProject(name); err != nil {
		writeNameError(w, err)
		return
	}
	value, err := descriptionInput(r)
	if err != nil {
		writeDescriptionError(w)
		return
	}
	publisher, ok := auth.PublisherFromContext(r.Context())
	if !ok {
		auth.WriteError(w, 500, "internal_error", "publisher identity unavailable")
		return
	}
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	a.recordWriteMu.Lock()
	if value == "" {
		err = a.store.DeleteRecord(r.Context(), name+".json")
	} else {
		var body []byte
		body, err = json.Marshal(projectRecord{Description: value})
		if err == nil {
			err = a.store.PutRecord(r.Context(), name+".json", body)
		}
	}
	if err == nil {
		a.mu.Lock()
		if value == "" {
			delete(a.projects, name)
		} else {
			a.projects[name] = projectRecord{Description: value}
		}
		a.mu.Unlock()
	}
	a.recordWriteMu.Unlock()
	if err != nil {
		a.storageUnavailable(w, "edit Project description", err)
		return
	}
	a.log.Info("project description edited", "project", name, "publisher", publisher.Label)
	a.mu.RLock()
	project := a.projectViewsLocked()[name]
	a.mu.RUnlock()
	// Clearing an empty Project removes it from the list, but PATCH still
	// returns the Project that was edited.
	if project.Name == "" {
		project = projectView{Name: name}
	}
	writeJSON(w, 200, project)
}
