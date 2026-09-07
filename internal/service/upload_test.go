package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/OmniTrustILM/cbom-repository/internal/store"
	mockS3 "github.com/OmniTrustILM/cbom-repository/internal/store/mock"
	manager "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/kaptinlin/jsonschema"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestUploadInputChecks(t *testing.T) {
	testCases := map[string]struct {
		input   cdx.BOM
		wantErr bool
	}{
		"empty struct": {
			input:   cdx.BOM{},
			wantErr: true,
		},
		"success": {
			input: cdx.BOM{
				SpecVersion:  cdx.SpecVersion1_6,
				BOMFormat:    cdx.BOMFormat,
				SerialNumber: "urn:uuid:550e8400-e29b-11d4-a716-446655440000",
			},
			wantErr: false,
		},
		"bad serial": {
			input: cdx.BOM{
				SpecVersion:  cdx.SpecVersion1_6,
				BOMFormat:    cdx.BOMFormat,
				SerialNumber: "urn:isbn:0451450523",
			},
			wantErr: true,
		},
		"lower spec version": {
			input: cdx.BOM{
				SpecVersion:  cdx.SpecVersion1_4,
				BOMFormat:    cdx.BOMFormat,
				SerialNumber: "urn:uuid:550e8400-e29b-11d4-a716-446655440000",
			},
			wantErr: true,
		},
		"unexpected bom format": {
			input: cdx.BOM{
				SpecVersion:  cdx.SpecVersion1_6,
				BOMFormat:    "something",
				SerialNumber: "urn:uuid:550e8400-e29b-11d4-a716-446655440000",
			},
			wantErr: true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := uploadInputChecks(tc.input, "1.6")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestURNValid(t *testing.T) {
	testCases := map[string]struct {
		input string
		want  bool
	}{
		"valid-v1-mac": {
			input: "urn:uuid:550e8400-e29b-11d4-a716-446655440000",
			want:  true,
		},
		"valid-v2-dce": {
			input: "urn:uuid:6ba7b810-9dad-21d1-80b4-00c04fd430c8",
			want:  true,
		},
		"valid-v3-md5": {
			input: "urn:uuid:c30c6b7b-107b-34df-b214-eb13f774fffa",
			want:  true,
		},
		"valid-v4-random": {
			input: "urn:uuid:9b2c51f2-6d3a-4c9a-8f3f-3a2c5f5a9c9d",
			want:  true,
		},
		"valid-v5-sha1": {
			input: "urn:uuid:2e93abd6-3a33-5e7d-b0c3-97c0d57b6d43",
			want:  true,
		},
		"valid-v6-time-based": {
			input: "urn:uuid:1ec9414c-232a-6b00-b3c8-9e8bde5ac4b8",
			want:  true,
		},
		"valid-v7-time-ordered": {
			input: "urn:uuid:019976ff-0e57-7044-8525-2a01f8e13736",
			want:  true,
		},
		"valid-v8-custom-format": {
			input: "urn:uuid:123e4567-e89b-89d3-a456-426614174000",
			want:  true,
		},
		"malformed-1": {
			input: "uuid:ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-2": {
			input: ":uuid:ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-3": {
			input: "xyz:uuid:ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-4": {
			input: "urn::uuid:ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-5": {
			input: "urn::ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-6": {
			input: "urn:ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-7": {
			input: "urn:uuid::ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-8": {
			input: "urn:md5:ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"malformed-9": {
			input: "uuid:urn:ecc69056-a50b-4c4c-9f25-fbb35af91f4d",
			want:  false,
		},
		"not-valid-uuid": {
			input: "urn:uuid:ecc69056-ax0b-4c4c-0f25-f0035af91f4d",
			want:  false,
		},
		"missing-uuid": {
			input: "urn:uuid:",
			want:  false,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got := URNValid(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}

// -----------------------------------------------------------------------------
// Uploaded BOM tests (merged from upload_bom_test.go)
// These tests exercise UploadBOM behavior using gomock for the store S3 client and manager.
// -----------------------------------------------------------------------------

// minimalBOM is a minimal CycloneDX document used to build test payloads. Optional
// fields are omitted when empty so the helpers can produce documents with or without
// a serialNumber/version.
type minimalBOM struct {
	BomFormat    string `json:"bomFormat"`
	SpecVersion  string `json:"specVersion"`
	SerialNumber string `json:"serialNumber,omitempty"`
	Version      int    `json:"version,omitempty"`
	Extra        string `json:"extra,omitempty"`
}

func marshalMinimalBOM(t *testing.T, bom minimalBOM) string {
	t.Helper()
	b, err := json.Marshal(bom)
	require.NoError(t, err)
	return string(b)
}

func minimalBOMJSON(t *testing.T, withSerial bool, serial string, version int, extra bool) string {
	t.Helper()
	if extra {
		return marshalMinimalBOM(t, minimalBOM{BomFormat: "CycloneDX", SpecVersion: "1.6", Extra: "x"})
	}
	return minimalBOMJSONVersion(t, "1.6", withSerial, serial, version)
}

func minimalBOMJSONVersion(t *testing.T, specVersion string, withSerial bool, serial string, version int) string {
	t.Helper()
	bom := minimalBOM{BomFormat: "CycloneDX", SpecVersion: specVersion}
	if withSerial {
		bom.SerialNumber = serial
		if version > 0 {
			bom.Version = version
		}
	}
	return marshalMinimalBOM(t, bom)
}

func TestUploadBOM_Success_MissingSerialGeneratesAndStores(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// HeadObject returns NotFound -> no key exists
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), &types.NotFound{}).AnyTimes()
	// Upload called twice
	s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).Return(&manager.UploadObjectOutput{}, nil).Times(2)

	rc := io.NopCloser(strings.NewReader(minimalBOMJSON(t, false, "", 0, false)))
	res, err := svc.UploadBOM(context.Background(), rc, "1.6")
	require.NoError(t, err)
	require.NotEmpty(t, res.SerialNumber)
	require.Equal(t, 1, res.Version)
}

func TestUploadBOM_Conflict_AlreadyExists(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	serial := "urn:uuid:550e8400-e29b-11d4-a716-446655440000"
	rc := io.NopCloser(strings.NewReader(minimalBOMJSON(t, true, serial, 2, false)))

	// HeadObject returns nil error -> exists true
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(&s3.HeadObjectOutput{}, nil)

	res, err := svc.UploadBOM(context.Background(), rc, "1.6")
	require.ErrorIs(t, err, ErrAlreadyExists)
	require.Equal(t, serial, res.SerialNumber)
	require.Equal(t, 2, res.Version)
}

func TestUploadBOM_InvalidJSONAndSchemaMismatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// invalid JSON
	rc := io.NopCloser(strings.NewReader("{ not json }"))
	_, err = svc.UploadBOM(context.Background(), rc, "1.6")
	require.Error(t, err)

	// schema mismatch: extra property not allowed
	rc2 := io.NopCloser(strings.NewReader(minimalBOMJSON(t, false, "", 0, true)))
	_, err = svc.UploadBOM(context.Background(), rc2, "1.6")
	require.ErrorIs(t, err, ErrValidation)
}

func TestUploadBOM_VersionIncrementHasOriginal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// ListObjectsV2 should return original and version 1 so new version becomes 2
	now := time.Now()
	serial := "urn:uuid:550e8400-e29b-11d4-a716-446655440000"
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{
			{Key: awsString(serial + "-original"), LastModified: &now},
			{Key: awsString(serial + "-1"), LastModified: &now},
		},
	}, nil)

	// Upload should be called once for modified version
	s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).Return(&manager.UploadObjectOutput{}, nil).Times(1)

	// Prepare BOM with serial only and no version (version defaults to 0 -> <1)
	rc := io.NopCloser(strings.NewReader("{\n  \"bomFormat\": \"CycloneDX\",\n  \"specVersion\": \"1.6\",\n  \"serialNumber\": \"" + serial + "\"\n}"))

	res, err := svc.UploadBOM(context.Background(), rc, "1.6")
	require.NoError(t, err)
	require.Equal(t, serial, res.SerialNumber)
	require.Equal(t, 2, res.Version)
}

func TestUploadBOM_SerialVersionSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// HeadObject returns NotFound -> key does not exist
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), &types.NotFound{})
	// Upload will be called once to store original BOM
	s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).Return(&manager.UploadObjectOutput{}, nil).Times(1)

	serial := "urn:uuid:550e8400-e29b-11d4-a716-446655440000"
	rc := io.NopCloser(strings.NewReader("{\n  \"bomFormat\": \"CycloneDX\",\n  \"specVersion\": \"1.6\",\n  \"serialNumber\": \"" + serial + "\",\n  \"version\": 3\n}"))

	res, err := svc.UploadBOM(context.Background(), rc, "1.6")
	require.NoError(t, err)
	require.Equal(t, serial, res.SerialNumber)
	require.Equal(t, 3, res.Version)
}

func TestUploadBOM_HeadObjectErrorPropagated(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// HeadObject returns some unexpected error
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), errors.New("boom"))

	serial := "urn:uuid:550e8400-e29b-11d4-a716-446655440000"
	rc := io.NopCloser(strings.NewReader("{\n  \"bomFormat\": \"CycloneDX\",\n  \"specVersion\": \"1.6\",\n  \"serialNumber\": \"" + serial + "\",\n  \"version\": 1\n}"))

	_, err = svc.UploadBOM(context.Background(), rc, "1.6")
	require.Error(t, err)
}

func TestKnownCdxVersion(t *testing.T) {
	// valid versions
	v, err := knownCdxVersion("1.0")
	require.NoError(t, err)
	require.Equal(t, cdx.SpecVersion1_0, v)

	v, err = knownCdxVersion("1.6")
	require.NoError(t, err)
	require.Equal(t, cdx.SpecVersion1_6, v)

	v, err = knownCdxVersion("1.7")
	require.NoError(t, err)
	require.Equal(t, cdx.SpecVersion1_7, v)

	// unknown version
	_, err = knownCdxVersion("9.9")
	require.Error(t, err)
}

func TestKnownCdxVersion_More(t *testing.T) {
	versions := map[string]cdx.SpecVersion{
		"1.1": cdx.SpecVersion1_1,
		"1.2": cdx.SpecVersion1_2,
		"1.3": cdx.SpecVersion1_3,
		"1.4": cdx.SpecVersion1_4,
		"1.5": cdx.SpecVersion1_5,
	}
	for v, want := range versions {
		got, err := knownCdxVersion(v)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestSupportedVersionAndVersionSupported(t *testing.T) {
	// construct service with jsonSchemas map
	s := Service{}
	s.jsonSchemas = map[string]*jsonschema.Schema{"1.6": nil, "1.4": nil}

	sv := s.SupportedVersion()
	require.Equal(t, []string{"1.4", "1.6"}, sv)
	require.True(t, s.VersionSupported("1.6"))
	require.False(t, s.VersionSupported("1.5"))
}

// helper to create *string for aws types
func awsString(s string) *string { return &s }

func TestCycloneDX17_ReEncodePreserves17Fields(t *testing.T) {
	// Guards that cyclonedx-go's decode->encode round-trip — the same
	// NewBOMDecoder/NewBOMEncoder(JSON) API pair the upload re-encode path
	// uses (NewBOMDecoder in UploadBOM, NewBOMEncoder in the uploadCase* helpers) —
	// preserves 1.7-only fields. A dependency-capability guard: if cyclonedx-go stopped modelling
	// a 1.7 field, the re-encode upload path would silently drop it while
	// schema validation (run on the original bytes) still passed.
	// "ECDH" is a documented valid algorithmFamily value (bom-1.7 schema examples).
	input := `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.7",
  "components": [
    {
      "type": "cryptographic-asset",
      "name": "example-alg",
      "cryptoProperties": {
        "assetType": "algorithm",
        "algorithmProperties": { "algorithmFamily": "ECDH" }
      }
    }
  ]
}`

	var bom cdx.BOM
	require.NoError(t, cdx.NewBOMDecoder(strings.NewReader(input), cdx.BOMFileFormatJSON).Decode(&bom))

	var out bytes.Buffer
	require.NoError(t, cdx.NewBOMEncoder(&out, cdx.BOMFileFormatJSON).Encode(&bom))

	// Decode the re-encoded output back and assert the 1.7-only field survived
	// at its exact path (stronger than a whole-document substring match).
	var rt cdx.BOM
	require.NoError(t, cdx.NewBOMDecoder(bytes.NewReader(out.Bytes()), cdx.BOMFileFormatJSON).Decode(&rt))
	require.NotNil(t, rt.Components)
	require.Len(t, *rt.Components, 1)
	cp := (*rt.Components)[0].CryptoProperties
	require.NotNil(t, cp)
	require.NotNil(t, cp.AlgorithmProperties)
	require.Equal(t, "ECDH", cp.AlgorithmProperties.AlgorithmFamily)
}

func TestUploadBOM_MissingValidatorIsValidationError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// "1.5" is known to knownCdxVersion but has no compiled schema (only 1.6/1.7 registered).
	// A body honestly declaring specVersion 1.5 must yield ErrValidation (400), never a bare 500-class error.
	rc := io.NopCloser(strings.NewReader("{\n  \"bomFormat\": \"CycloneDX\",\n  \"specVersion\": \"1.5\"\n}"))
	_, err = svc.UploadBOM(context.Background(), rc, "1.5")
	require.ErrorIs(t, err, ErrValidation)
}

func TestUploadBOM_UnknownSpecVersionIsValidationError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// specVersion "1.8" is unknown to cyclonedx-go v0.11.0 -> Decode fails.
	// It must be classified as a validation error (400), not a bare error (500).
	rc := io.NopCloser(strings.NewReader("{\n  \"bomFormat\": \"CycloneDX\",\n  \"specVersion\": \"1.8\"\n}"))
	_, err = svc.UploadBOM(context.Background(), rc, "1.7")
	require.ErrorIs(t, err, ErrValidation)
}

func TestUploadBOM_1_7_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// serial-number-missing 1.7 doc -> generates URN, stores original + modified (2 uploads)
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), &types.NotFound{}).AnyTimes()
	s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).Return(&manager.UploadObjectOutput{}, nil).Times(2)

	rc := io.NopCloser(strings.NewReader(minimalBOMJSONVersion(t, "1.7", false, "", 0)))
	res, err := svc.UploadBOM(context.Background(), rc, "1.7")
	require.NoError(t, err)
	require.NotEmpty(t, res.SerialNumber)
	require.Equal(t, 1, res.Version)
}

func TestUploadBOM_VersionMismatch_DeclaredVsBody(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// declared 1.6, body says 1.7 -> rejected at cross-check (400) before schema validation
	rc := io.NopCloser(strings.NewReader(minimalBOMJSONVersion(t, "1.7", false, "", 0)))
	_, err = svc.UploadBOM(context.Background(), rc, "1.6")
	require.ErrorIs(t, err, ErrValidation)

	// declared 1.7, body says 1.6 -> symmetric rejection (400)
	rc2 := io.NopCloser(strings.NewReader(minimalBOMJSONVersion(t, "1.6", false, "", 0)))
	_, err = svc.UploadBOM(context.Background(), rc2, "1.7")
	require.ErrorIs(t, err, ErrValidation)
}

func TestUploadBOM_1_6ShapedContentDeclared1_7_Accepted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// 1.6-style content (a plain library component, valid in both 1.6 and 1.7) that
	// honestly declares specVersion 1.7. 1.7 is a backward-compatible superset, so this
	// is valid 1.7 and must be accepted. Distinct from the minimal happy-path fixture.
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), &types.NotFound{}).AnyTimes()
	s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).Return(&manager.UploadObjectOutput{}, nil).Times(2)

	body := `{"bomFormat":"CycloneDX","specVersion":"1.7","components":[{"type":"library","name":"example-lib"}]}`
	rc := io.NopCloser(strings.NewReader(body))
	_, err = svc.UploadBOM(context.Background(), rc, "1.7")
	require.NoError(t, err)
}

func TestUploadBOM_1_7_InvalidSchema(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	// Declared 1.7, body declares 1.7 (passes the cross-check) but has a disallowed
	// top-level property -> fails 1.7 schema validation (root additionalProperties:false).
	rc := io.NopCloser(strings.NewReader(`{"bomFormat":"CycloneDX","specVersion":"1.7","extra":"x"}`))
	_, err = svc.UploadBOM(context.Background(), rc, "1.7")
	require.ErrorIs(t, err, ErrValidation)
}

// TestUploadBOM_AutodetectsVersionWhenUndeclared proves that when no version is
// declared (empty string — the path taken when a client omits the Content-Type
// `version` parameter, as core's CbomRepositoryClient does) the service detects the
// version from the document's own specVersion and validates against it. Without this,
// an undeclared 1.7 upload would be rejected against a fixed default version.
func TestUploadBOM_AutodetectsVersionWhenUndeclared(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), &types.NotFound{}).AnyTimes()
	s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).Return(&manager.UploadObjectOutput{}, nil).Times(2)

	rc := io.NopCloser(strings.NewReader(minimalBOMJSONVersion(t, "1.7", false, "", 0)))
	res, err := svc.UploadBOM(context.Background(), rc, "")
	require.NoError(t, err)
	require.NotEmpty(t, res.SerialNumber)
	require.Equal(t, 1, res.Version)
}

// TestUploadBOM_AutodetectUnsupportedVersionIsValidationError proves that an
// auto-detected version with no compiled schema (1.5 is known to the decoder but only
// 1.6/1.7 are registered) yields ErrValidation (400), not a bare 500.
func TestUploadBOM_AutodetectUnsupportedVersionIsValidationError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	rc := io.NopCloser(strings.NewReader(`{"bomFormat":"CycloneDX","specVersion":"1.5"}`))
	_, err = svc.UploadBOM(context.Background(), rc, "")
	require.ErrorIs(t, err, ErrValidation)
}

// On the auto-detect path (no declared version), a document with no specVersion
// decodes to the zero SpecVersion whose String() is the raw "SpecVersion(0)". It
// must be rejected as a validation error (400) with a clean message, not leak the
// stringer output to the client.
func TestUploadBOM_AutodetectMissingSpecVersionIsValidationError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	rc := io.NopCloser(strings.NewReader(`{"bomFormat":"CycloneDX"}`))
	_, err = svc.UploadBOM(context.Background(), rc, "")
	require.ErrorIs(t, err, ErrValidation)
	require.NotContains(t, err.Error(), "SpecVersion(0)")
}

// ctxErrReadCloser is an io.ReadCloser whose Read always fails with the given
// error, simulating a transport/read failure (client disconnect, context
// deadline) mid-body-read.
type ctxErrReadCloser struct{ err error }

func (r ctxErrReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r ctxErrReadCloser) Close() error             { return nil }

// A read failure mid-decode must NOT be reclassified as a client validation
// error (400); it surfaces as a non-ErrValidation error the handler maps to 5xx.
func TestUploadBOM_ReadFailureNotValidationError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	_, err = svc.UploadBOM(context.Background(), ctxErrReadCloser{err: context.DeadlineExceeded}, "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrValidation)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestSchemasSelfContained asserts that every embedded bom-*.schema.json resolves
// all of its external `$ref`s from the vendored sub-schemas alone, leaving nothing
// to be fetched over the network. This is network-independent: if a vendored
// sub-schema is removed or a new external reference is introduced, the guarantee
// breaks here (fail closed) instead of silently degrading validation to fail-open
// at runtime when the network is unavailable.
func TestSchemasSelfContained(t *testing.T) {
	for version, filename := range versionToEmbeddedFileMapping {
		compiler := jsonschema.NewCompiler()
		// Mirror production (see New): block network loaders so this test cannot be
		// fooled into passing by a networked CI runner silently fetching a missing
		// sub-schema from cyclonedx.org — the failure it exists to catch.
		compiler.RegisterLoader("http", noNetworkSchemaLoader)
		compiler.RegisterLoader("https", noNetworkSchemaLoader)
		for _, sub := range subSchemaFiles {
			sb, err := schemas.ReadFile(sub)
			require.NoErrorf(t, err, "reading vendored sub-schema %s", sub)
			_, err = compiler.Compile(sb)
			require.NoErrorf(t, err, "compiling vendored sub-schema %s", sub)
		}
		b, err := schemas.ReadFile(filename)
		require.NoError(t, err)
		schema, err := compiler.Compile(b)
		require.NoError(t, err)
		require.Emptyf(t, schema.UnresolvedReferenceURIs(),
			"schema %s (version %s) has unresolved external references; a sub-schema is likely missing from subSchemaFiles",
			filename, version)
	}
}

// TestSchemaCompilerRefusesNetworkFetch proves that the no-network loader wired up
// by New actually blocks external `$ref` resolution over the wire. A schema whose
// `$ref` points at an unvendored cyclonedx.org URL must never resolve silently: the
// loader either fails compilation with our sentinel error, or the reference is left
// unresolved — in no case is it fetched from the network. This makes the
// self-contained guarantee robust even if a future sub-schema is forgotten.
func TestSchemaCompilerRefusesNetworkFetch(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	compiler.RegisterLoader("http", noNetworkSchemaLoader)
	compiler.RegisterLoader("https", noNetworkSchemaLoader)

	// References a sub-schema that is deliberately NOT vendored and NOT pre-registered,
	// so the only way to resolve it would be a network fetch.
	root := []byte(`{"$id":"http://example.test/root.json","$ref":"http://cyclonedx.org/schema/does-not-exist-999.schema.json"}`)
	schema, err := compiler.Compile(root)
	if err != nil {
		require.ErrorContains(t, err, "refused to fetch",
			"unvendored $ref must fail through the no-network loader, not some other path")
		return
	}
	require.NotEmpty(t, schema.UnresolvedReferenceURIs(),
		"unvendored external $ref was resolved without going through the no-network loader — a network fetch was not blocked")
}

// TestUploadBOM_Rejects17InvalidAlgorithmFamily proves the vendored
// cryptography-defs.schema.json is actually applied during validation: the 1.7
// `algorithmFamily` field is constrained solely by that sub-schema's enum, so a
// bogus value must be rejected. Without local resolution of that external ref,
// validation would fail-open and accept it.
func TestUploadBOM_Rejects17InvalidAlgorithmFamily(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)
	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := New(st, Config{CheckOnFetch: false})
	require.NoError(t, err)

	bogus := `{"bomFormat":"CycloneDX","specVersion":"1.7","components":[{"type":"cryptographic-asset","name":"x","cryptoProperties":{"assetType":"algorithm","algorithmProperties":{"primitive":"key-agree","algorithmFamily":"BOGUS-NOT-A-REAL-FAMILY-999"}}}]}`
	_, err = svc.UploadBOM(context.Background(), io.NopCloser(strings.NewReader(bogus)), "1.7")
	require.ErrorIs(t, err, ErrValidation)
}

// Every stored object — the normalised version and the `original` copy alike — carries
// the counting-algorithm version next to its statistics, so consumers can tell a
// nested (v2) count from a legacy shallow one.
func TestUploadBOM_WritesCryptoStatsVersionMetadata(t *testing.T) {
	serial := "urn:uuid:550e8400-e29b-11d4-a716-446655440000"
	cases := map[string]struct {
		body        string
		listVersion bool
		uploads     int
	}{
		"missing serial stores original and version 1": {body: minimalBOMJSON(t, false, "", 0, false), uploads: 2},
		"serial only gets the next version":            {body: minimalBOMJSONVersion(t, "1.6", true, serial, 0), listVersion: true, uploads: 1},
		"serial and version stored as-is":              {body: minimalBOMJSONVersion(t, "1.6", true, serial, 3), uploads: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s3Mock := mockS3.NewMockS3Contract(ctrl)
			s3Manager := mockS3.NewMockS3Manager(ctrl)
			svc, err := New(store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager), Config{})
			require.NoError(t, err)

			s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), &types.NotFound{}).AnyTimes()
			if tc.listVersion {
				s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{Contents: []types.Object{}}, nil)
			}
			s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, in *manager.UploadObjectInput, _ ...func(*manager.Options)) (*manager.UploadObjectOutput, error) {
					require.Equal(t, CryptoStatsVersion, in.Metadata[store.MetaCryptoStatsVersionKey], "key %s", *in.Key)
					require.NotEmpty(t, in.Metadata[store.MetaCryptoStatsKey])
					require.NotEmpty(t, in.Metadata[store.MetaVersionKey])
					return &manager.UploadObjectOutput{}, nil
				}).Times(tc.uploads)

			_, err = svc.UploadBOM(context.Background(), io.NopCloser(strings.NewReader(tc.body)), "1.6")
			require.NoError(t, err)
		})
	}
}

// deeplyNestedBOMJSON returns a valid CycloneDX 1.6 document whose `components` nest
// `depth` levels deep, every level a crypto asset.
func deeplyNestedBOMJSON(t *testing.T, depth int) string {
	t.Helper()
	node := map[string]any{
		"type": "cryptographic-asset", "name": fmt.Sprintf("level-%d", depth),
		"cryptoProperties": map[string]any{"assetType": "algorithm"},
	}
	for level := depth - 1; level >= 1; level-- {
		node = map[string]any{
			"type": "cryptographic-asset", "name": fmt.Sprintf("level-%d", level),
			"cryptoProperties": map[string]any{"assetType": "algorithm"},
			"components":       []any{node},
		}
	}
	b, err := json.Marshal(map[string]any{
		"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
		"components": []any{node},
	})
	require.NoError(t, err)
	return string(b)
}

// A document nested past the walk's depth bound is stored with the truncation marker, so
// paged search can tell consumers its statistics are a lower bound. A document within
// the bound carries no marker at all — the key's absence is the "counts are complete"
// signal, and adding it with value "false" to every object would be noise.
func TestUploadBOM_MarksTruncatedCryptoStats(t *testing.T) {
	cases := map[string]struct {
		body          string
		wantTruncated bool
	}{
		"nested past the depth bound": {body: deeplyNestedBOMJSON(t, maxComponentDepth+5), wantTruncated: true},
		"an ordinary document":        {body: deeplyNestedBOMJSON(t, 3)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s3Mock := mockS3.NewMockS3Contract(ctrl)
			s3Manager := mockS3.NewMockS3Manager(ctrl)
			svc, err := New(store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager), Config{})
			require.NoError(t, err)

			s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return((*s3.HeadObjectOutput)(nil), &types.NotFound{}).AnyTimes()
			// No serial number in the document: the original copy and the normalised
			// version 1 are both stored, and both must agree on the marker.
			s3Manager.EXPECT().UploadObject(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, in *manager.UploadObjectInput, _ ...func(*manager.Options)) (*manager.UploadObjectOutput, error) {
					value, present := in.Metadata[store.MetaCryptoStatsTruncatedKey]
					require.Equal(t, tc.wantTruncated, present, "key %s", *in.Key)
					if tc.wantTruncated {
						require.Equal(t, "true", value, "key %s", *in.Key)
					}
					return &manager.UploadObjectOutput{}, nil
				}).Times(2)

			_, err = svc.UploadBOM(context.Background(), io.NopCloser(strings.NewReader(tc.body)), "1.6")
			require.NoError(t, err)
		})
	}
}
