package portal

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareSpoolClearsOnlyPortalUploadFiles(t *testing.T) {
	spool := t.TempDir()
	for name, content := range map[string]string{
		"publish-stale": "partial upload",
		"keep.txt":      "not owned by the spool",
	} {
		if err := os.WriteFile(filepath.Join(spool, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app := newApplication(newMemoryArtifactStore(), "https://pub.example.test", spool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := app.prepareSpool(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spool, "publish-stale")); !os.IsNotExist(err) {
		t.Errorf("stale upload still exists (stat error %v)", err)
	}
	if contents, err := os.ReadFile(filepath.Join(spool, "keep.txt")); err != nil || string(contents) != "not owned by the spool" {
		t.Errorf("unrelated spool file = %q, err=%v", contents, err)
	}
}
