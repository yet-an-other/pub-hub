package portal

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareSpoolEmptiesSpoolAtStartup(t *testing.T) {
	spool := t.TempDir()
	for name, content := range map[string]string{
		"publish-stale": "partial upload",
		"keep.txt":      "stale data",
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
	if _, err := os.Stat(filepath.Join(spool, "keep.txt")); !os.IsNotExist(err) {
		t.Errorf("stale spool file still exists (stat error %v)", err)
	}
}
