// Package naming defines the shared Artifact URL naming rules.
package naming

import (
	"errors"
	"fmt"
	"strings"
)

const maxArtifactPathLength = 200

var (
	ErrNameInvalid    = errors.New("name_invalid")
	ErrNameReserved   = errors.New("name_reserved")
	ErrSuffixRequired = errors.New("artifact suffix required")
)

// Kind is the public shape of an Artifact.
type Kind string

const (
	SingleFile Kind = "single_file"
	Bundle     Kind = "bundle"
)

// ArtifactPath is a validated, relative Reader path. Categories are the
// segments between Project and Artifact name.
type ArtifactPath struct {
	Project    string
	Categories []string
	Name       string
	Kind       Kind
}

// ParseArtifactPath validates a path with its required public suffix: .html
// for a single-file Artifact or / for a Bundle. The returned path has no
// leading slash.
func ParseArtifactPath(value string) (ArtifactPath, error) {
	var (
		kind Kind
		stem string
	)
	switch {
	case strings.HasSuffix(value, ".html"):
		kind = SingleFile
		stem = strings.TrimSuffix(value, ".html")
	case strings.HasSuffix(value, "/"):
		kind = Bundle
		stem = strings.TrimSuffix(value, "/")
	default:
		return ArtifactPath{}, ErrSuffixRequired
	}
	if len(value) > maxArtifactPathLength {
		return ArtifactPath{}, fmt.Errorf("%w: path exceeds %d characters", ErrNameInvalid, maxArtifactPathLength)
	}
	segments, err := validateSegments(stem)
	if err != nil {
		return ArtifactPath{}, err
	}
	if len(segments) < 2 {
		return ArtifactPath{}, fmt.Errorf("%w: an Artifact must be below its Project", ErrNameInvalid)
	}
	return ArtifactPath{
		Project:    segments[0],
		Categories: append([]string(nil), segments[1:len(segments)-1]...),
		Name:       segments[len(segments)-1],
		Kind:       kind,
	}, nil
}

// PublicPath returns the canonical Reader path including the shape suffix.
func (p ArtifactPath) PublicPath() string {
	path := p.Stem()
	if p.Kind == Bundle {
		return path + "/"
	}
	return path + ".html"
}

// Stem returns the Artifact path without .html or the Bundle's trailing slash.
func (p ArtifactPath) Stem() string {
	segments := make([]string, 0, len(p.Categories)+2)
	segments = append(segments, p.Project)
	segments = append(segments, p.Categories...)
	segments = append(segments, p.Name)
	return strings.Join(segments, "/")
}

// ValidateProject checks a single Project name.
func ValidateProject(value string) error {
	segments, err := validateSegments(value)
	if err != nil {
		return err
	}
	if len(segments) != 1 {
		return fmt.Errorf("%w: expected one Project segment", ErrNameInvalid)
	}
	return nil
}

// ValidatePrefix checks an optional API list prefix. Unlike an Artifact path,
// a prefix may name only a Project or Category and has no shape suffix.
func ValidatePrefix(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxArtifactPathLength {
		return fmt.Errorf("%w: prefix exceeds %d characters", ErrNameInvalid, maxArtifactPathLength)
	}
	value = strings.TrimSuffix(value, "/")
	_, err := validateSegments(value)
	return err
}

func validateSegments(value string) ([]string, error) {
	if value == "" || strings.HasPrefix(value, "/") {
		return nil, fmt.Errorf("%w: empty or absolute path", ErrNameInvalid)
	}
	segments := strings.Split(value, "/")
	for _, segment := range segments {
		if !validSegment(segment) {
			return nil, fmt.Errorf("%w: invalid path segment %q", ErrNameInvalid, segment)
		}
		if segment == "index" || segment == "cdn-cgi" {
			return nil, fmt.Errorf("%w: reserved path segment %q", ErrNameReserved, segment)
		}
	}
	return segments, nil
}

func validSegment(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
		if c == '-' && i > 0 && value[i-1] == '-' {
			return false
		}
	}
	return true
}
