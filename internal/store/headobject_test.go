package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OmniTrustILM/cbom-repository/internal/store"
	mockS3 "github.com/OmniTrustILM/cbom-repository/internal/store/mock"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestStore_GetHeadObject(t *testing.T) {
	testTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	testMetadata := map[string]string{
		"version":      "v1",
		"crypto-stats": "encrypted",
	}

	tests := []struct {
		name      string
		key       string
		setupMock func(*mockS3.MockS3Contract)
		wantHead  store.HeadObject
		wantErr   error
	}{
		{
			name: "success - valid head object",
			key:  "test-key",
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().
					HeadObject(gomock.Any(), gomock.Any()).
					DoAndReturn(func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
						require.Equal(t, "test-bucket", *input.Bucket)
						require.Equal(t, "test-key", *input.Key)
						return &s3.HeadObjectOutput{
							ContentLength: aws.Int64(1024),
							ContentType:   aws.String("application/json"),
							LastModified:  aws.Time(testTime),
							Metadata:      testMetadata,
						}, nil
					})
			},
			wantHead: store.HeadObject{
				ContentLength: 1024,
				ContentType:   "application/json",
				LastModified:  testTime,
				Metadata:      testMetadata,
			},
			wantErr: nil,
		},
		{
			name: "error - NoSuchKey",
			key:  "non-existent-key",
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().
					HeadObject(gomock.Any(), gomock.Any()).
					Return(nil, &types.NoSuchKey{
						Message: aws.String("key does not exist"),
					})
			},
			wantHead: store.HeadObject{},
			wantErr:  store.ErrNotFound,
		},
		{
			name: "error - NotFound",
			key:  "not-found-key",
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().
					HeadObject(gomock.Any(), gomock.Any()).
					Return(nil, &types.NotFound{
						Message: aws.String("not found"),
					})
			},
			wantHead: store.HeadObject{},
			wantErr:  store.ErrNotFound,
		},
		{
			name: "error - generic s3 error",
			key:  "error-key",
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().
					HeadObject(gomock.Any(), gomock.Any()).
					Return(nil, errors.New("s3 service error"))
			},
			wantHead: store.HeadObject{},
			wantErr:  errors.New("s3 service error"),
		},
		{
			name: "error - nil response without error",
			key:  "nil-response-key",
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().
					HeadObject(gomock.Any(), gomock.Any()).
					Return(nil, nil)
			},
			wantHead: store.HeadObject{},
			wantErr:  errors.New("`s3.HeadObject() returned nil result without error"),
		},
		{
			name: "success - empty metadata",
			key:  "empty-metadata-key",
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().
					HeadObject(gomock.Any(), gomock.Any()).
					Return(&s3.HeadObjectOutput{
						ContentLength: aws.Int64(512),
						ContentType:   aws.String("text/plain"),
						LastModified:  aws.Time(testTime),
						Metadata:      map[string]string{},
					}, nil)
			},
			wantHead: store.HeadObject{
				ContentLength: 512,
				ContentType:   "text/plain",
				LastModified:  testTime,
				Metadata:      map[string]string{},
			},
			wantErr: nil,
		},
		{
			name: "success - nil optional fields tolerated",
			key:  "nil-optional-fields-key",
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().
					HeadObject(gomock.Any(), gomock.Any()).
					Return(&s3.HeadObjectOutput{
						ContentLength: nil,
						ContentType:   nil,
						LastModified:  nil,
						Metadata:      map[string]string{},
					}, nil)
			},
			// Nil optional fields must not panic; they decode to their zero values.
			wantHead: store.HeadObject{Metadata: map[string]string{}},
			wantErr:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockS3Client := mockS3.NewMockS3Contract(ctrl)
			mockS3Manager := mockS3.NewMockS3Manager(ctrl)

			tt.setupMock(mockS3Client)

			cfg := store.Config{
				Bucket: "test-bucket",
				Region: "us-east-1",
			}

			s := store.New(cfg, mockS3Client, mockS3Manager)
			ctx := context.Background()

			gotHead, gotErr := s.GetHeadObject(ctx, tt.key)

			if tt.wantErr != nil {
				require.Error(t, gotErr)
				if errors.Is(tt.wantErr, store.ErrNotFound) {
					require.Equal(t, tt.wantErr.Error(), gotErr.Error())
				}
			} else {
				require.NoError(t, gotErr)
			}

			require.Equal(t, tt.wantHead, gotHead)
		})
	}
}
