package service_test

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is an in-memory store.S3Contract for protocol-level tests: it lists in UTF-8
// key order with MaxKeys/ContinuationToken pagination (like S3), reports LastModified
// at full precision from LIST (like MinIO) and truncated to seconds from HEAD (like the
// HTTP Last-Modified header), and lets a test inject objects between logical listings
// to simulate concurrent uploads.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string]fakeObject
	pageSize int
	// beforeListing runs at the start of every logical listing (first LIST call of a
	// paginated listing), before the lock is taken.
	beforeListing func(listing int)
	listings      int
	listCalls     int
	headCalls     int
	headErr       map[string]error
	// headTime overrides the LastModified HeadObject reports for a key, independent of
	// the value LIST reports for the same key. It simulates HEAD and LIST disagreeing
	// on the second (e.g. an unguarded overwrite landing between LIST and HEAD, or a
	// store whose HTTP-date rounding differs from its listing precision).
	headTime map[string]time.Time
}

type fakeObject struct {
	lastModified time.Time
	metadata     map[string]string
}

func newFakeS3(pageSize int) *fakeS3 {
	return &fakeS3{objects: map[string]fakeObject{}, pageSize: pageSize, headErr: map[string]error{}, headTime: map[string]time.Time{}}
}

func (f *fakeS3) put(key string, lastModified time.Time, metadata map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{lastModified: lastModified, metadata: metadata}
}

// validStatsJSON is the crypto-stats metadata value of a healthy object: one algorithm
// asset, counted by the current (nested) walk.
const validStatsJSON = `{"cryptoAssets":{"total":1,"algorithms":{"total":1},"certificates":{"total":0},"protocols":{"total":0},"relatedCryptoMaterials":{"total":0}}}`

// putWithStats stores an object the way an upload does today: valid statistics stamped
// with the counting-algorithm version, so paged search reports no warnings for it.
func (f *fakeS3) putWithStats(key string, lastModified time.Time) {
	f.put(key, lastModified, map[string]string{"version": "1", "crypto-stats": validStatsJSON, "crypto-stats-version": "2"})
}

// overwrite replaces the stored object's lastModified and keeps its metadata: the LIST
// and HEAD clocks move together, as they do when a key is overwritten in place while a
// run is in progress.
func (f *fakeS3) overwrite(key string, lastModified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[key]
	if !ok {
		panic("fakeS3.overwrite: no such key " + key)
	}
	obj.lastModified = lastModified
	f.objects[key] = obj
}

func (f *fakeS3) sortedKeys(prefix string) []string {
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if in.ContinuationToken == nil {
		f.mu.Lock()
		f.listings++
		listing := f.listings
		hook := f.beforeListing
		f.mu.Unlock()
		if hook != nil {
			hook(listing)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++

	keys := f.sortedKeys(aws.ToString(in.Prefix))
	start := 0
	if in.ContinuationToken != nil {
		for i, k := range keys {
			if k > *in.ContinuationToken {
				start = i
				break
			}
			start = len(keys)
		}
	}
	end := min(start+f.pageSize, len(keys))

	out := &s3.ListObjectsV2Output{}
	for _, k := range keys[start:end] {
		obj := f.objects[k]
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k), LastModified: aws.Time(obj.lastModified)})
	}
	if end < len(keys) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(keys[end-1])
	}
	return out, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headCalls++
	key := aws.ToString(in.Key)
	if err, ok := f.headErr[key]; ok {
		return nil, err
	}
	obj, ok := f.objects[key]
	if !ok {
		return nil, &types.NotFound{}
	}
	lastModified := obj.lastModified
	if t, ok := f.headTime[key]; ok {
		lastModified = t
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(1),
		ContentType:   aws.String("application/json"),
		LastModified:  aws.Time(lastModified.Truncate(time.Second)),
		Metadata:      obj.metadata,
	}, nil
}

// headCallCount returns the number of HeadObject calls made so far. Reads f.headCalls
// under the same mutex HeadObject writes it under, so tests can safely snapshot the
// counter while other goroutines could in principle still be calling into the fake.
func (f *fakeS3) headCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headCalls
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[aws.ToString(in.Key)]; !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader([]byte(`{"bomFormat":"CycloneDX"}`)))}, nil
}

func (f *fakeS3) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeS3) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{}, nil
}
