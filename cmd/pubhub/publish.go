package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yet-an-other/pub-hub/internal/naming"
)

const maxBytes = 100 << 20
const maxFiles = 2000

type uploadFile struct {
	path, key string
	size      int64
}
type upload struct {
	files          []uploadFile
	size           int64
	title          string
	dots, symlinks int
	absolute       bool
	path           naming.ArtifactPath
}

var absoluteRef = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']/[^/][^"']*["']`)
var titleRe = regexp.MustCompile(`(?is)<title\s*>(.*?)</title\s*>`)

func title(path, fallback string) string {
	file, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return fallback
	}
	if match := titleRe.FindSubmatch(b); len(match) > 1 {
		value := strings.TrimSpace(string(match[1]))
		if value != "" {
			return value
		}
	}
	return fallback
}
func target(value string, dir bool) (naming.ArtifactPath, error) {
	suffix := ".html"
	if dir {
		suffix = "/"
	}
	if (dir && strings.HasSuffix(value, ".html")) || (!dir && strings.HasSuffix(value, "/")) {
		return naming.ArtifactPath{}, local("name_invalid", "source and target suffix disagree")
	}
	if !strings.HasSuffix(value, suffix) {
		value += suffix
	}
	parsed, err := naming.ParseArtifactPath(value)
	if err != nil {
		code := "name_invalid"
		if errors.Is(err, naming.ErrNameReserved) {
			code = "name_reserved"
		}
		return parsed, local(code, fmt.Sprintf("%v; use lowercase letters, digits and single hyphens in each segment, with a Project and name", err))
	}
	return parsed, nil
}
func inspect(source, targetName, description string) (upload, error) {
	var u upload
	info, err := os.Lstat(source)
	if err != nil {
		return u, local("source_invalid", err.Error())
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return u, local("source_invalid", "source must be a regular file or directory")
	}
	u.path, err = target(targetName, info.IsDir())
	if err != nil {
		return u, err
	}
	if !info.IsDir() && strings.ToLower(filepath.Ext(source)) != ".html" {
		return u, local("source_invalid", "single-file source must be .html")
	}
	if info.IsDir() {
		err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == source {
				return nil
			}
			rel, e := filepath.Rel(source, path)
			if e != nil {
				return e
			}
			if strings.HasPrefix(entry.Name(), ".") {
				u.dots++
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				u.symlinks++
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return local("source_invalid", "non-regular file: "+rel)
			}
			stat, e := entry.Info()
			if e != nil {
				return e
			}
			u.files = append(u.files, uploadFile{path, filepath.ToSlash(rel), stat.Size()})
			u.size += stat.Size()
			if len(u.files) > maxFiles {
				return local("too_many_files", "Bundle exceeds 2,000 files")
			}
			if u.size > maxBytes {
				return local("too_large", "request exceeds 100 MB")
			}
			return nil
		})
		if err != nil {
			return u, err
		}
		found := false
		for _, file := range u.files {
			if file.key == "index.html" {
				found = true
				u.title = title(file.path, u.path.Name)
				entry, e := os.Open(file.path)
				if e == nil {
					b, _ := io.ReadAll(io.LimitReader(entry, 1<<20))
					u.absolute = absoluteRef.Match(b)
					entry.Close()
				}
				break
			}
		}
		if !found {
			return u, local("index_missing", "Bundle needs index.html")
		}
	} else {
		u.files = []uploadFile{{source, "file", info.Size()}}
		u.size = info.Size()
		u.title = title(source, u.path.Name)
	}
	if u.size > maxBytes {
		return u, local("too_large", "request exceeds 100 MB")
	}
	// Include framing, field headers, description and closing boundary in the request limit.
	count := &counter{}
	writer := multipart.NewWriter(count)
	if description != "" {
		if err = writer.WriteField("description", description); err != nil {
			return u, err
		}
	}
	for _, f := range u.files {
		if _, err = writer.CreateFormFile(f.key, filepath.Base(f.path)); err != nil {
			return u, err
		}
	}
	if err = writer.Close(); err != nil {
		return u, err
	}
	if u.size+count.n > maxBytes {
		return u, local("too_large", "multipart request exceeds 100 MB")
	}
	return u, nil
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
func multipartBody(files []uploadFile, description, boundary string) io.ReadCloser {
	read, write := io.Pipe()
	mw := multipart.NewWriter(write)
	_ = mw.SetBoundary(boundary)
	go func() {
		var err error
		if description != "" {
			err = mw.WriteField("description", description)
		}
		for _, f := range files {
			if err != nil {
				break
			}
			var part io.Writer
			part, err = mw.CreateFormFile(f.key, filepath.Base(f.path))
			if err != nil {
				break
			}
			var file *os.File
			file, err = os.Open(f.path)
			if err != nil {
				break
			}
			_, err = io.Copy(part, file)
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err == nil {
			err = mw.Close()
		}
		write.CloseWithError(err)
	}()
	return read
}
func (c *cli) publish(args []string) error {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	description := flags.String("d", "", "private description")
	noOverwrite := flags.Bool("no-overwrite", false, "create only")
	dry := flags.Bool("dry-run", false, "validate only")
	asJSON := flags.Bool("json", false, "print metadata")
	// flag.FlagSet stops at the first positional argument; accept flags after the target too.
	var options, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-d" {
			options = append(options, arg)
			if i+1 < len(args) {
				i++
				options = append(options, args[i])
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			options = append(options, arg)
		} else {
			positionals = append(positionals, arg)
		}
	}
	if err := flags.Parse(append(options, positionals...)); err != nil {
		return local("usage", err.Error())
	}
	if flags.NArg() != 2 {
		return local("usage", "publish <file|dir> <project>/<name> [-d description] [--no-overwrite] [--dry-run] [--json]")
	}
	if len([]rune(*description)) > 1000 {
		return local("request_invalid", "description exceeds 1,000 characters")
	}
	u, err := inspect(flags.Arg(0), flags.Arg(1), *description)
	if err != nil {
		return err
	}
	if u.dots > 0 || u.symlinks > 0 {
		fmt.Fprintf(c.err, "Skipped %d dot-files/directories and %d symlinks\n", u.dots, u.symlinks)
	}
	if u.absolute {
		fmt.Fprintln(c.err, "Warning: index.html references absolute / paths; build with a relative base (Vite base: './').")
	}
	if *dry {
		fmt.Fprintf(c.out, "%s: %d files, %d bytes, title %q; skipped %d dot-items and %d symlinks\n", u.path.PublicPath(), len(u.files), u.size, u.title, u.dots, u.symlinks)
		return nil
	}
	base, token, err := c.credentials()
	if err != nil {
		return err
	}
	writer := multipart.NewWriter(io.Discard)
	ct := writer.FormDataContentType()
	boundary := writer.Boundary()
	data, err := c.call("PUT", "/api/artifacts/"+u.path.PublicPath(), token, base, ct, func() (io.ReadCloser, error) { return multipartBody(u.files, *description, boundary), nil }, *noOverwrite)
	if err != nil {
		return err
	}
	if *asJSON {
		return jsonOutput(c.out, data)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return err
	}
	if result.URL == "" {
		return errors.New("Portal response has no URL")
	}
	fmt.Fprintln(c.out, result.URL)
	return nil
}
