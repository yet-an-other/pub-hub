package s3store_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yet-an-other/pub-hub/internal/storage/s3store"
)

func TestNewRejectsANonLoopbackEndpoint(t *testing.T) {
	_, err := s3store.New(s3store.Config{
		Endpoint:       "https://s3.example.test",
		ArtifactBucket: "pubhub-artifacts",
		MetadataBucket: "pubhub-meta",
		AccessKey:      "access",
		SecretKey:      "secret",
	})
	if err == nil {
		t.Fatal("New accepted a non-loopback endpoint")
	}
}

func TestLoadRecordsIncludesProjectAndArtifactRecords(t *testing.T) {
	var fetched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pubhub-meta":
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>xform.json</Key></Contents><Contents><Key>xform/notes/plan.json</Key></Contents><Contents><Key>readme.txt</Key></Contents></ListBucketResult>`)
		case "/pubhub-meta/xform.json":
			fetched = append(fetched, "xform.json")
			_, _ = io.WriteString(w, `{"description":"A project"}`)
		case "/pubhub-meta/xform/notes/plan.json":
			fetched = append(fetched, "xform/notes/plan.json")
			_, _ = io.WriteString(w, `{"artifact":"plan"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := newTestStore(t, server)
	records, err := store.LoadRecords(context.Background())
	if err != nil {
		t.Fatalf("LoadRecords: %v", err)
	}
	if want := []string{"xform.json", "xform/notes/plan.json"}; !reflect.DeepEqual(fetched, want) {
		t.Errorf("fetched keys = %v, want %v", fetched, want)
	}
	if len(records) != 2 || string(records[0]) != `{"project":"xform","description":"A project"}` || string(records[1]) != `{"artifact":"plan"}` {
		t.Errorf("records = %q, want Project and Artifact JSON", records)
	}
}

func TestLoadRecordsValidatesProjectRecord(t *testing.T) {
	for _, test := range []struct {
		name, key, body, errorText string
	}{
		{"invalid JSON", "xform.json", `{`, `is not valid JSON`},
		{"oversized", "xform.json", strings.Repeat(" ", 1<<20+1), `exceeds 1048576 bytes`},
		{"missing description", "xform.json", `{}`, `must contain only a description`},
		{"empty description", "xform.json", `{"description":""}`, `must contain a nonempty description`},
		{"non-string description", "xform.json", `{"description":42}`, `must contain a nonempty description`},
		{"extra field", "xform.json", `{"description":"A project","project":"other"}`, `must contain only a description`},
		{"invalid Project name", "Bad.json", `{"description":"A project"}`, `invalid Project name`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/pubhub-meta":
					_, _ = io.WriteString(w, strings.Replace(`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>xform.json</Key></Contents></ListBucketResult>`, "xform.json", test.key, 1))
				case "/pubhub-meta/" + test.key:
					_, _ = io.WriteString(w, test.body)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			store := newTestStore(t, server)
			records, err := store.LoadRecords(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.key) || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("LoadRecords error = %v, want record key and %q", err, test.errorText)
			}
			if records != nil {
				t.Errorf("records = %q, want nil on error", records)
			}
		})
	}
}

func newTestStore(t *testing.T, server *httptest.Server) *s3store.Store {
	t.Helper()
	store, err := s3store.New(s3store.Config{
		Endpoint:       server.URL,
		ArtifactBucket: "pubhub-artifacts",
		MetadataBucket: "pubhub-meta",
		AccessKey:      "access",
		SecretKey:      "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func TestPutArtifactDoesNotUseAWSChunkedOverTLS(t *testing.T) {
	var contentEncoding, requestPath, authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentEncoding = r.Header.Get("Content-Encoding")
		requestPath = r.URL.EscapedPath()
		authorization = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, err := s3store.New(s3store.Config{
		Endpoint:       server.URL,
		ArtifactBucket: "pubhub-artifacts",
		MetadataBucket: "pubhub-meta",
		AccessKey:      "access",
		SecretKey:      "secret",
		HTTPClient:     server.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.PutArtifact(context.Background(), "xform/plan.html", strings.NewReader("<title>Plan</title>"), int64(len("<title>Plan</title>")), "text/html"); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	if strings.Contains(strings.ToLower(contentEncoding), "aws-chunked") {
		t.Errorf("PutObject Content-Encoding = %q, must not contain aws-chunked", contentEncoding)
	}
	if requestPath != "/pubhub-artifacts/xform/plan.html" {
		t.Errorf("PutObject path = %q, want path-style bucket prefix", requestPath)
	}
	if !strings.Contains(authorization, "/default/s3/aws4_request") {
		t.Errorf("SigV4 scope = %q, want region default and service s3", authorization)
	}
}
