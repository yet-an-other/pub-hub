package portal

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/naming"
)

type bundleFile struct {
	key  string
	path string
	size int64
}

func validBundleKey(key string) bool {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") || len(segment) > 255 {
			return false
		}
		for _, char := range segment {
			if char == '\\' || unicode.IsControl(char) {
				return false
			}
		}
	}
	return true
}

var bundleContentTypes = map[string]string{
	".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript", ".json": "application/json",
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif", ".ico": "image/x-icon", ".woff": "font/woff", ".woff2": "font/woff2", ".txt": "text/plain", ".xml": "application/xml", ".wasm": "application/wasm", ".pdf": "application/pdf", ".map": "application/json",
}

func bundleContentType(key string) string {
	if value, ok := bundleContentTypes[strings.ToLower(filepath.Ext(key))]; ok {
		return value
	}
	return "application/octet-stream"
}

func (a *application) spoolBundle(w http.ResponseWriter, r *http.Request, prefix string, description **string) (string, []bundleFile, int64, error) {
	if r.ContentLength > maxPublishBytes {
		return "", nil, 0, errTooLarge
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return "", nil, 0, errors.New("request must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		return "", nil, 0, err
	}
	dir, err := os.MkdirTemp(a.spoolDir, "publish-")
	if err != nil {
		return "", nil, 0, fmt.Errorf("%w: %v", errSpoolFailure, err)
	}
	// The caller removes the directory on every outcome, including validation errors.
	var files []bundleFile
	seen := make(map[string]struct{})
	var total int64
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return dir, nil, 0, err
		}
		if part.FileName() == "" {
			if part.FormName() != "description" {
				_ = part.Close()
				return dir, nil, 0, errors.New("unexpected multipart field")
			}
			if *description != nil {
				_ = part.Close()
				return dir, nil, 0, errors.New("duplicate description")
			}
			body, err := io.ReadAll(io.LimitReader(part, 4001))
			_ = part.Close()
			if err != nil {
				return dir, nil, 0, err
			}
			if !validDescriptionBytes(body) {
				return dir, nil, 0, errors.New("description too long or invalid")
			}
			value := string(body)
			*description = &value
			continue
		}
		key := part.FormName()
		if !validBundleKey(key) || len(prefix)+len(key) > 1024 {
			_ = part.Close()
			return dir, nil, 0, errPathInvalid
		}
		if _, ok := seen[key]; ok {
			_ = part.Close()
			return dir, nil, 0, errPathInvalid
		}
		seen[key] = struct{}{}
		if len(files) >= maxBundleFiles {
			_ = part.Close()
			return dir, nil, 0, errTooManyFiles
		}
		file, err := os.CreateTemp(dir, "file-")
		if err != nil {
			_ = part.Close()
			return dir, nil, 0, fmt.Errorf("%w: %v", errSpoolFailure, err)
		}
		size, copyErr := io.Copy(file, part)
		closeErr := file.Close()
		_ = part.Close()
		if copyErr != nil {
			return dir, nil, 0, copyErr
		}
		if closeErr != nil {
			return dir, nil, 0, fmt.Errorf("%w: %v", errSpoolFailure, closeErr)
		}
		total += size
		files = append(files, bundleFile{key: key, path: file.Name(), size: size})
	}
	if _, ok := seen["index.html"]; !ok {
		return dir, nil, 0, errIndexMissing
	}
	return dir, files, total, nil
}

func (a *application) publishBundle(w http.ResponseWriter, r *http.Request, path naming.ArtifactPath, publisher string, started time.Time) {
	var description *string
	dir, files, total, err := a.spoolBundle(w, r, path.PublicPath(), &description)
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	if err != nil {
		writeUploadError(w, err)
		return
	}
	stem := path.Stem()
	var entry bundleFile
	for _, file := range files {
		if file.key == "index.html" {
			entry = file
			break
		}
	}
	titleFile, err := os.Open(entry.path)
	if err != nil {
		auth.WriteError(w, 500, "internal_error", "Could not read spooled entry")
		return
	}
	title := extractTitle(titleFile, path.Name)
	_ = titleFile.Close()
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
	record := artifactRecord{Path: path.PublicPath(), Title: title, Description: value, CreatedAt: createdAt, UpdatedAt: updatedAt, LastPublisher: publisher, TotalSize: total, FileCount: len(files), State: "incomplete"}
	if err := a.putRecord(r.Context(), stem, record, r.Header.Get("If-None-Match") == "*"); err != nil {
		writeRecordError(w, a, err)
		return
	}
	uploaded := make(map[string]struct{}, len(files))
	put := func(file bundleFile) error {
		key := path.PublicPath() + file.key
		handle, err := os.Open(file.path)
		if err != nil {
			return err
		}
		defer handle.Close()
		if err := a.store.PutArtifact(r.Context(), key, handle, file.size, bundleContentType(file.key)); err != nil {
			return err
		}
		uploaded[key] = struct{}{}
		return nil
	}
	for _, file := range files {
		if file.key == "index.html" {
			continue
		}
		if err := put(file); err != nil {
			a.storageUnavailable(w, "upload Bundle file", err)
			return
		}
	}
	if err := put(entry); err != nil {
		a.storageUnavailable(w, "upload Bundle entry", err)
		return
	}
	if err := a.store.DeleteLeftovers(r.Context(), path.PublicPath(), uploaded); err != nil {
		a.storageUnavailable(w, "delete Bundle leftovers", err)
		return
	}
	record.State = "published"
	if err := a.putRecord(r.Context(), stem, record, false); err != nil {
		a.storageUnavailable(w, "write published record", err)
		return
	}
	a.log.Info("artifact published", "path", record.Path, "publisher", publisher, "file_count", record.FileCount, "bytes", record.TotalSize, "duration_ms", time.Since(started).Milliseconds())
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	writeJSON(w, status, a.view(record))
}
