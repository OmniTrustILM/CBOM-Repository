package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/OmniTrustILM/cbom-repository/internal/health"
	"github.com/OmniTrustILM/cbom-repository/internal/service"
	"github.com/OmniTrustILM/cbom-repository/internal/store"
	mockS3 "github.com/OmniTrustILM/cbom-repository/internal/store/mock"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	pd "github.com/kodeart/go-problem/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// The cursor query parameter is validated before anything is listed: it needs `limit`,
// excludes `after`, and must be in the canonical form this service issues. Every rejection is a 400
// problem+json with a detail that names the parameter.
func TestSearch_CursorValidation(t *testing.T) {
	valid := service.NewCursor(time.Unix(1672531200, 0), "urn:uuid:a-1").String()
	tests := []struct {
		name           string
		query          string
		expectedStatus int
		expectedDetail string
	}{
		{name: "cursor with limit is accepted", query: "cursor=" + valid + "&limit=5", expectedStatus: http.StatusOK},
		{name: "cursor with the largest limit", query: "cursor=" + valid + "&limit=1000", expectedStatus: http.StatusOK},
		{name: "cursor without limit", query: "cursor=" + valid, expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'cursor' requires 'limit'"},
		{name: "cursor with after", query: "cursor=" + valid + "&after=1672531200&limit=5", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'cursor' cannot be combined with 'after'"},
		{name: "cursor with an empty after still counts as combined", query: "cursor=" + valid + "&after=&limit=5", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'cursor' cannot be combined with 'after'"},
		{name: "combination is reported before the missing limit", query: "cursor=" + valid + "&after=1", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'cursor' cannot be combined with 'after'"},
		{name: "cursor with limit zero", query: "cursor=" + valid + "&limit=0", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "cursor with limit above the maximum", query: "cursor=" + valid + "&limit=1001", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "cursor with a non-integer limit", query: "cursor=" + valid + "&limit=ten", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "malformed cursor", query: "cursor=%21%21%21&limit=5", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'cursor' is malformed"},
		{name: "cursor present but empty", query: "cursor=&limit=5", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'cursor' is malformed"},
		{name: "cursor of a foreign version", query: "cursor=" + urlSafeBase64("v9|1|urn:uuid:a-1") + "&limit=5", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'cursor' is malformed"},
		{name: "limit is validated before the cursor is decoded", query: "cursor=%21%21%21&limit=0", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
	}

	// One server for the whole matrix: a rejected request never reaches the store, so
	// the bucket is listed exactly once per accepted request.
	accepted := 0
	for _, tt := range tests {
		if tt.expectedStatus == http.StatusOK {
			accepted++
		}
	}
	ctrl := gomock.NewController(t)
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{}, nil).Times(accepted)
	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)
	server := New(Config{Prefix: "/api"}, svc, health.NewService(mockChecker{name: "storage", status: health.StatusUp}))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/bom?"+tt.query, nil)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedStatus == http.StatusOK {
				require.JSONEq(t, "[]", w.Body.String())
				require.Empty(t, w.Header().Get("Link"), "an empty page is the last page")
				return
			}
			require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
			var p pd.Problem
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
			require.Contains(t, p.Detail, tt.expectedDetail)
		})
	}
}

func urlSafeBase64(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// A run over the HTTP API: the opening page (after + limit) carries the next page as a
// Link header — a relative reference with the request path, the cursor and the same
// limit, rel="next" — the client follows it verbatim, and the last page carries no Link.
// The body of the opening page is the deliverable-1 body; the cursor is header-only.
func TestSearch_LinkHeaderRun(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	for _, prefix := range []string{"/api", "", "/custom"} {
		t.Run(fmt.Sprintf("prefix %q", prefix), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s3Mock := mockS3.NewMockS3Contract(ctrl)
			when := map[string]time.Time{
				"urn:uuid:a-1": base.Add(1 * time.Second),
				"urn:uuid:b-1": base.Add(2*time.Second + 250*time.Millisecond),
				"urn:uuid:c-1": base.Add(3 * time.Second),
			}
			listBucket(s3Mock, when, 2, 3) // two pages; HEAD for a and b, then for c
			svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
			require.NoError(t, err)
			server := New(Config{Prefix: prefix}, svc, health.NewService(mockChecker{name: "storage", status: health.StatusUp}))

			path := prefix + "/v1/bom"
			w := get(server, fmt.Sprintf("%s?after=%d&limit=2", path, base.Unix()))
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))
			body := w.Body.String()
			require.Equal(t, `[{"serialNumber":"urn:uuid:a","version":"1","created_at":"2026-09-02T10:00:01Z","cryptoStats":{"cryptoAssets":{"total":0,"algorithms":{"total":0},"certificates":{"total":0},"protocols":{"total":0},"relatedCryptoMaterials":{"total":0}}}},{"serialNumber":"urn:uuid:b","version":"1","created_at":"2026-09-02T10:00:02Z","cryptoStats":{"cryptoAssets":{"total":0,"algorithms":{"total":0},"certificates":{"total":0},"protocols":{"total":0},"relatedCryptoMaterials":{"total":0}}}}]`+"\n", body,
				"the body of the opening page is the deliverable-1 body, byte for byte")

			expectedCursor := service.NewCursor(when["urn:uuid:b-1"], "urn:uuid:b-1").String()
			require.Equal(t, fmt.Sprintf(`<bom?cursor=%s&limit=2>; rel="next"`, expectedCursor), w.Header().Get("Link"),
				"a relative-path reference: the same resource, whatever path the client reached it by")

			// Follow the Link as a client does: take the reference between < and >
			// verbatim and resolve it against the URL just requested (RFC 3986 §5.2).
			ref, err := url.Parse(linkNext(t, w.Header().Get("Link")))
			require.NoError(t, err)
			require.Equal(t, expectedCursor, ref.Query().Get("cursor"))
			require.Equal(t, "2", ref.Query().Get("limit"))
			requested, err := url.Parse(fmt.Sprintf("http://cbom.example%s?after=%d&limit=2", path, base.Unix()))
			require.NoError(t, err)
			next := requested.ResolveReference(ref)
			require.Equal(t, path, next.Path, "resolving against the request URL lands on the same route")
			require.Equal(t, fmt.Sprintf("cursor=%s&limit=2", expectedCursor), next.RawQuery)

			// The reference is relative to the *client's* URL, so it also survives an
			// ingress that strips a prefix before the request reaches this service: the
			// client keeps the prefix it used, the service never has to know it.
			proxied, err := url.Parse(fmt.Sprintf("https://ilm.example/cbom%s?after=%d&limit=2", path, base.Unix()))
			require.NoError(t, err)
			require.Equal(t, "/cbom"+path, proxied.ResolveReference(ref).Path)

			w = get(server, next.RequestURI())
			require.Equal(t, http.StatusOK, w.Code)
			var entries []service.SearchRes
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &entries))
			require.Len(t, entries, 1)
			require.Equal(t, "urn:uuid:c", entries[0].SerialNumber)
			require.Empty(t, w.Header().Get("Link"), "the last page carries no Link")
		})
	}
}

// The unpaged call keeps its exact wire shape: no Link header, however many entries.
func TestSearch_LegacyHasNoLinkHeader(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	when := map[string]time.Time{}
	for i := 0; i < 5; i++ {
		when[fmt.Sprintf("urn:uuid:%d-1", i)] = base.Add(time.Duration(i+1) * time.Second)
	}
	listBucket(s3Mock, when, 1, 5)
	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)
	server := New(Config{Prefix: "/api"}, svc, health.NewService(mockChecker{name: "storage", status: health.StatusUp}))

	w := get(server, fmt.Sprintf("/api/v1/bom?after=%d", base.Unix()))
	require.Equal(t, http.StatusOK, w.Code)
	var entries []service.SearchRes
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &entries))
	require.Len(t, entries, 5)
	_, hasLink := w.Header()["Link"]
	require.False(t, hasLink, "legacy mode never emits a Link header")
}

// A continued page whose listing fails is a 500, like any other search.
func TestSearch_CursorStoreErrorIsInternal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("boom"))
	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)
	server := New(Config{Prefix: "/api"}, svc, health.NewService(mockChecker{name: "storage", status: health.StatusUp}))

	cursor := service.NewCursor(time.Unix(1672531200, 0), "urn:uuid:a-1").String()
	w := get(server, "/api/v1/bom?cursor="+cursor+"&limit=5")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
}

// listBucket makes the mock list the given objects exactly `lists` times and answer
// exactly `heads` HEAD requests, each with empty statistics — pinning the fan-out a
// scenario is expected to cost.
func listBucket(s3Mock *mockS3.MockS3Contract, when map[string]time.Time, lists, heads int) {
	contents := make([]types.Object, 0, len(when))
	for key, stamp := range when {
		contents = append(contents, types.Object{Key: aws.String(key), LastModified: aws.Time(stamp)})
	}
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{Contents: contents}, nil).Times(lists)
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{
				ContentLength: aws.Int64(1), ContentType: aws.String("application/json"), LastModified: aws.Time(when[aws.ToString(in.Key)]),
				Metadata: map[string]string{store.MetaCryptoStatsKey: "{}", store.MetaCryptoStatsVersionKey: "2"},
			}, nil
		}).Times(heads)
}

func get(server Server, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

var linkNextRe = regexp.MustCompile(`^<([^>]+)>; rel="next"$`)

// linkNext extracts the reference of the single rel="next" link the header carries.
func linkNext(t *testing.T, header string) string {
	t.Helper()
	m := linkNextRe.FindStringSubmatch(header)
	require.NotNil(t, m, "Link header %q must be exactly one rel=\"next\" link", header)
	return m[1]
}
