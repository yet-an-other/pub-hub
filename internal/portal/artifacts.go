package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/buildversion"
	"github.com/yet-an-other/pub-hub/internal/naming"
)

const (
	maxPublishBytes       = 100 << 20
	maxBundleFiles        = 2000
	readinessProbeTimeout = 3 * time.Second
	spoolDirectory        = "/var/cache/pubhub/spool"
)

type artifactStore interface {
	LoadRecords(context.Context) ([]json.RawMessage, error)
	PutRecord(context.Context, string, []byte) error
	PutArtifact(context.Context, string, io.Reader, int64, string) error
	DeleteLeftovers(context.Context, string, map[string]struct{}) error
	DeleteArtifact(context.Context, string) error
	DeleteRecord(context.Context, string) error
	CheckBuckets(context.Context) (artifactsErr, metadataErr error)
}

type artifactRecord struct {
	Path          string    `json:"path"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastPublisher string    `json:"last_publisher"`
	TotalSize     int64     `json:"total_size"`
	FileCount     int       `json:"file_count"`
	State         string    `json:"state"`
}

type projectRecord struct {
	Description string `json:"description"`
}

type projectView struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	ArtifactCount int    `json:"artifact_count"`
}

type artifactView struct {
	Path          string    `json:"path"`
	URL           string    `json:"url"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastPublisher string    `json:"last_publisher"`
	TotalSize     int64     `json:"total_size"`
	FileCount     int       `json:"file_count"`
	State         string    `json:"state"`
}

type readinessStorage struct {
	Artifacts bool `json:"artifacts"`
	Metadata  bool `json:"metadata"`
}

type readinessIDP struct {
	Reachable bool `json:"reachable"`
}

type readinessResponse struct {
	Status  string           `json:"status"`
	Storage readinessStorage `json:"storage"`
	Zitadel readinessIDP     `json:"zitadel"`
}

type application struct {
	store         artifactStore
	publicBaseURL string
	spoolDir      string
	log           *slog.Logger
	now           func() time.Time

	loadMu    sync.Mutex
	mu        sync.RWMutex
	loaded    bool
	artifacts *artifactCatalogue
	projects  map[string]projectRecord
	// Writes to the metadata bucket and the in-memory index are serialized;
	// object transfers happen outside this lock.
	recordWriteMu sync.Mutex
}

func newApplication(store artifactStore, publicBaseURL, spoolDir string, log *slog.Logger) *application {
	if log == nil {
		log = slog.Default()
	}
	if spoolDir == "" {
		spoolDir = spoolDirectory
	}
	a := &application{
		store:         store,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
		spoolDir:      spoolDir,
		log:           log,
		now:           time.Now,
		projects:      make(map[string]projectRecord),
	}
	a.artifacts = newArtifactCatalogue(store, a.publicBaseURL, &a.recordWriteMu)
	return a
}

func (a *application) prepareSpool() error {
	if err := os.MkdirAll(a.spoolDir, 0o700); err != nil {
		return fmt.Errorf("create upload spool: %w", err)
	}
	entries, err := os.ReadDir(a.spoolDir)
	if err != nil {
		return fmt.Errorf("read upload spool: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(a.spoolDir, entry.Name())); err != nil {
			return fmt.Errorf("clear upload spool: %w", err)
		}
	}
	return nil
}

func (a *application) loadStartup(ctx context.Context) {
	count, incomplete, err := a.reloadRecords(ctx)
	if err != nil {
		a.log.Warn("artifact records unavailable at startup", "error", err.Error())
	}
	a.log.Info("artifact records loaded", "count", count, "incomplete", incomplete, "available", err == nil)
}

func (a *application) reloadRecords(ctx context.Context) (int, int, error) {
	a.loadMu.Lock()
	defer a.loadMu.Unlock()
	a.mu.RLock()
	alreadyLoaded := a.loaded
	a.mu.RUnlock()
	if alreadyLoaded {
		count, incomplete := a.artifacts.counts()
		return count, incomplete, nil
	}

	payloads, err := a.store.LoadRecords(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load metadata records: %w", err)
	}
	projects := make(map[string]projectRecord)
	var artifactPayloads []json.RawMessage
	for _, payload := range payloads {
		var shape struct {
			Path    string `json:"path"`
			Project string `json:"project"`
		}
		if err := json.Unmarshal(payload, &shape); err != nil {
			return 0, 0, fmt.Errorf("decode metadata record: %w", err)
		}
		if shape.Path != "" {
			artifactPayloads = append(artifactPayloads, payload)
			continue
		}
		project, err := decodeProjectRecord(payload, shape.Project)
		if err != nil {
			return 0, 0, err
		}
		if _, exists := projects[shape.Project]; exists {
			return 0, 0, fmt.Errorf("duplicate Project record %q", shape.Project)
		}
		projects[shape.Project] = project
	}
	count, incomplete, err := a.artifacts.load(artifactPayloads)
	if err != nil {
		return 0, 0, err
	}
	a.mu.Lock()
	a.projects = projects
	a.loaded = true
	a.mu.Unlock()
	return count, incomplete, nil
}

func (a *application) ensureLoaded(ctx context.Context) error {
	a.mu.RLock()
	loaded := a.loaded
	a.mu.RUnlock()
	if loaded {
		return nil
	}
	_, _, err := a.reloadRecords(ctx)
	return err
}

func (a *application) readyz(w http.ResponseWriter, r *http.Request, authenticator *auth.Authenticator) {
	type bucketStatus struct {
		artifactsErr, metadataErr error
	}
	storageResults := make(chan bucketStatus, 1)
	idpResults := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), readinessProbeTimeout)
		defer cancel()
		artifactsErr, metadataErr := a.store.CheckBuckets(ctx)
		storageResults <- bucketStatus{artifactsErr: artifactsErr, metadataErr: metadataErr}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), readinessProbeTimeout)
		defer cancel()
		idpResults <- authenticator.IDPReachable(ctx)
	}()
	storage := <-storageResults
	idpReachable := <-idpResults
	ready := storage.artifactsErr == nil && storage.metadataErr == nil
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(readinessResponse{
		Status: state,
		Storage: readinessStorage{
			Artifacts: storage.artifactsErr == nil,
			Metadata:  storage.metadataErr == nil,
		},

		Zitadel: readinessIDP{Reachable: idpReachable},
	})
}

func (a *application) apiRoutes(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if path == "/whoami" {
		if r.Method != http.MethodGet {
			auth.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		publisher, ok := auth.PublisherFromContext(r.Context())
		if !ok {
			auth.WriteError(w, http.StatusInternalServerError, "internal_error", "publisher identity unavailable")
			return
		}
		label := publisher.Label
		if publisher.DisplayLabel != "" {
			label = publisher.DisplayLabel
		}
		writeJSON(w, http.StatusOK, struct {
			Label         string `json:"label"`
			PortalVersion string `json:"portal_version"`
		}{Label: label, PortalVersion: buildversion.Current()})
		return
	}
	if path == "/config" {
		if r.Method != http.MethodGet {
			auth.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			PublicBaseURL string `json:"public_base_url"`
		}{a.publicBaseURL})
		return
	}
	if path == "/artifacts" {
		if r.Method != http.MethodGet {
			auth.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		a.listArtifacts(w, r)
		return
	}
	if path == "/projects" && r.Method == http.MethodGet {
		a.listProjects(w, r)
		return
	}
	if strings.HasPrefix(path, "/projects/") {
		name := strings.TrimPrefix(path, "/projects/")
		if r.Method == http.MethodPatch {
			a.patchProject(w, r, name)
			return
		}
		if r.Method == http.MethodDelete {
			a.deleteProject(w, r, name)
			return
		}
	}
	if strings.HasPrefix(path, "/artifacts/") {
		if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete && r.Method != http.MethodPatch {
			auth.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		a.artifact(w, r, strings.TrimPrefix(path, "/artifacts/"))
		return
	}
	auth.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
}

func (a *application) listArtifacts(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if err := naming.ValidatePrefix(prefix); err != nil {
		writeNameError(w, err)
		return
	}
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	writeJSON(w, http.StatusOK, a.artifacts.list(prefix))
}

func (a *application) artifact(w http.ResponseWriter, r *http.Request, path string) {
	parsed, err := naming.ParseArtifactPath(path)
	if errors.Is(err, naming.ErrSuffixRequired) {
		auth.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if err != nil {
		writeNameError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		a.getArtifact(w, r, parsed)
		return
	}
	if r.Method == http.MethodDelete {
		a.deleteArtifact(w, r, parsed)
		return
	}
	if r.Method == http.MethodPatch {
		a.patchArtifact(w, r, parsed)
		return
	}
	a.publishArtifact(w, r, parsed)
}

func (a *application) getArtifact(w http.ResponseWriter, r *http.Request, path naming.ArtifactPath) {
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	view, ok := a.artifacts.get(path)
	if !ok {
		auth.WriteError(w, http.StatusNotFound, "not_found", "Artifact not found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *application) deleteArtifact(w http.ResponseWriter, r *http.Request, path naming.ArtifactPath) {
	publisher, ok := auth.PublisherFromContext(r.Context())
	if !ok {
		auth.WriteError(w, http.StatusInternalServerError, "internal_error", "publisher identity unavailable")
		return
	}
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	mutation, ok := a.artifacts.beginMutation(path)
	if !ok {
		a.log.Warn("artifact mutation busy", "path", path.PublicPath(), "publisher", publisher.Label)
		w.Header().Set("Retry-After", "5")
		auth.WriteError(w, http.StatusConflict, "busy", "Artifact mutation already in progress")
		return
	}
	defer mutation.release()
	started := time.Now()
	record, err := mutation.delete(r.Context())
	if errors.Is(err, errArtifactMissing) {
		auth.WriteError(w, http.StatusNotFound, "not_found", "Artifact not found")
		return
	}
	if err != nil {
		var failure *artifactStorageError
		if errors.As(err, &failure) {
			a.storageUnavailable(w, string(failure.operation), err)
		} else {
			a.storageUnavailable(w, "delete Artifact", err)
		}
		return
	}
	a.log.Info("artifact deleted", "path", record.Path, "publisher", publisher.Label, "file_count", record.FileCount, "bytes", record.TotalSize, "duration_ms", time.Since(started).Milliseconds())
	w.WriteHeader(http.StatusNoContent)
}

func (a *application) publishArtifact(w http.ResponseWriter, r *http.Request, path naming.ArtifactPath) {
	publisher, ok := auth.PublisherFromContext(r.Context())
	if !ok {
		auth.WriteError(w, http.StatusInternalServerError, "internal_error", "publisher identity unavailable")
		return
	}
	if err := a.ensureLoaded(r.Context()); err != nil {
		a.storageUnavailable(w, "load metadata records", err)
		return
	}
	mutation, ok := a.artifacts.beginMutation(path)
	if !ok {
		w.Header().Set("Retry-After", "5")
		auth.WriteError(w, http.StatusConflict, "busy", "Artifact mutation already in progress")
		return
	}
	defer mutation.release()

	a.publishStaged(w, r, mutation, publisher.Label, time.Now())
}

func (a *application) writePublishError(w http.ResponseWriter, err error, path naming.ArtifactPath, publisher string) {
	var failure *artifactStorageError
	if !errors.As(err, &failure) {
		a.storageUnavailable(w, "publish Artifact", err)
		return
	}
	if failure.operation == opWriteIncomplete {
		writeRecordError(w, a, err)
		return
	}
	if failure.operation == opUploadSingle {
		a.log.Warn("artifact upload failed", "path", path.PublicPath(), "publisher", publisher, "error", err.Error())
		auth.WriteError(w, http.StatusServiceUnavailable, "storage_unavailable", "Artifact storage is unavailable")
		return
	}
	a.storageUnavailable(w, string(failure.operation), err)
}

func writeRecordError(w http.ResponseWriter, a *application, err error) {
	switch {
	case errors.Is(err, errNestingConflict):
		auth.WriteError(w, http.StatusConflict, "nesting_conflict", "Artifact cannot nest inside another Artifact")
	case errors.Is(err, errShapeConflict):
		auth.WriteError(w, http.StatusConflict, "shape_conflict", "Artifact shape conflicts with existing record")
	case errors.Is(err, errExists):
		auth.WriteError(w, http.StatusPreconditionFailed, "exists", "Artifact already exists")
	default:
		a.storageUnavailable(w, "write incomplete record", err)
	}
}

func (a *application) storageUnavailable(w http.ResponseWriter, operation string, err error) {
	a.log.Warn("storage unavailable", "operation", operation, "error", err.Error())
	auth.WriteError(w, http.StatusServiceUnavailable, "storage_unavailable", "Artifact storage is unavailable")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeNameError(w http.ResponseWriter, err error) {
	if errors.Is(err, naming.ErrNameReserved) {
		auth.WriteError(w, http.StatusBadRequest, "name_reserved", "Artifact path contains a reserved name")
		return
	}
	auth.WriteError(w, http.StatusBadRequest, "name_invalid", "Artifact path is invalid")
}
