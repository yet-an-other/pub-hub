package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/naming"
	htmltokenizer "golang.org/x/net/html"
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

	loadMu sync.Mutex
	mu     sync.RWMutex
	loaded bool
	// records is keyed by the suffix-free Artifact stem.
	records  map[string]artifactRecord
	projects map[string]projectRecord
	inFlight map[string]struct{}
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
	return &application{
		store:         store,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
		spoolDir:      spoolDir,
		log:           log,
		now:           time.Now,
		records:       make(map[string]artifactRecord),
		projects:      make(map[string]projectRecord),
		inFlight:      make(map[string]struct{}),
	}
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
		a.mu.RLock()
		count := len(a.records)
		incomplete := countIncomplete(a.records)
		a.mu.RUnlock()
		return count, incomplete, nil
	}

	payloads, err := a.store.LoadRecords(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load metadata records: %w", err)
	}
	records := make(map[string]artifactRecord, len(payloads))
	projects := make(map[string]projectRecord)
	incomplete := 0
	for _, payload := range payloads {
		var shape struct {
			Path    string `json:"path"`
			Project string `json:"project"`
		}
		if err := json.Unmarshal(payload, &shape); err != nil {
			return 0, 0, fmt.Errorf("decode metadata record: %w", err)
		}
		if shape.Path == "" {
			var project projectRecord
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(payload, &project); err != nil || json.Unmarshal(payload, &fields) != nil || !validDescription(project.Description) || project.Description == "" || len(fields) != 2 || fields["description"] == nil || fields["project"] == nil || naming.ValidateProject(shape.Project) != nil {
				return 0, 0, fmt.Errorf("invalid Project metadata record")
			}
			if _, exists := projects[shape.Project]; exists {
				return 0, 0, fmt.Errorf("duplicate Project record %q", shape.Project)
			}
			projects[shape.Project] = project
			continue
		}
		var record artifactRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return 0, 0, fmt.Errorf("decode metadata record: %w", err)
		}
		parsed, err := naming.ParseArtifactPath(record.Path)
		if err != nil || parsed.Stem() == "" || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
			return 0, 0, fmt.Errorf("invalid metadata record for path %q", record.Path)
		}
		if record.State != "incomplete" && record.State != "published" {
			return 0, 0, fmt.Errorf("invalid state %q in metadata record for path %q", record.State, record.Path)
		}
		stem := parsed.Stem()
		if _, exists := records[stem]; exists {
			return 0, 0, fmt.Errorf("duplicate metadata record for path %q", record.Path)
		}
		records[stem] = record
		if record.State == "incomplete" {
			incomplete++
		}
	}
	a.mu.Lock()
	a.records = records
	a.projects = projects
	a.loaded = true
	a.mu.Unlock()
	return len(records), incomplete, nil
}

func countIncomplete(records map[string]artifactRecord) int {
	count := 0
	for _, record := range records {
		if record.State == "incomplete" {
			count++
		}
	}
	return count
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
		writeJSON(w, http.StatusOK, struct {
			Label string `json:"label"`
		}{Label: publisher.Label})
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
	if strings.HasPrefix(path, "/projects/") && r.Method == http.MethodPatch {
		a.patchProject(w, r, strings.TrimPrefix(path, "/projects/"))
		return
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
	a.mu.RLock()
	views := make([]artifactView, 0, len(a.records))
	for _, record := range a.records {
		if strings.HasPrefix(record.Path, prefix) {
			views = append(views, a.view(record))
		}
	}
	a.mu.RUnlock()
	sort.Slice(views, func(i, j int) bool { return views[i].Path < views[j].Path })
	writeJSON(w, http.StatusOK, views)
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
	a.mu.RLock()
	record, ok := a.records[path.Stem()]
	a.mu.RUnlock()
	if !ok || record.Path != path.PublicPath() {
		auth.WriteError(w, http.StatusNotFound, "not_found", "Artifact not found")
		return
	}
	writeJSON(w, http.StatusOK, a.view(record))
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
	stem := path.Stem()
	if !a.claim(stem) {
		a.log.Warn("artifact mutation busy", "path", path.PublicPath(), "publisher", publisher.Label)
		w.Header().Set("Retry-After", "5")
		auth.WriteError(w, http.StatusConflict, "busy", "Artifact mutation already in progress")
		return
	}
	defer a.release(stem)

	a.mu.RLock()
	record, exists := a.records[stem]
	a.mu.RUnlock()
	if !exists || record.Path != path.PublicPath() {
		auth.WriteError(w, http.StatusNotFound, "not_found", "Artifact not found")
		return
	}
	started := time.Now()
	record.State = "incomplete"
	if err := a.putRecord(r.Context(), stem, record, false); err != nil {
		a.storageUnavailable(w, "write incomplete record", err)
		return
	}
	entry := path.PublicPath()
	if path.Kind == naming.Bundle {
		entry += "index.html"
	}
	if err := a.store.DeleteArtifact(r.Context(), entry); err != nil {
		a.storageUnavailable(w, "delete Artifact entry", err)
		return
	}
	if path.Kind == naming.Bundle {
		if err := a.store.DeleteLeftovers(r.Context(), path.PublicPath(), nil); err != nil {
			a.storageUnavailable(w, "delete Bundle files", err)
			return
		}
	}
	a.recordWriteMu.Lock()
	err := a.store.DeleteRecord(r.Context(), stem+".json")
	if err == nil {
		a.mu.Lock()
		delete(a.records, stem)
		a.mu.Unlock()
	}
	a.recordWriteMu.Unlock()
	if err != nil {
		a.storageUnavailable(w, "delete Artifact record", err)
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
	stem := path.Stem()
	if !a.claim(stem) {
		w.Header().Set("Retry-After", "5")
		auth.WriteError(w, http.StatusConflict, "busy", "Artifact mutation already in progress")
		return
	}
	defer a.release(stem)

	started := time.Now()
	if path.Kind == naming.Bundle {
		a.publishBundle(w, r, path, publisher.Label, started)
		return
	}
	var description *string
	file, size, err := a.spoolSingleFile(w, r, &description)
	if err != nil {
		writeUploadError(w, err)
		return
	}
	defer func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}()

	title := extractTitle(file, path.Name)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		auth.WriteError(w, http.StatusInternalServerError, "internal_error", "could not read spooled upload")
		return
	}

	updatedAt := a.now().UTC()
	a.mu.RLock()
	previous, exists := a.records[stem]
	a.mu.RUnlock()
	createdAt := updatedAt
	if exists {
		createdAt = previous.CreatedAt
	}
	value := previous.Description
	if description != nil {
		value = *description
	}
	record := artifactRecord{
		Path:          path.PublicPath(),
		Title:         title,
		Description:   value,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
		LastPublisher: publisher.Label,
		TotalSize:     size,
		FileCount:     1,
		State:         "incomplete",
	}
	if err := a.putRecord(r.Context(), stem, record, r.Header.Get("If-None-Match") == "*"); err != nil {
		writeRecordError(w, a, err)
		return
	}

	if err := a.store.PutArtifact(r.Context(), path.PublicPath(), file, size, "text/html"); err != nil {
		a.log.Warn("artifact upload failed", "path", path.PublicPath(), "publisher", publisher.Label, "error", err.Error())
		auth.WriteError(w, http.StatusServiceUnavailable, "storage_unavailable", "Artifact storage is unavailable")
		return
	}

	record.State = "published"
	if err := a.putRecord(r.Context(), stem, record, false); err != nil {
		a.storageUnavailable(w, "write published record", err)
		return
	}
	a.log.Info("artifact published", "path", record.Path, "publisher", publisher.Label, "file_count", record.FileCount, "bytes", record.TotalSize, "duration_ms", time.Since(started).Milliseconds())
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	writeJSON(w, status, a.view(record))
}

var (
	errNestingConflict = errors.New("Artifact nesting conflict")
	errShapeConflict   = errors.New("Artifact shape conflict")
	errExists          = errors.New("Artifact already exists")
)

func (a *application) nestingConflict(stem string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for other := range a.records {
		if other != stem && (strings.HasPrefix(stem, other+"/") || strings.HasPrefix(other, stem+"/")) {
			return true
		}
	}
	return false
}

func (a *application) putRecord(ctx context.Context, stem string, record artifactRecord, createOnly bool) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	a.recordWriteMu.Lock()
	defer a.recordWriteMu.Unlock()
	if record.State == "incomplete" {
		if a.nestingConflict(stem) {
			return errNestingConflict
		}
		a.mu.RLock()
		previous, exists := a.records[stem]
		a.mu.RUnlock()
		if exists && previous.Path != record.Path {
			return errShapeConflict
		}
		if exists && createOnly {
			return errExists
		}
	}
	if err := a.store.PutRecord(ctx, stem+".json", body); err != nil {
		return err
	}
	a.mu.Lock()
	a.records[stem] = record
	a.mu.Unlock()
	return nil
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

func (a *application) claim(stem string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.inFlight[stem]; exists {
		return false
	}
	a.inFlight[stem] = struct{}{}
	return true
}

func (a *application) release(stem string) {
	a.mu.Lock()
	delete(a.inFlight, stem)
	a.mu.Unlock()
}

func (a *application) view(record artifactRecord) artifactView {
	return artifactView{
		Path:          record.Path,
		URL:           a.publicBaseURL + "/" + record.Path,
		Title:         record.Title,
		Description:   record.Description,
		CreatedAt:     record.CreatedAt,
		UpdatedAt:     record.UpdatedAt,
		LastPublisher: record.LastPublisher,
		TotalSize:     record.TotalSize,
		FileCount:     record.FileCount,
		State:         record.State,
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

func writeUploadError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		auth.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "Publish request exceeds 100 MB")
	case errors.Is(err, errTooLarge):
		auth.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "Publish request exceeds 100 MB")
	case errors.Is(err, errTooManyFiles):
		auth.WriteError(w, http.StatusRequestEntityTooLarge, "too_many_files", "Bundle exceeds 2,000 files")
	case errors.Is(err, errPathInvalid):
		auth.WriteError(w, http.StatusBadRequest, "path_invalid", "Invalid Bundle file path")
	case errors.Is(err, errIndexMissing):
		auth.WriteError(w, http.StatusUnprocessableEntity, "index_missing", "Bundle requires index.html")
	case errors.Is(err, errFileCount):
		auth.WriteError(w, http.StatusUnprocessableEntity, "file_count", "Single-file Artifacts require exactly one file")
	case errors.Is(err, errSpoolFailure):
		auth.WriteError(w, http.StatusInternalServerError, "internal_error", "Could not spool the upload")
	default:
		auth.WriteError(w, http.StatusBadRequest, "request_invalid", "Malformed publish request")
	}
}

var (
	errTooLarge     = errors.New("publish request too large")
	errFileCount    = errors.New("single-file publish requires exactly one file")
	errSpoolFailure = errors.New("upload spool failed")
	errTooManyFiles = errors.New("too many Bundle files")
	errPathInvalid  = errors.New("invalid Bundle path")
	errIndexMissing = errors.New("Bundle index missing")
)

func validDescription(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= 1000
}

func validDescriptionBytes(value []byte) bool {
	return len(value) <= 4000 && validDescription(string(value))
}

func (a *application) spoolSingleFile(w http.ResponseWriter, r *http.Request, description **string) (*os.File, int64, error) {
	if r.ContentLength > maxPublishBytes {
		return nil, 0, errTooLarge
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return nil, 0, errors.New("request must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, 0, fmt.Errorf("read multipart request: %w", err)
	}
	var file *os.File
	var size int64
	files := 0
	unexpectedField := false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if file != nil {
				_ = file.Close()
				_ = os.Remove(file.Name())
			}
			return nil, 0, fmt.Errorf("read multipart part: %w", err)
		}
		if part.FileName() == "" {
			body, readErr := io.ReadAll(io.LimitReader(part, 4001))
			_ = part.Close()
			if readErr != nil {
				if file != nil {
					_ = file.Close()
					_ = os.Remove(file.Name())
				}
				return nil, 0, fmt.Errorf("read multipart field: %w", readErr)
			}
			if part.FormName() != "description" || *description != nil || !validDescriptionBytes(body) {
				unexpectedField = true
			} else {
				value := string(body)
				*description = &value
			}
			continue
		}
		files++
		if files > 1 {
			_ = part.Close()
			if file != nil {
				_ = file.Close()
				_ = os.Remove(file.Name())
			}
			return nil, 0, errFileCount
		}
		file, err = os.CreateTemp(a.spoolDir, "publish-*")
		if err != nil {
			_ = part.Close()
			return nil, 0, fmt.Errorf("%w: create upload spool file: %v", errSpoolFailure, err)
		}
		size, err = io.Copy(file, part)
		closePartErr := part.Close()
		if err == nil {
			err = closePartErr
		}
		if err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				return nil, 0, fmt.Errorf("spool upload: %w", err)
			}
			return nil, 0, fmt.Errorf("%w: spool upload: %v", errSpoolFailure, err)
		}
	}
	if files != 1 {
		if file != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
		return nil, 0, errFileCount
	}
	if unexpectedField {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, 0, errors.New("unexpected multipart field")
	}
	return file, size, nil
}

func extractTitle(file *os.File, fallback string) string {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fallback
	}
	tokenizer := htmltokenizer.NewTokenizer(file)
	var title strings.Builder
	inTitle := false
	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case htmltokenizer.StartTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "title") {
				inTitle = true
			}
		case htmltokenizer.TextToken:
			if inTitle {
				title.Write(tokenizer.Text())
				title.WriteByte(' ')
			}
		case htmltokenizer.EndTagToken:
			token := tokenizer.Token()
			if strings.EqualFold(token.Data, "title") && inTitle {
				value := strings.Join(strings.Fields(html.UnescapeString(title.String())), " ")
				if value != "" {
					return value
				}
				return fallback
			}
		case htmltokenizer.ErrorToken:
			return fallback
		}
	}
}
