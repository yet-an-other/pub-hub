package portal

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/naming"
)

func writeBusy(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", "5")
	auth.WriteError(w, http.StatusConflict, "busy", message)
}

func (a *application) deleteProject(w http.ResponseWriter, r *http.Request, name string) {
	if err := naming.ValidateProject(name); err != nil {
		writeNameError(w, err)
		return
	}
	publisher, ok := auth.PublisherFromContext(r.Context())
	if !ok {
		auth.WriteError(w, http.StatusInternalServerError, "internal_error", "publisher identity unavailable")
		return
	}
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	release, acquired := a.artifacts.beginProjectMutation(name, true)
	if !acquired {
		writeBusy(w, "Project mutation already in progress")
		return
	}
	defer release()

	started := time.Now()
	paths := a.projectArtifactPaths(name)
	for _, path := range paths {
		parsed, err := naming.ParseArtifactPath(path)
		if err != nil {
			a.storageUnavailable(w, "load Project Artifact", err)
			return
		}
		mutation := &artifactMutation{catalogue: a.artifacts, path: parsed, project: name, active: true}
		if _, err := mutation.delete(r.Context()); err != nil {
			var failure *artifactStorageError
			if errors.As(err, &failure) {
				a.storageUnavailable(w, string(failure.operation), err)
			} else {
				a.storageUnavailable(w, "delete Project Artifact", err)
			}
			return
		}
	}

	// Keep the description as the retry marker until every Artifact is gone.
	a.recordWriteMu.Lock()
	a.mu.RLock()
	_, hasDescription := a.projects[name]
	a.mu.RUnlock()
	var err error
	if hasDescription {
		err = a.store.DeleteRecord(r.Context(), name+".json")
	}
	if err == nil {
		a.mu.Lock()
		delete(a.projects, name)
		a.mu.Unlock()
	}
	a.recordWriteMu.Unlock()
	if err != nil {
		a.storageUnavailable(w, "delete Project description", err)
		return
	}
	a.log.Info("project deleted", "project", name, "artifact_count", len(paths), "publisher", publisher.Label, "duration_ms", time.Since(started).Milliseconds())
	w.WriteHeader(http.StatusNoContent)
}

func (a *application) projectArtifactPaths(name string) []string {
	a.artifacts.mu.RLock()
	defer a.artifacts.mu.RUnlock()
	prefix := name + "/"
	paths := make([]string, 0)
	for _, record := range a.artifacts.records {
		if strings.HasPrefix(record.Path, prefix) {
			paths = append(paths, record.Path)
		}
	}
	sort.Strings(paths)
	return paths
}
