package portal

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
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
