package portal

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed web
var webFiles embed.FS

func catalogueHandler() http.Handler {
	files, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	server := http.FileServerFS(files)
	shell, err := fs.ReadFile(files, "index.html")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(name, "assets/") {
			if _, err := fs.Stat(files, name); err != nil {
				http.NotFound(w, r)
				return
			}
			server.ServeHTTP(w, r)
			return
		}
		// Unknown routes belong to the SPA. Never expose other embedded files.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(shell))
	})
}
