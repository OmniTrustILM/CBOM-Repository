package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/cbom-repository/internal/service"
	"github.com/OmniTrustILM/cbom-repository/internal/store"

	mockS3 "github.com/OmniTrustILM/cbom-repository/internal/store/mock"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestNewFunc(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	svc, err := service.New(
		store.New(store.Config{Bucket: "something"}, s3Mock, s3Manager),
		service.Config{CheckOnFetch: false},
	)
	require.NoError(t, err)
	require.True(t, svc.VersionSupported("1.6"))
	require.True(t, svc.VersionSupported("1.7"))
	require.False(t, svc.VersionSupported("1.4"))
	require.Equal(t, []string{"1.6", "1.7"}, svc.SupportedVersion())
}

func TestSearch_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	// Return a single page with two objects where LastModified is recent
	now := time.Now()
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{
			{Key: awsString("urn:uuid:1-1"), LastModified: &now},
			{Key: awsString("urn:uuid:2-2"), LastModified: &now},
		},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("urn:uuid:1-1"),
	}).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(123456),
		ContentType:   aws.String("application/vnd.cyclonedx+json"),
		LastModified:  &now,
		Metadata: map[string]string{
			store.MetaCryptoStatsKey: "{}",
		},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("urn:uuid:2-2"),
	}).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(123456),
		ContentType:   aws.String("application/vnd.cyclonedx+json"),
		LastModified:  &now,
		Metadata: map[string]string{
			store.MetaCryptoStatsKey: "{}",
		},
	}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: false})
	require.NoError(t, err)

	res, err := svc.Search(context.Background(), now.Unix()-1, 0)
	require.NoError(t, err)
	require.Len(t, res, 2)
	require.Equal(t, "urn:uuid:1", res[0].SerialNumber)
	require.Equal(t, "1", res[0].Version)
}

// A HEAD 404 for one listed key skips only that object (legacy mode); the call still
// succeeds and returns the remaining entries.
func TestSearch_LegacyHeadNotFoundSkipsObject(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	now := time.Now()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{
			{Key: awsString("urn:uuid:1-1"), LastModified: &now},
			{Key: awsString("urn:uuid:2-1"), LastModified: &now},
		},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("urn:uuid:1-1"),
	}).Return(nil, &types.NotFound{})
	s3Mock.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("urn:uuid:2-1"),
	}).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(1),
		ContentType:   aws.String("application/json"),
		LastModified:  &now,
		Metadata: map[string]string{
			store.MetaCryptoStatsKey: "{}",
		},
	}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{})
	require.NoError(t, err)

	res, err := svc.Search(context.Background(), now.Unix()-1, 0)
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, "urn:uuid:2", res[0].SerialNumber)
}

// A HEAD error other than not-found fails the whole call (legacy mode): the page would
// otherwise silently omit an object the caller cannot tell apart from one that simply
// does not exist.
func TestSearch_LegacyHeadErrorFailsCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	now := time.Now()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{{Key: awsString("urn:uuid:1-1"), LastModified: &now}},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(nil, errors.New("boom"))

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{})
	require.NoError(t, err)

	_, err = svc.Search(context.Background(), now.Unix()-1, 0)
	require.Error(t, err)
}

// A ListObjectsV2 failure fails Search, in either mode: the error surfaces before the
// legacy/paged branch is chosen.
func TestSearch_ListErrorFailsCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{
		{"legacy", 0},
		{"paged", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s3Mock := mockS3.NewMockS3Contract(ctrl)
			s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("boom"))

			st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
			svc, err := service.New(st, service.Config{})
			require.NoError(t, err)

			_, err = svc.Search(context.Background(), 0, tc.limit)
			require.Error(t, err)
		})
	}
}

func TestSearch_BadKey(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	now := time.Now()
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{{Key: awsString("badkey"), LastModified: &now}},
	}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: false})
	require.NoError(t, err)

	_, err = svc.Search(context.Background(), now.Unix()-1, 0)
	require.Error(t, err)
}

func TestGetBOMByUrn_VersionsNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	// ListObjectsV2 returns no contents -> store.GetObjectVersions returns ErrNotFound
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{Contents: []types.Object{}}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: false})
	require.NoError(t, err)

	_, err = svc.GetBOMByUrn(context.Background(), "urn:uuid:123", "")
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestGetBOMByUrn_GetObjectNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	// ListObjectsV2 returns one object version
	now := time.Now()
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{{Key: awsString("urn:uuid:123-1"), LastModified: &now}},
	}, nil)
	// GetObject returns NoSuchKey error
	s3Mock.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).Return((*s3.GetObjectOutput)(nil), &types.NoSuchKey{})

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: false})
	require.NoError(t, err)

	_, err = svc.GetBOMByUrn(context.Background(), "urn:uuid:123", "")
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestGetBOMByUrn_UnmarshalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	// When version is provided, service should call GetObject directly; return invalid JSON
	s3Mock.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("not json"))}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: true})
	require.NoError(t, err)

	_, err = svc.GetBOMByUrn(context.Background(), "urn:uuid:123", "1")
	require.Error(t, err)
}

func TestGetBOMByUrn_UnmarshalError_ButCheckOnFetchFalse(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	// When version is provided, service should call GetObject directly; return invalid JSON
	s3Mock.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("not json"))}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: false})
	require.NoError(t, err)

	_, err = svc.GetBOMByUrn(context.Background(), "urn:uuid:123", "1")
	require.NoError(t, err)
}

func TestGetBOMByUrn_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	// GetObject returns valid JSON
	s3Mock.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("{\"bomFormat\":\"CycloneDX\"}"))}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: false})
	require.NoError(t, err)

	res, err := svc.GetBOMByUrn(context.Background(), "urn:uuid:123", "1")
	require.NoError(t, err)
	require.IsType(t, []byte{}, res)
}

func TestGetBOMByUrn_Success_CheckOnFetchTrue(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	// GetObject returns valid JSON
	s3Mock.EXPECT().GetObject(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("{\"bomFormat\":\"CycloneDX\"}"))}, nil)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
	svc, err := service.New(st, service.Config{CheckOnFetch: true})
	require.NoError(t, err)

	res, err := svc.GetBOMByUrn(context.Background(), "urn:uuid:123", "1")
	require.NoError(t, err)
	require.IsType(t, []byte{}, res)
}

// helper to create *string for aws types
func awsString(s string) *string { return &s }

// Pins the legacy wire format byte for byte: field names (`created_at`, not
// `timestamp`), field order and encoding are what Core's client sees today. The
// handler encodes with json.NewEncoder exactly like this test does.
func TestSearch_LegacyEntryJSONIsByteCompatible(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	when := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{{Key: awsString("urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79-1"), LastModified: &when}},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(1),
		ContentType:   aws.String("application/json"),
		LastModified:  &when,
		Metadata: map[string]string{
			store.MetaCryptoStatsKey: `{"cryptoAssets":{"total":3,"algorithms":{"total":1},"certificates":{"total":1},"protocols":{"total":1},"relatedCryptoMaterials":{"total":0}}}`,
		},
	}, nil)

	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)

	res, err := svc.Search(context.Background(), when.Unix()-1, 0)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(res))
	want := `[{"serialNumber":"urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79","version":"1","created_at":"2024-01-01T12:00:00Z","cryptoStats":{"cryptoAssets":{"total":3,"algorithms":{"total":1},"certificates":{"total":1},"protocols":{"total":1},"relatedCryptoMaterials":{"total":0}}}}]` + "\n"
	require.Equal(t, want, buf.String())
}

// Legacy mode (no `limit`) is exactly main's behaviour: an object without crypto-stats
// metadata is skipped, with a warning in the log, and the call still succeeds. The
// legacy caller (Core) NPE-skips a null `cryptoStats` at DEBUG and moves its watermark
// past the entry, so surfacing such an object as a warning entry would turn a loud skip
// into a silent permanent miss. Warnings exist in paged mode only.
func TestSearch_LegacyMissingStatsIsSkipped(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	when := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{
			{Key: awsString("urn:uuid:1-1"), LastModified: &when},
			{Key: awsString("urn:uuid:2-1"), LastModified: &when},
		},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("urn:uuid:1-1")}).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(1), ContentType: aws.String("application/json"), LastModified: &when,
		Metadata: map[string]string{store.MetaVersionKey: "1"},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("urn:uuid:2-1")}).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(1), ContentType: aws.String("application/json"), LastModified: &when,
		Metadata: map[string]string{store.MetaCryptoStatsKey: "{}"},
	}, nil)

	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)

	res, err := svc.Search(context.Background(), when.Unix()-1, 0)
	require.NoError(t, err)
	require.Len(t, res, 1, "the object without statistics is skipped, the other one is returned")
	require.Equal(t, "urn:uuid:2", res[0].SerialNumber)
	require.Empty(t, res[0].Warnings, "legacy mode never sets warnings")

	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(res))
	require.Equal(t, `[{"serialNumber":"urn:uuid:2","version":"1","created_at":"2024-01-01T12:00:00Z","cryptoStats":{"cryptoAssets":{"total":0,"algorithms":{"total":0},"certificates":{"total":0},"protocols":{"total":0},"relatedCryptoMaterials":{"total":0}}}}]`+"\n", buf.String())
}

// Legacy mode fails the call on unparseable statistics, as main did: the caller gets an
// error it can retry rather than an entry whose statistics silently read as null.
func TestSearch_LegacyInvalidStatsFailsCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	when := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{{Key: awsString("urn:uuid:1-1"), LastModified: &when}},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(1), ContentType: aws.String("application/json"), LastModified: &when,
		Metadata: map[string]string{store.MetaCryptoStatsKey: "not json"},
	}, nil)

	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)

	_, err = svc.Search(context.Background(), when.Unix()-1, 0)
	require.EqualError(t, err, "unmarshaling json failed")
}

// created_at comes from the HEAD Last-Modified header. A store that answers HEAD without
// one (aws.ToTime then yields the zero time) must not produce a year-1 created_at: the
// LIST timestamp of the same object is used instead, truncated to RFC 3339 seconds.
func TestSearch_LegacyCreatedAtFallsBackToListingClock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	when := time.Date(2024, 1, 1, 12, 0, 0, 500*int(time.Millisecond), time.UTC)
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{{Key: awsString("urn:uuid:1-1"), LastModified: &when}},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(&s3.HeadObjectOutput{
		ContentLength: aws.Int64(1), ContentType: aws.String("application/json"), LastModified: nil,
		Metadata: map[string]string{store.MetaCryptoStatsKey: "{}"},
	}, nil)

	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)

	res, err := svc.Search(context.Background(), when.Unix()-1, 0)
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, "2024-01-01T12:00:00Z", res[0].Timestamp)
}
