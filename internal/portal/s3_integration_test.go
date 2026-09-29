package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/storage/s3store"
)

func TestSingleFilePublishAgainstS3CompatibleServer(t *testing.T) {
	endpoint := os.Getenv("PUBHUB_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set PUBHUB_TEST_S3_ENDPOINT to run the S3 integration test")
	}
	accessKey := os.Getenv("PUBHUB_TEST_S3_ACCESS_KEY")
	secretKey := os.Getenv("PUBHUB_TEST_S3_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Fatal("PUBHUB_TEST_S3_ACCESS_KEY and PUBHUB_TEST_S3_SECRET_KEY are required with PUBHUB_TEST_S3_ENDPOINT")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitForS3(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	client := s3.New(s3.Options{
		Region:       "default",
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		BaseEndpoint: aws.String(strings.TrimRight(endpoint, "/")),
		UsePathStyle: true,
	})
	id, err := randomBucketSuffix()
	if err != nil {
		t.Fatal(err)
	}
	artifactBucket := "pubhub-it-artifacts-" + id
	metadataBucket := "pubhub-it-meta-" + id
	for _, bucket := range []string{artifactBucket, metadataBucket} {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("create bucket %s: %v", bucket, err)
		}
		bucket := bucket
		t.Cleanup(func() { deleteTestBucket(t, client, bucket) })
	}
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::%s/*"}]}`, artifactBucket)
	if _, err := client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(artifactBucket), Policy: aws.String(policy)}); err != nil {
		t.Fatalf("allow anonymous Artifact reads: %v", err)
	}

	store, err := s3store.New(s3store.Config{
		Endpoint:       endpoint,
		ArtifactBucket: artifactBucket,
		MetadataBucket: metadataBucket,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
	})
	if err != nil {
		t.Fatalf("create Portal S3 store: %v", err)
	}
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v2/introspect":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"active":true,"sub":"integration-publisher"}`)
		case "/.well-known/openid-configuration":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	authenticator, err := auth.NewAuthenticator(idp.URL, "hub-api", "secret", map[string]string{"integration-publisher": "integration"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	app := newApplication(store, "https://pub.example.test", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := app.prepareSpool(); err != nil {
		t.Fatal(err)
	}
	app.loadStartup(ctx)
	handler := routes(authenticator, app)
	content := "<!doctype html><title>Integration page</title><p>hello</p>"
	response := multipartRequest(t, handler, "xform/notes/plan.html", "plan.html", content)
	if response.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, body=%s", response.Code, response.Body)
	}
	metadata := decodeArtifact(t, response)
	if metadata.URL != "https://pub.example.test/xform/notes/plan.html" || metadata.Title != "Integration page" || metadata.State != "published" {
		t.Errorf("publish metadata = %+v", metadata)
	}

	anonymousURL := strings.TrimRight(endpoint, "/") + "/" + artifactBucket + "/xform/notes/plan.html"
	anonymousResponse, err := http.Get(anonymousURL)
	if err != nil {
		t.Fatalf("anonymous GET %s: %v", anonymousURL, err)
	}
	defer anonymousResponse.Body.Close()
	if anonymousResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(anonymousResponse.Body)
		t.Fatalf("anonymous GET status = %d, body=%s", anonymousResponse.StatusCode, body)
	}
	if got := anonymousResponse.Header.Get("Content-Type"); got != "text/html" {
		t.Errorf("anonymous Content-Type = %q, want text/html", got)
	}
	anonymousContent, err := io.ReadAll(anonymousResponse.Body)
	if err != nil || string(anonymousContent) != content {
		t.Errorf("anonymous body = %q, err=%v", anonymousContent, err)
	}

	storedRecord, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(metadataBucket), Key: aws.String("xform/notes/plan.json")})
	if err != nil {
		t.Fatalf("read stored metadata record: %v", err)
	}
	var record artifactRecord
	if err := json.NewDecoder(storedRecord.Body).Decode(&record); err != nil {
		t.Fatalf("decode stored record: %v", err)
	}
	_ = storedRecord.Body.Close()
	if record.Path != "xform/notes/plan.html" || record.State != "published" || record.Title != "Integration page" {
		t.Errorf("stored record = %+v", record)
	}

	ready := artifactRequest(t, handler, http.MethodGet, "/readyz", nil, "")
	if ready.Code != http.StatusOK {
		t.Errorf("readyz = %d %s, want 200", ready.Code, ready.Body)
	}

	// Bundle replacement must paginate the listing and batch deletion of more
	// than one ListObjectsV2 page of leftovers.
	bundle := bundleRequest(t, handler, map[string]string{"index.html": "<title>Bundle</title>", "old.css": "body{}", "asset name.txt": "hello"})
	if bundle.Code != http.StatusCreated {
		t.Fatalf("publish Bundle = %d %s", bundle.Code, bundle.Body)
	}
	testReaderNginx(t, endpoint, artifactBucket)
	bulkCtx, cancelBulk := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelBulk()
	for i := 0; i < 1001; i++ {
		key := "xform/demo/old-" + strconv.Itoa(i) + ".txt"
		_, err := client.PutObject(bulkCtx, &s3.PutObjectInput{Bucket: aws.String(artifactBucket), Key: aws.String(key), Body: strings.NewReader("old")})
		if err != nil {
			t.Fatalf("seed leftover %d: %v", i, err)
		}
	}
	bundle = bundleRequest(t, handler, map[string]string{"index.html": "<title>Replaced</title>", "LICENSE": "hello"})
	if bundle.Code != http.StatusOK {
		t.Fatalf("replace Bundle = %d %s", bundle.Code, bundle.Body)
	}
	if got := decodeArtifact(t, bundle); got.FileCount != 2 || got.Title != "Replaced" {
		t.Errorf("Bundle metadata = %+v", got)
	}
	objects, err := client.ListObjectsV2(bulkCtx, &s3.ListObjectsV2Input{Bucket: aws.String(artifactBucket), Prefix: aws.String("xform/demo/")})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects.Contents) != 2 {
		t.Errorf("Bundle objects after replacement = %d, want 2", len(objects.Contents))
	}

	restarted := newApplication(store, "https://pub.example.test", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	restarted.loadStartup(ctx)
	reloaded := artifactRequest(t, routes(authenticator, restarted), http.MethodGet, "/api/artifacts/xform/notes/plan.html", nil, "")
	if reloaded.Code != http.StatusOK || decodeArtifact(t, reloaded) != metadata {
		t.Errorf("metadata after startup reload = %d %s, want %+v", reloaded.Code, reloaded.Body, metadata)
	}
	for _, path := range []string{"xform/notes/plan.html", "xform/demo/"} {
		deleted := artifactRequest(t, handler, http.MethodDelete, "/api/artifacts/"+path, nil, "")
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("delete %s = %d %s", path, deleted.Code, deleted.Body)
		}
	}
	for _, bucket := range []string{artifactBucket, metadataBucket} {
		objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err != nil {
			t.Fatalf("list bucket %s after deletes: %v", bucket, err)
		}
		if len(objects.Contents) != 0 {
			t.Errorf("bucket %s after deletes: objects=%v", bucket, objects.Contents)
		}
	}
}

func waitForS3(ctx context.Context, endpoint string) error {
	client := &http.Client{Timeout: time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/minio/health/live", nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				_ = response.Body.Close()
				if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("S3-compatible server at %s did not become ready: %w", endpoint, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func randomBucketSuffix() (string, error) {
	var value [6]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func deleteTestBucket(t *testing.T, client *s3.Client, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return
		}
		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key})
		}
	}
	_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
}
