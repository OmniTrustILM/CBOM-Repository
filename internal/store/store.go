package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	manager "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	managerTypes "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var (
	ErrNotFound = errors.New("not found")
)

const (
	MetaVersionKey     = "version"
	MetaCryptoStatsKey = "crypto-stats"
	// MetaCryptoStatsVersionKey names the counting algorithm that produced the value
	// under MetaCryptoStatsKey (see service.CryptoStatsVersion). Objects uploaded
	// before the key existed carry no value and hold the legacy shallow count.
	MetaCryptoStatsVersionKey = "crypto-stats-version"
	// MetaCryptoStatsTruncatedKey is set to "true" when the component walk that
	// produced MetaCryptoStatsKey stopped at its depth bound, so the counts are a
	// lower bound rather than the whole document. The key is absent otherwise.
	MetaCryptoStatsTruncatedKey = "crypto-stats-truncated"
)

type S3Contract interface {
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type S3Manager interface {
	UploadObject(ctx context.Context, input *manager.UploadObjectInput, opts ...func(*manager.Options)) (*manager.UploadObjectOutput, error)
}

type Config struct {
	Region       string `envconfig:"APP_S3_REGION" required:"true"`
	Endpoint     string `envconfig:"APP_S3_ENDPOINT"`
	Bucket       string `envconfig:"APP_S3_BUCKET" required:"true"`
	AccessKey    string `envconfig:"APP_S3_ACCESS_KEY" required:"true"`
	SecretKey    string `envconfig:"APP_S3_SECRET_KEY" required:"true"`
	UsePathStyle bool   `envconfig:"APP_S3_USE_PATH_STYLE" default:"true"`
}

type Store struct {
	cfg       Config
	s3Client  S3Contract
	s3Manager S3Manager
}

type Metadata struct {
	Version     string
	CryptoStats string
	// CryptoStatsVersion is written under MetaCryptoStatsVersionKey when non-empty.
	CryptoStatsVersion string
	// CryptoStatsTruncated is written under MetaCryptoStatsTruncatedKey, as "true",
	// only when set; a complete count leaves the key out entirely.
	CryptoStatsTruncated bool
}

func (m Metadata) Map() map[string]string {
	res := map[string]string{
		MetaVersionKey:     m.Version,
		MetaCryptoStatsKey: m.CryptoStats,
	}
	if m.CryptoStatsVersion != "" {
		res[MetaCryptoStatsVersionKey] = m.CryptoStatsVersion
	}
	if m.CryptoStatsTruncated {
		res[MetaCryptoStatsTruncatedKey] = "true"
	}
	return res
}

func New(cfg Config, s3Client S3Contract, s3Manager S3Manager) Store {
	s := Store{
		cfg:       cfg,
		s3Client:  s3Client,
		s3Manager: s3Manager,
	}

	return s
}

// ObjectInfo is one listed object: its key and the LastModified timestamp the store
// reported for it. LastModified precision is the store's (seconds on AWS S3,
// milliseconds on MinIO).
type ObjectInfo struct {
	Key          string
	LastModified time.Time
}

// Search returns every object in the S3 bucket whose LastModified is strictly after the
// specified Unix timestamp. The search iterates through all objects in the bucket using
// pagination and filters them by LastModified; objects are returned in listing order
// (UTF-8 key order).
//
// Parameters:
//   - ctx: Context for cancellation, deadlines and additional slog fields.
//   - ts: Unix timestamp (seconds since epoch) used as the lower bound for filtering
//
// Returns a slice of ObjectInfo and an error if the operation fails. An empty slice is
// returned if no objects match the criteria or if the bucket is empty. Listed entries
// without a key or LastModified are logged and skipped.
func (s Store) Search(ctx context.Context, ts int64) ([]ObjectInfo, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
	}

	unixTimestamp := time.Unix(ts, 0)

	var err error
	var output *s3.ListObjectsV2Output
	res := []ObjectInfo{}

	objectPaginator := s3.NewListObjectsV2Paginator(s.s3Client, input)
	for objectPaginator.HasMorePages() {
		if output, err = objectPaginator.NextPage(ctx); err != nil {
			slog.ErrorContext(ctx, "`s3.paginator.NextPage()` failed.", slog.String("error", err.Error()))
			return nil, errors.New("obtaining next page failed")
		}
		for _, cpy := range output.Contents {
			if cpy.Key == nil || cpy.LastModified == nil {
				slog.WarnContext(ctx, "Listed object lacks a key or LastModified. Skipping.", slog.String("key", aws.ToString(cpy.Key)))
				continue
			}
			if unixTimestamp.Before(*cpy.LastModified) {
				res = append(res, ObjectInfo{Key: *cpy.Key, LastModified: *cpy.LastModified})
			}
		}
	}
	return res, nil
}

// GetObjectVersions retrieves all version numbers for a given object URN and
// indicates whether an original version exists. The function lists all objects
// in the S3 bucket with the specified URN prefix and parses their version
// suffixes.
//
// Object keys are expected to follow the format "urn:uuid:<uuid>-<version>"
// where version is either a numeric value or the literal string "original".
//
// Parameters:
//   - ctx: Context for cancellation, deadlines and additional slog fields.
//   - urn: The object URN prefix to search for (e.g., "urn:uuid:12345678-1234-1234-1234-123456789abc")
//
// Returns:
//   - []int: A sorted slice of version numbers found for the object
//   - bool: True if an "original" version exists, false otherwise
//   - error: ErrNotFound if no objects match the URN, or other errors if the operation fails
//
// The function will return an error if any object key does not follow the expected
// naming convention or if version suffixes cannot be parsed as integers.
func (s Store) GetObjectVersions(ctx context.Context, urn string) ([]int, bool, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(urn),
	}

	var err error
	var output *s3.ListObjectsV2Output
	var objects []types.Object

	objectPaginator := s3.NewListObjectsV2Paginator(s.s3Client, input)
	for objectPaginator.HasMorePages() {
		if output, err = objectPaginator.NextPage(ctx); err != nil {
			slog.ErrorContext(ctx, "`s3.paginator.NextPage()` failed.", slog.String("error", err.Error()))
			return nil, false, errors.New("obtaining next page failed")
		}
		objects = append(objects, output.Contents...)
	}

	if len(objects) == 0 {
		return nil, false, ErrNotFound
	}

	// post process just the versions
	var res []int
	var hasOriginal bool
	for _, cpy := range objects {
		after, found := strings.CutPrefix(*cpy.Key, fmt.Sprintf("%s-", urn))
		if !found {
			slog.ErrorContext(ctx, "Unexpected suffix in s3 key.",
				slog.String("key", *cpy.Key),
				slog.String("key format invariant", "urn:uuid:<uuid>-<version>"),
			)
			return nil, false, fmt.Errorf("unexpected key %s", *cpy.Key)
		}
		if after == "original" {
			hasOriginal = true
			continue
		}
		ver, err := strconv.Atoi(after)
		if err != nil {
			slog.ErrorContext(ctx, "Unexpected suffix in s3 key, suffix should be a number",
				slog.String("key", *cpy.Key),
				slog.String("suffix", after),
				slog.String("key format invariant", "urn:uuid:<uuid>-<version>"),
			)
			return nil, false, fmt.Errorf("unexpected suffix %s", after)
		}
		res = append(res, ver)
	}
	sort.Ints(res)
	return res, hasOriginal, nil
}

type HeadObject struct {
	ContentLength int64
	ContentType   string
	LastModified  time.Time
	Metadata      map[string]string
}

// GetHeadObject retrieves metadata for an object in S3 without downloading the
// object's content. This is useful for checking object existence and obtaining
// metadata such as size, content type, last modified time, and custom metadata.
//
// Parameters:
//   - ctx: Context for cancellation, deadlines and additional slog fields.
//   - key: The S3 object key to retrieve metadata for
//
// Returns a HeadObject containing the object's metadata and an error if the
// operation fails. Returns ErrNotFound if the object does not exist in the bucket.
func (s Store) GetHeadObject(ctx context.Context, key string) (HeadObject, error) {
	head, err := s.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})

	var nsk *types.NoSuchKey
	var nf *types.NotFound

	switch {
	case errors.As(err, &nsk) || errors.As(err, &nf):
		return HeadObject{}, ErrNotFound

	case err != nil:
		slog.ErrorContext(ctx, "`s3.HeadObject()` failed.", slog.String("error", err.Error()))
		return HeadObject{}, errors.New("`s3.HeadObject()` failed")

	// Defensive check: handle an unexpected nil HeadObject result when no error is reported.
	case head == nil:
		return HeadObject{}, errors.New("`s3.HeadObject()` returned nil result without error")
	}

	return HeadObject{
		ContentLength: aws.ToInt64(head.ContentLength),
		ContentType:   aws.ToString(head.ContentType),
		LastModified:  aws.ToTime(head.LastModified),
		Metadata:      head.Metadata,
	}, nil
}

// GetObject retrieves the complete contents of an object from S3 and returns
// it as a byte slice. Returns ErrNotFound if the object does not exist
// in the bucket.
//
// Parameters:
//   - ctx: Context for cancellation, deadlines and additional slog fields.
//   - key: The S3 object key to retrieve
//
// Returns the object's contents as a byte slice and an error if the operation
// fails.
func (s Store) GetObject(ctx context.Context, key string) ([]byte, error) {
	result, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})

	var nsk *types.NoSuchKey
	var nf *types.NotFound

	switch {
	case errors.As(err, &nsk) || errors.As(err, &nf):
		return nil, ErrNotFound

	case err != nil:
		slog.ErrorContext(ctx, "`s3.GetObject()` failed.", slog.String("error", err.Error()))
		return nil, err
	}

	defer func() {
		_ = result.Body.Close()
	}()

	b, err := io.ReadAll(result.Body)
	if err != nil {
		slog.ErrorContext(ctx, "`io.ReadAll()` failed.", slog.String("error", err.Error()))
		return nil, err
	}

	return b, nil
}

// KeyExists checks whether an object with the specified key exists in the S3
// bucket without retrieving its contents.
//
// Parameters:
//   - ctx: Context for cancellation, deadlines and additional slog fields.
//   - key: The S3 object key to check
//
// Returns true if the object exists, false if it does not exist, and an error
// if the operation fails for reasons other than the object not being found
// (e.g., network errors, permission issues).
func (s Store) KeyExists(ctx context.Context, key string) (bool, error) {
	_, err := s.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}

	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return false, nil
	}

	slog.ErrorContext(ctx, "`s3.HeadObject()` failed.", slog.String("error", err.Error()))
	return false, err
}

// Upload stores an object in S3 with the specified key, metadata, and contents.
// The object is uploaded with a SHA256 checksum for data integrity verification.
//
// Parameters:
//   - ctx: Context for cancellation, deadlines and additional slog fields.
//   - key: The S3 object key under which to store the content
//   - meta: Metadata to attach to the object (version and crypto stats)
//   - contents: The byte slice containing the object's data to upload
//
// Returns an error if the upload operation fails.
func (s Store) Upload(ctx context.Context, key string, meta Metadata, contents []byte) error {
	input := &manager.UploadObjectInput{
		Bucket:            aws.String(s.cfg.Bucket),
		Key:               aws.String(key),
		Body:              bytes.NewReader(contents),
		Metadata:          meta.Map(),
		ChecksumAlgorithm: managerTypes.ChecksumAlgorithmSha256,
		ContentType:       aws.String("application/json"),
	}
	_, err := s.s3Manager.UploadObject(ctx, input)
	if err != nil {
		slog.ErrorContext(ctx, "`s3.manager.UploadObject()` failed.", slog.String("error", err.Error()))
		return err
	}

	return nil
}

func (s Store) HealthCheck(ctx context.Context) error {
	_, err := s.s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.cfg.Bucket),
	})
	return err
}
