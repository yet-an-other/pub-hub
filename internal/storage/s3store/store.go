// Package s3store implements Artifact and metadata storage over S3.
package s3store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const maxRecordSize = 1 << 20

// Config contains the S3-compatible endpoint and credentials. Credentials
// are supplied through systemd LoadCredential= by the Portal.
type Config struct {
	Endpoint       string
	ArtifactBucket string
	MetadataBucket string
	AccessKey      string
	SecretKey      string
	HTTPClient     s3.HTTPClient
}

// Store accesses the two pub-hub buckets through the generic S3 API.
type Store struct {
	client         *s3.Client
	artifactBucket string
	metadataBucket string
}

// New constructs an RGW-compatible S3 client. Path-style addressing, region
// "default", and checksum behavior are pinned for Ceph RGW compatibility.
func New(cfg Config) (*Store, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", cfg.Endpoint)
	}
	endpointIP := net.ParseIP(endpoint.Hostname())
	if endpointIP == nil || !endpointIP.IsLoopback() {
		return nil, fmt.Errorf("S3 endpoint must use a loopback IP address, got %q", cfg.Endpoint)
	}
	if cfg.ArtifactBucket == "" || cfg.MetadataBucket == "" || cfg.ArtifactBucket == cfg.MetadataBucket {
		return nil, errors.New("distinct artifact and metadata buckets are required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3 access key and secret key are required")
	}

	options := s3.Options{
		Region:                     "default",
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		BaseEndpoint:               aws.String(strings.TrimRight(cfg.Endpoint, "/")),
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	}
	if cfg.HTTPClient != nil {
		options.HTTPClient = cfg.HTTPClient
	}
	return &Store{
		client:         s3.New(options),
		artifactBucket: cfg.ArtifactBucket,
		metadataBucket: cfg.MetadataBucket,
	}, nil
}

// PutArtifact writes one object under its Reader path.
func (s *Store) PutArtifact(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.artifactBucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	return err
}

// DeleteArtifact removes the Reader entry before any other Bundle objects.
// S3 treats a missing key as a successful deletion.
func (s *Store) DeleteArtifact(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.artifactBucket), Key: aws.String(key),
	})
	return err
}

// DeleteRecord removes an Artifact's metadata after all its objects are gone.
func (s *Store) DeleteRecord(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.metadataBucket), Key: aws.String(key),
	})
	return err
}

// DeleteLeftovers removes keys under prefix not present in keep. Listing is
// paginated and each delete is limited to S3's 1,000-key maximum.
func (s *Store) DeleteLeftovers(ctx context.Context, prefix string, keep map[string]struct{}) error {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.artifactBucket), Prefix: aws.String(prefix),
	})
	var leftovers []types.ObjectIdentifier
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list Bundle objects: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}
			if _, ok := keep[*object.Key]; !ok {
				leftovers = append(leftovers, types.ObjectIdentifier{Key: object.Key})
			}
		}
	}
	for len(leftovers) > 0 {
		chunk := leftovers
		if len(chunk) > 1000 {
			chunk = chunk[:1000]
		}
		result, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.artifactBucket), Delete: &types.Delete{Objects: chunk, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete Bundle leftovers: %w", err)
		}
		if len(result.Errors) > 0 {
			return fmt.Errorf("delete Bundle leftover %q: %s", aws.ToString(result.Errors[0].Key), aws.ToString(result.Errors[0].Code))
		}
		leftovers = leftovers[len(chunk):]
	}
	return nil
}

// PutRecord stores an Artifact record in the private metadata bucket.
func (s *Store) PutRecord(ctx context.Context, key string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.metadataBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("application/json"),
	})
	return err
}

// LoadRecords reads all nested JSON objects from the metadata bucket. Top-
// level JSON objects are reserved for Project records.
func (s *Store) LoadRecords(ctx context.Context) ([]json.RawMessage, error) {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.metadataBucket),
	})
	var records []json.RawMessage
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list metadata records: %w", err)
		}
		for _, object := range page.Contents {
			if object.Key == nil || !strings.HasSuffix(*object.Key, ".json") || !strings.Contains(*object.Key, "/") {
				continue
			}
			response, err := s.client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(s.metadataBucket),
				Key:    object.Key,
			})
			if err != nil {
				return nil, fmt.Errorf("read metadata record %q: %w", *object.Key, err)
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, maxRecordSize+1))
			closeErr := response.Body.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read metadata record %q: %w", *object.Key, readErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close metadata record %q: %w", *object.Key, closeErr)
			}
			if len(body) > maxRecordSize {
				return nil, fmt.Errorf("metadata record %q exceeds %d bytes", *object.Key, maxRecordSize)
			}
			if !json.Valid(body) {
				return nil, fmt.Errorf("metadata record %q is not valid JSON", *object.Key)
			}
			records = append(records, json.RawMessage(body))
		}
	}
	return records, nil
}

// CheckBuckets probes the two configured buckets independently.
func (s *Store) CheckBuckets(ctx context.Context) (artifactsErr, metadataErr error) {
	_, artifactsErr = s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.artifactBucket)})
	_, metadataErr = s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.metadataBucket)})
	return artifactsErr, metadataErr
}
