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

func TestStore_Search(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		after     int64
		setupMock func(*mockS3.MockS3Contract)
		want      []store.ObjectInfo
		wantErr   string
	}{
		{
			name:  "returns key and LastModified of objects strictly after the timestamp, in listing order",
			after: base.Unix(),
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
					Contents: []types.Object{
						{Key: aws.String("urn:uuid:b-1"), LastModified: aws.Time(base.Add(2 * time.Second))},
						{Key: aws.String("urn:uuid:a-1"), LastModified: aws.Time(base)},                             // == after → excluded
						{Key: aws.String("urn:uuid:c-1"), LastModified: aws.Time(base.Add(500 * time.Millisecond))}, // sub-second later → included (legacy nanosecond compare)
						{Key: aws.String("urn:uuid:d-1"), LastModified: aws.Time(base.Add(-time.Second))},
					},
				}, nil)
			},
			want: []store.ObjectInfo{
				{Key: "urn:uuid:b-1", LastModified: base.Add(2 * time.Second)},
				{Key: "urn:uuid:c-1", LastModified: base.Add(500 * time.Millisecond)},
			},
		},
		{
			name:  "aggregates across pages",
			after: 0,
			setupMock: func(m *mockS3.MockS3Contract) {
				gomock.InOrder(
					m.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
						Contents:              []types.Object{{Key: aws.String("urn:uuid:a-1"), LastModified: aws.Time(base)}},
						IsTruncated:           aws.Bool(true),
						NextContinuationToken: aws.String("token-1"),
					}, nil),
					m.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
						func(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
							require.Equal(t, "token-1", aws.ToString(in.ContinuationToken))
							return &s3.ListObjectsV2Output{
								Contents: []types.Object{{Key: aws.String("urn:uuid:b-1"), LastModified: aws.Time(base)}},
							}, nil
						}),
				)
			},
			want: []store.ObjectInfo{
				{Key: "urn:uuid:a-1", LastModified: base},
				{Key: "urn:uuid:b-1", LastModified: base},
			},
		},
		{
			name:  "skips listed objects without key or LastModified instead of panicking",
			after: 0,
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
					Contents: []types.Object{
						{Key: nil, LastModified: aws.Time(base)},
						{Key: aws.String("urn:uuid:no-time-1"), LastModified: nil},
						{Key: aws.String("urn:uuid:ok-1"), LastModified: aws.Time(base)},
					},
				}, nil)
			},
			want: []store.ObjectInfo{{Key: "urn:uuid:ok-1", LastModified: base}},
		},
		{
			name:  "empty bucket yields an empty, non-nil slice",
			after: 0,
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{}, nil)
			},
			want: []store.ObjectInfo{},
		},
		{
			name:  "listing error is reported",
			after: 0,
			setupMock: func(m *mockS3.MockS3Contract) {
				m.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("boom"))
			},
			wantErr: "obtaining next page failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s3Mock := mockS3.NewMockS3Contract(ctrl)
			tt.setupMock(s3Mock)

			st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
			got, err := st.Search(context.Background(), tt.after)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
