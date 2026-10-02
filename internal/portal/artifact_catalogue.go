package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yet-an-other/pub-hub/internal/naming"
)

// artifactCatalogue owns Artifact records and per-Artifact claims. Metadata
// writes share writeMu with Project descriptions; byte transfers do not hold it.
type artifactCatalogue struct {
	store            artifactStore
	publicBaseURL    string
	mu               sync.RWMutex
	records          map[string]artifactRecord
	inFlight         map[string]struct{}
	activeProjects   map[string]int
	deletingProjects map[string]struct{}
	writeMu          *sync.Mutex
}

func newArtifactCatalogue(store artifactStore, baseURL string, writeMu *sync.Mutex) *artifactCatalogue {
	return &artifactCatalogue{store: store, publicBaseURL: baseURL, records: make(map[string]artifactRecord), inFlight: make(map[string]struct{}), activeProjects: make(map[string]int), deletingProjects: make(map[string]struct{}), writeMu: writeMu}
}

func (c *artifactCatalogue) load(payloads []json.RawMessage) (int, int, error) {
	records := make(map[string]artifactRecord, len(payloads))
	incomplete := 0
	for _, payload := range payloads {
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
	c.mu.Lock()
	c.records = records
	c.mu.Unlock()
	return len(records), incomplete, nil
}

func (c *artifactCatalogue) counts() (int, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	incomplete := 0
	for _, record := range c.records {
		if record.State == "incomplete" {
			incomplete++
		}
	}
	return len(c.records), incomplete
}

func (c *artifactCatalogue) projectCounts() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	counts := make(map[string]int)
	for _, record := range c.records {
		name, _, _ := strings.Cut(record.Path, "/")
		counts[name]++
	}
	return counts
}

func (c *artifactCatalogue) list(prefix string) []artifactView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	views := make([]artifactView, 0, len(c.records))
	for _, record := range c.records {
		if strings.HasPrefix(record.Path, prefix) {
			views = append(views, c.view(record))
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Path < views[j].Path })
	return views
}

func (c *artifactCatalogue) get(path naming.ArtifactPath) (artifactView, bool) {
	record, ok := c.record(path)
	if !ok {
		return artifactView{}, false
	}
	return c.view(record), true
}

func (c *artifactCatalogue) record(path naming.ArtifactPath) (artifactRecord, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	record, ok := c.records[path.Stem()]
	return record, ok && record.Path == path.PublicPath()
}

// beginMutation returns the only handle able to mutate this Artifact.
// The handler holds it across upload staging so busy publishes fail early.
func (c *artifactCatalogue) beginMutation(path naming.ArtifactPath) (*artifactMutation, bool) {
	stem := path.Stem()
	c.mu.Lock()
	defer c.mu.Unlock()
	project := strings.SplitN(path.PublicPath(), "/", 2)[0]
	if _, deleting := c.deletingProjects[project]; deleting {
		return nil, false
	}
	if _, exists := c.inFlight[stem]; exists {
		return nil, false
	}
	c.inFlight[stem] = struct{}{}
	c.activeProjects[project]++
	return &artifactMutation{catalogue: c, path: path, project: project, active: true}, true
}

type artifactMutation struct {
	catalogue *artifactCatalogue
	path      naming.ArtifactPath
	project   string
	mu        sync.Mutex
	active    bool
}

func (m *artifactMutation) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return
	}
	m.active = false
	m.catalogue.mu.Lock()
	delete(m.catalogue.inFlight, m.path.Stem())
	m.catalogue.activeProjects[m.project]--
	if m.catalogue.activeProjects[m.project] == 0 {
		delete(m.catalogue.activeProjects, m.project)
	}
	m.catalogue.mu.Unlock()
}

func (c *artifactCatalogue) beginProjectMutation(project string, deleting bool) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.deletingProjects[project]; exists || (deleting && c.activeProjects[project] != 0) {
		return nil, false
	}
	if deleting {
		c.deletingProjects[project] = struct{}{}
	} else {
		c.activeProjects[project]++
	}
	return func() {
		c.mu.Lock()
		if deleting {
			delete(c.deletingProjects, project)
		} else {
			c.activeProjects[project]--
			if c.activeProjects[project] == 0 {
				delete(c.activeProjects, project)
			}
		}
		c.mu.Unlock()
	}, true
}

var (
	errNestingConflict  = errors.New("Artifact nesting conflict")
	errShapeConflict    = errors.New("Artifact shape conflict")
	errExists           = errors.New("Artifact already exists")
	errArtifactMissing  = errors.New("Artifact not found")
	errMutationReleased = errors.New("Artifact mutation lease released")
)

type artifactOperation string

const (
	opWriteIncomplete   artifactOperation = "write incomplete record"
	opUploadSingle      artifactOperation = "upload single Artifact"
	opUploadBundleFile  artifactOperation = "upload Bundle file"
	opUploadBundleEntry artifactOperation = "upload Bundle entry"
	opDeleteLeftovers   artifactOperation = "delete Bundle leftovers"
	opWritePublished    artifactOperation = "write published record"
	opDeleteEntry       artifactOperation = "delete Artifact entry"
	opDeleteBundleFiles artifactOperation = "delete Bundle files"
	opDeleteRecord      artifactOperation = "delete Artifact record"
	opEditDescription   artifactOperation = "edit Artifact description"
)

type artifactStorageError struct {
	operation artifactOperation
	err       error
}

func (e *artifactStorageError) Error() string { return e.err.Error() }
func (e *artifactStorageError) Unwrap() error { return e.err }

func (c *artifactCatalogue) putRecord(ctx context.Context, path naming.ArtifactPath, record artifactRecord, createOnly bool) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	stem := path.Stem()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if record.State == "incomplete" {
		c.mu.RLock()
		for other := range c.records {
			if other != stem && (strings.HasPrefix(stem, other+"/") || strings.HasPrefix(other, stem+"/")) {
				c.mu.RUnlock()
				return errNestingConflict
			}
		}
		previous, exists := c.records[stem]
		c.mu.RUnlock()
		if exists && previous.Path != record.Path {
			return errShapeConflict
		}
		if exists && createOnly {
			return errExists
		}
	}
	if err := c.store.PutRecord(ctx, stem+".json", body); err != nil {
		return err
	}
	c.mu.Lock()
	c.records[stem] = record
	c.mu.Unlock()
	return nil
}

type artifactPublish struct {
	title       string
	description *string
	publisher   string
	updatedAt   time.Time
	createOnly  bool
	files       []bundleFile
	single      io.Reader
	size        int64
}

// publish persists Incomplete before any object write, then publishes Bundle
// assets before its entry and removes leftovers before marking Published.
func (m *artifactMutation) publish(ctx context.Context, input artifactPublish) (artifactView, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return artifactView{}, false, errMutationReleased
	}
	c, path := m.catalogue, m.path
	previous, exists := c.record(path)
	createdAt := input.updatedAt
	if exists {
		createdAt = previous.CreatedAt
	}
	description := previous.Description
	if input.description != nil {
		description = *input.description
	}
	count := len(input.files)
	if input.single != nil {
		count = 1
	}
	record := artifactRecord{Path: path.PublicPath(), Title: input.title, Description: description, CreatedAt: createdAt, UpdatedAt: input.updatedAt, LastPublisher: input.publisher, TotalSize: input.size, FileCount: count, State: "incomplete"}
	if err := c.putRecord(ctx, path, record, input.createOnly); err != nil {
		return artifactView{}, false, &artifactStorageError{opWriteIncomplete, err}
	}
	if input.single != nil {
		if err := c.store.PutArtifact(ctx, record.Path, input.single, input.size, "text/html"); err != nil {
			return artifactView{}, exists, &artifactStorageError{opUploadSingle, err}
		}
	} else {
		var entry bundleFile
		uploaded := make(map[string]struct{}, len(input.files))
		put := func(file bundleFile) error {
			key := record.Path + file.key
			handle, err := os.Open(file.path)
			if err != nil {
				return err
			}
			defer handle.Close()
			if err := c.store.PutArtifact(ctx, key, handle, file.size, bundleContentType(file.key)); err != nil {
				return err
			}
			uploaded[key] = struct{}{}
			return nil
		}
		for _, file := range input.files {
			if file.key == "index.html" {
				entry = file
				continue
			}
			if err := put(file); err != nil {
				return artifactView{}, exists, &artifactStorageError{opUploadBundleFile, err}
			}
		}
		if err := put(entry); err != nil {
			return artifactView{}, exists, &artifactStorageError{opUploadBundleEntry, err}
		}
		if err := c.store.DeleteLeftovers(ctx, record.Path, uploaded); err != nil {
			return artifactView{}, exists, &artifactStorageError{opDeleteLeftovers, err}
		}
	}
	record.State = "published"
	if err := c.putRecord(ctx, path, record, false); err != nil {
		return artifactView{}, exists, &artifactStorageError{opWritePublished, err}
	}
	return c.view(record), exists, nil
}

func (m *artifactMutation) delete(ctx context.Context) (artifactRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return artifactRecord{}, errMutationReleased
	}
	c, path := m.catalogue, m.path
	record, exists := c.record(path)
	if !exists {
		return artifactRecord{}, errArtifactMissing
	}
	record.State = "incomplete"
	if err := c.putRecord(ctx, path, record, false); err != nil {
		return artifactRecord{}, &artifactStorageError{opWriteIncomplete, err}
	}
	entry := path.PublicPath()
	if path.Kind == naming.Bundle {
		entry += "index.html"
	}
	if err := c.store.DeleteArtifact(ctx, entry); err != nil {
		return artifactRecord{}, &artifactStorageError{opDeleteEntry, err}
	}
	if path.Kind == naming.Bundle {
		if err := c.store.DeleteLeftovers(ctx, path.PublicPath(), nil); err != nil {
			return artifactRecord{}, &artifactStorageError{opDeleteBundleFiles, err}
		}
	}
	c.writeMu.Lock()
	err := c.store.DeleteRecord(ctx, path.Stem()+".json")
	if err == nil {
		c.mu.Lock()
		delete(c.records, path.Stem())
		c.mu.Unlock()
	}
	c.writeMu.Unlock()
	if err != nil {
		return artifactRecord{}, &artifactStorageError{opDeleteRecord, err}
	}
	return record, nil
}

func (m *artifactMutation) editDescription(ctx context.Context, value string) (artifactView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return artifactView{}, errMutationReleased
	}
	c, path := m.catalogue, m.path
	record, exists := c.record(path)
	if !exists {
		return artifactView{}, errArtifactMissing
	}
	record.Description = value
	if err := c.putRecord(ctx, path, record, false); err != nil {
		return artifactView{}, &artifactStorageError{opEditDescription, err}
	}
	return c.view(record), nil
}

func (c *artifactCatalogue) view(record artifactRecord) artifactView {
	return artifactView{Path: record.Path, URL: c.publicBaseURL + "/" + record.Path, Title: record.Title, Description: record.Description, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, LastPublisher: record.LastPublisher, TotalSize: record.TotalSize, FileCount: record.FileCount, State: record.State}
}
