package portal

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"

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
	mutation, ok := a.artifacts.beginMutation(path)
	if !ok {
		w.Header().Set("Retry-After", "5")
		auth.WriteError(w, 409, "busy", "Artifact mutation already in progress")
		return
	}
	defer mutation.release()
	view, err := mutation.editDescription(r.Context(), value)
	if errors.Is(err, errArtifactMissing) {
		auth.WriteError(w, 404, "not_found", "Artifact not found")
		return
	}
	if err != nil {
		a.storageUnavailable(w, "edit Artifact description", err)
		return
	}
	a.log.Info("artifact description edited", "path", view.Path, "publisher", publisher.Label)
	writeJSON(w, 200, view)
}

func (a *application) listProjects(w http.ResponseWriter, r *http.Request) {
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	projects := a.projectViews()
	result := make([]projectView, 0, len(projects))
	for _, project := range projects {
		result = append(result, project)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	writeJSON(w, 200, result)
}

func (a *application) projectViews() map[string]projectView {
	// Metadata writes update their in-memory indexes before releasing this lock.
	// Hold it across both reads so counts and descriptions share one snapshot.
	a.recordWriteMu.Lock()
	defer a.recordWriteMu.Unlock()
	counts := a.artifacts.projectCounts()
	a.mu.RLock()
	defer a.mu.RUnlock()
	projects := make(map[string]projectView, len(a.projects)+len(counts))
	for name, record := range a.projects {
		projects[name] = projectView{Name: name, Description: record.Description}
	}
	for name, count := range counts {
		project := projects[name]
		project.Name = name
		project.ArtifactCount = count
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
	release, acquired := a.artifacts.beginProjectMutation(name, false)
	if !acquired {
		writeBusy(w, "Project mutation already in progress")
		return
	}
	defer release()
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
	project := a.projectViews()[name]
	// Clearing an empty Project removes it from the list, but PATCH still
	// returns the Project that was edited.
	if project.Name == "" {
		project = projectView{Name: name}
	}
	writeJSON(w, 200, project)
}

func decodeProjectRecord(payload json.RawMessage, name string) (projectRecord, error) {
	var project projectRecord
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &project); err != nil || json.Unmarshal(payload, &fields) != nil || !validDescription(project.Description) || project.Description == "" || len(fields) != 2 || fields["description"] == nil || fields["project"] == nil || naming.ValidateProject(name) != nil {
		return projectRecord{}, errors.New("invalid Project metadata record")
	}
	return project, nil
}
