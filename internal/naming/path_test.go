package naming_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yet-an-other/pub-hub/internal/naming"
)

func TestParseArtifactPath(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantPath  string
		wantStem  string
		wantKind  naming.Kind
		wantError error
	}{
		{name: "single file", value: "xform/notes/my-plan.html", wantPath: "xform/notes/my-plan.html", wantStem: "xform/notes/my-plan", wantKind: naming.SingleFile},
		{name: "file directly under project", value: "xform/plan.html", wantPath: "xform/plan.html", wantStem: "xform/plan", wantKind: naming.SingleFile},
		{name: "bundle", value: "xform/notes/roster-sync/", wantPath: "xform/notes/roster-sync/", wantStem: "xform/notes/roster-sync", wantKind: naming.Bundle},
		{name: "missing suffix", value: "xform/notes/plan", wantError: naming.ErrSuffixRequired},
		{name: "project-level artifact", value: "xform.html", wantError: naming.ErrNameInvalid},
		{name: "reserved artifact name", value: "xform/notes/index.html", wantError: naming.ErrNameReserved},
		{name: "reserved category", value: "xform/cdn-cgi/plan.html", wantError: naming.ErrNameReserved},
		{name: "reserved project", value: "index/plan.html", wantError: naming.ErrNameReserved},
		{name: "uppercase", value: "xform/Plan.html", wantError: naming.ErrNameInvalid},
		{name: "dot in segment", value: "xform/plan.v2.html", wantError: naming.ErrNameInvalid},
		{name: "leading slash", value: "/xform/plan.html", wantError: naming.ErrNameInvalid},
		{name: "empty segment", value: "xform//plan.html", wantError: naming.ErrNameInvalid},
		{name: "double hyphen", value: "xform/my--plan.html", wantError: naming.ErrNameInvalid},
		{name: "segment too long", value: "xform/" + strings.Repeat("a", 64) + ".html", wantError: naming.ErrNameInvalid},
		{name: "path too long", value: "project/" + strings.Repeat("a", 63) + "/" + strings.Repeat("b", 63) + "/" + strings.Repeat("c", 63) + ".html", wantError: naming.ErrNameInvalid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := naming.ParseArtifactPath(test.value)
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("ParseArtifactPath(%q) error = %v, want %v", test.value, err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArtifactPath(%q): %v", test.value, err)
			}
			if got.PublicPath() != test.wantPath {
				t.Errorf("PublicPath() = %q, want %q", got.PublicPath(), test.wantPath)
			}
			if got.Stem() != test.wantStem {
				t.Errorf("Stem() = %q, want %q", got.Stem(), test.wantStem)
			}
			if got.Kind != test.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, test.wantKind)
			}
		})
	}
}

func TestValidatePrefix(t *testing.T) {
	for _, prefix := range []string{"", "xform", "xform/notes", "xform/notes/"} {
		if err := naming.ValidatePrefix(prefix); err != nil {
			t.Errorf("ValidatePrefix(%q): %v", prefix, err)
		}
	}
	for _, prefix := range []string{"/xform", "xform//notes", "xform/index", "xform/Bad"} {
		if err := naming.ValidatePrefix(prefix); err == nil {
			t.Errorf("ValidatePrefix(%q) succeeded, want an error", prefix)
		}
	}
}
