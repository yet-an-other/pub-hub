package portal

import (
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/naming"
	htmltokenizer "golang.org/x/net/html"
)

var (
	errTooLarge     = errors.New("publish request too large")
	errFileCount    = errors.New("single-file publish requires exactly one file")
	errSpoolFailure = errors.New("upload spool failed")
	errTooManyFiles = errors.New("too many Bundle files")
	errPathInvalid  = errors.New("invalid Bundle path")
	errIndexMissing = errors.New("Bundle index missing")
)

// stagedArtifact owns the temporary files until the publish attempt finishes.
// The Catalogue owns the record-first mutation; staging never writes records.
type stagedArtifact struct {
	single       *os.File
	dir          string
	files        []bundleFile
	seen         map[string]struct{}
	size         int64
	description  *string
	invalidField bool
}

func (s *stagedArtifact) close() {
	if s.single != nil {
		name := s.single.Name()
		_ = s.single.Close()
		_ = os.Remove(name)
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

func validDescription(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= 1000
}

func validDescriptionBytes(value []byte) bool {
	return len(value) <= 4000 && validDescription(string(value))
}

// stageArtifact applies shared multipart and description rules while leaving
// file count, Bundle keys, and entry requirements specific to each shape.
func (a *application) stageArtifact(w http.ResponseWriter, r *http.Request, path naming.ArtifactPath) (_ *stagedArtifact, err error) {
	if r.ContentLength > maxPublishBytes {
		return nil, errTooLarge
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return nil, errors.New("request must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("read multipart request: %w", err)
	}

	staged := &stagedArtifact{}
	defer func() {
		if err != nil {
			staged.close()
		}
	}()
	if path.Kind == naming.Bundle {
		staged.dir, err = os.MkdirTemp(a.spoolDir, "publish-")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errSpoolFailure, err)
		}
		staged.seen = make(map[string]struct{})
	}
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			return nil, fmt.Errorf("read multipart part: %w", partErr)
		}
		if part.FileName() == "" {
			// Bundle requests reject unexpected fields immediately. HTML-file
			// requests finish reading first, then report them after file count.
			if path.Kind == naming.Bundle && (part.FormName() != "description" || staged.description != nil) {
				_ = part.Close()
				return nil, errors.New("unexpected multipart field")
			}
			body, readErr := io.ReadAll(io.LimitReader(part, 4001))
			_ = part.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read multipart field: %w", readErr)
			}
			if part.FormName() != "description" || staged.description != nil || !validDescriptionBytes(body) {
				if path.Kind == naming.Bundle {
					return nil, errors.New("invalid description")
				}
				staged.invalidField = true
				continue
			}
			value := string(body)
			staged.description = &value
			continue
		}
		if path.Kind == naming.Bundle {
			err = staged.addBundleFile(part, path.PublicPath())
		} else {
			err = staged.addSingleFile(part, a.spoolDir)
		}
		if err != nil {
			return nil, err
		}
	}
	if path.Kind == naming.Bundle {
		if _, ok := staged.seen["index.html"]; !ok {
			return nil, errIndexMissing
		}
	} else {
		if staged.single == nil {
			return nil, errFileCount
		}
		if staged.invalidField {
			return nil, errors.New("unexpected multipart field")
		}
	}
	return staged, nil
}

func (s *stagedArtifact) addSingleFile(part *multipart.Part, spoolDir string) error {
	if s.single != nil {
		_ = part.Close()
		return errFileCount
	}
	file, err := os.CreateTemp(spoolDir, "publish-")
	if err != nil {
		_ = part.Close()
		return fmt.Errorf("%w: create upload spool file: %v", errSpoolFailure, err)
	}
	s.single = file
	s.size, err = io.Copy(file, part)
	closePartErr := part.Close()
	if err == nil {
		err = closePartErr
	}
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return fmt.Errorf("spool upload: %w", err)
		}
		return fmt.Errorf("%w: spool upload: %v", errSpoolFailure, err)
	}
	return nil
}

func (s *stagedArtifact) addBundleFile(part *multipart.Part, prefix string) error {
	defer part.Close()
	key := part.FormName()
	if !validBundleKey(key) || len(prefix)+len(key) > 1024 {
		return errPathInvalid
	}
	if _, exists := s.seen[key]; exists {
		return errPathInvalid
	}
	s.seen[key] = struct{}{}
	if len(s.files) >= maxBundleFiles {
		return errTooManyFiles
	}
	file, err := os.CreateTemp(s.dir, "file-")
	if err != nil {
		return fmt.Errorf("%w: %v", errSpoolFailure, err)
	}
	size, copyErr := io.Copy(file, part)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return fmt.Errorf("%w: %v", errSpoolFailure, closeErr)
	}
	s.size += size
	s.files = append(s.files, bundleFile{key: key, path: file.Name(), size: size})
	return nil
}

func (s *stagedArtifact) title(fallback string) (string, error) {
	if s.single != nil {
		title := extractTitle(s.single, fallback)
		_, err := s.single.Seek(0, io.SeekStart)
		return title, err
	}
	for _, file := range s.files {
		if file.key == "index.html" {
			handle, err := os.Open(file.path)
			if err != nil {
				return "", err
			}
			defer handle.Close()
			return extractTitle(handle, fallback), nil
		}
	}
	return "", errIndexMissing
}

func (a *application) publishStaged(w http.ResponseWriter, r *http.Request, mutation *artifactMutation, publisher string, started time.Time) {
	staged, err := a.stageArtifact(w, r, mutation.path)
	if err != nil {
		writeUploadError(w, err)
		return
	}
	defer staged.close()
	title, err := staged.title(mutation.path.Name)
	if err != nil {
		message := "Could not read spooled entry"
		if staged.single != nil {
			message = "could not read spooled upload"
		}
		auth.WriteError(w, http.StatusInternalServerError, "internal_error", message)
		return
	}
	var single io.Reader
	if staged.single != nil {
		single = staged.single
	}
	view, exists, err := mutation.publish(r.Context(), artifactPublish{
		title: title, description: staged.description, publisher: publisher,
		updatedAt: a.now().UTC(), createOnly: r.Header.Get("If-None-Match") == "*",
		files: staged.files, single: single, size: staged.size,
	})
	if err != nil {
		a.writePublishError(w, err, mutation.path, publisher)
		return
	}
	a.log.Info("artifact published", "path", view.Path, "publisher", publisher, "file_count", view.FileCount, "bytes", view.TotalSize, "duration_ms", time.Since(started).Milliseconds())
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	writeJSON(w, status, view)
}

func writeUploadError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr), errors.Is(err, errTooLarge):
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
