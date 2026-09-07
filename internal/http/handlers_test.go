package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/cbom-repository/internal/health"
	"github.com/OmniTrustILM/cbom-repository/internal/service"
	"github.com/OmniTrustILM/cbom-repository/internal/store"
	mockS3 "github.com/OmniTrustILM/cbom-repository/internal/store/mock"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	pd "github.com/kodeart/go-problem/v2"
)

func TestHealthHandlersStatusUp(t *testing.T) {
	storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
	healthSvc := health.NewService(storageChecker)

	svc := service.Service{}
	srv := New(Config{Prefix: "api"}, svc, healthSvc)

	t.Run("health_ok", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		w := httptest.NewRecorder()
		srv.HealthHandler(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	})

	t.Run("liveness_ok", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health/liveness", nil)
		w := httptest.NewRecorder()
		srv.LivenessHandler(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("readiness_ok", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health/readiness", nil)
		w := httptest.NewRecorder()
		srv.ReadinessHandler(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestHealthHandler_OverrideReadiness(t *testing.T) {
	// Override readiness to OutOfService to force 503
	healthSvc := health.NewService(mockChecker{name: "readiness", status: health.StatusOutOfService})
	srv := New(Config{Prefix: "api"}, service.Service{}, healthSvc)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	w := httptest.NewRecorder()
	srv.HealthHandler(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestHealthHandler(t *testing.T) {
	tests := []struct {
		name           string
		healthStatus   health.Status
		expectedStatus int
	}{
		{
			name:           "health status UP",
			healthStatus:   health.StatusUp,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "health status DEGRADED",
			healthStatus:   health.StatusDegraded,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "health status DOWN",
			healthStatus:   health.StatusDown,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "health status OUT_OF_SERVICE",
			healthStatus:   health.StatusOutOfService,
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := mockChecker{name: "storage", status: tt.healthStatus, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(checker)

			cfg := Config{Port: 8080, Prefix: "/api"}
			server := New(cfg, service.Service{}, healthSvc)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			w := httptest.NewRecorder()

			server.HealthHandler(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))

			var response health.Health
			err := json.NewDecoder(w.Body).Decode(&response)
			require.NoError(t, err)
		})
	}
}

func TestLivenessHandler(t *testing.T) {
	tests := []struct {
		name           string
		healthStatus   health.Status
		expectedStatus int
	}{
		{
			name:           "liveness UP",
			healthStatus:   health.StatusUp,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "liveness DOWN",
			healthStatus:   health.StatusDown,
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := mockChecker{name: "storage", status: tt.healthStatus, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(checker)

			cfg := Config{Port: 8080, Prefix: "/api"}
			server := New(cfg, service.Service{}, healthSvc)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/health/liveness", nil)
			w := httptest.NewRecorder()

			server.LivenessHandler(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))

			var response health.Health
			err := json.NewDecoder(w.Body).Decode(&response)
			require.NoError(t, err)
		})
	}
}

func TestReadinessHandler(t *testing.T) {
	tests := []struct {
		name           string
		healthStatus   health.Status
		expectedStatus int
	}{
		{
			name:           "readiness UP",
			healthStatus:   health.StatusUp,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "readiness DOWN",
			healthStatus:   health.StatusDown,
			expectedStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := mockChecker{name: "storage", status: tt.healthStatus, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(checker)

			cfg := Config{Port: 8080, Prefix: "/api"}
			server := New(cfg, service.Service{}, healthSvc)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/health/readiness", nil)
			w := httptest.NewRecorder()

			server.ReadinessHandler(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))

			var response health.Health
			err := json.NewDecoder(w.Body).Decode(&response)
			require.NoError(t, err)
		})
	}
}

func TestHttpInfoContext(t *testing.T) {
	nextCalled := false
	expectedMethod := http.MethodGet
	expectedPath := "/test/path"

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		require.NotNil(t, r.Context())
	})

	middleware := httpInfoContext(next)

	req := httptest.NewRequest(expectedMethod, expectedPath, nil)
	w := httptest.NewRecorder()

	middleware.ServeHTTP(w, req)

	require.True(t, nextCalled)
}

func TestUpload(t *testing.T) {
	validBOM := `{
		"bomFormat": "CycloneDX",
		"specVersion": "1.6",
		"serialNumber": "urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79",
		"version": 1,
		"metadata": {
			"timestamp": "2023-01-01T00:00:00Z"
		}
	}`

	tests := []struct {
		name               string
		contentType        string
		body               string
		setupMocks         func(*mockS3.MockS3Contract, *mockS3.MockS3Manager)
		expectedStatus     int
		expectedInResponse string
	}{
		{
			name:           "unsupported content type",
			contentType:    "application/json",
			body:           validBOM,
			setupMocks:     func(s3c *mockS3.MockS3Contract, s3m *mockS3.MockS3Manager) {},
			expectedStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:           "unsupported version",
			contentType:    "application/vnd.cyclonedx+json; version=1.5",
			body:           validBOM,
			setupMocks:     func(s3c *mockS3.MockS3Contract, s3m *mockS3.MockS3Manager) {},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:        "successful upload",
			contentType: "application/vnd.cyclonedx+json; version=1.6",
			body:        validBOM,
			setupMocks: func(s3c *mockS3.MockS3Contract, s3m *mockS3.MockS3Manager) {
				s3c.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(nil, &types.NotFound{})
				s3m.EXPECT().UploadObject(gomock.Any(), gomock.Any()).Return(nil, nil)
			},
			expectedStatus:     http.StatusCreated,
			expectedInResponse: "urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79",
		},
		{
			name:        "conflict - BOM already exists",
			contentType: "application/vnd.cyclonedx+json; version=1.6",
			body:        validBOM,
			setupMocks: func(s3c *mockS3.MockS3Contract, s3m *mockS3.MockS3Manager) {
				s3c.EXPECT().HeadObject(gomock.Any(), gomock.Any()).Return(&s3.HeadObjectOutput{}, nil)
			},
			expectedStatus: http.StatusConflict,
		},
		{
			name:           "invalid BOM format",
			contentType:    "application/vnd.cyclonedx+json; version=1.6",
			body:           `{"invalid": "json"}`,
			setupMocks:     func(s3c *mockS3.MockS3Contract, s3m *mockS3.MockS3Manager) {},
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s3Mock := mockS3.NewMockS3Contract(ctrl)
			s3Manager := mockS3.NewMockS3Manager(ctrl)
			tt.setupMocks(s3Mock, s3Manager)

			st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
			svc, err := service.New(st, service.Config{CheckOnFetch: true})
			require.NoError(t, err)

			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(storageChecker)

			cfg := Config{Port: 8080, Prefix: "/api"}
			server := New(cfg, svc, healthSvc)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/bom", strings.NewReader(tt.body))
			req.Header.Set(HeaderContentType, tt.contentType)
			w := httptest.NewRecorder()

			server.Upload(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)

			if tt.expectedInResponse != "" {
				bodyStr := w.Body.String()
				require.Contains(t, bodyStr, tt.expectedInResponse)
			}
		})
	}
}

func TestMaxUploadSize(t *testing.T) {
	validBOM := `{
		"bomFormat": "CycloneDX",
		"specVersion": "1.6",
		"serialNumber": "urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79",
		"version": 1,
		"metadata": {
			"timestamp": "2023-01-01T00:00:00Z"
		}
	}`

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Manager := mockS3.NewMockS3Manager(ctrl)

	st := store.New(store.Config{Bucket: "bucket"}, s3Mock, s3Manager)
	svc, err := service.New(st, service.Config{CheckOnFetch: true})
	require.NoError(t, err)

	storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
	healthSvc := health.NewService(storageChecker)

	cfg := Config{Port: 8080, Prefix: "/api", MaxBodySize: 5}
	server := New(cfg, svc, healthSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/bom", strings.NewReader(validBOM))
	req.Header.Set(HeaderContentType, "application/vnd.cyclonedx+json; version=1.6")
	w := httptest.NewRecorder()

	router := server.Handler()

	router.ServeHTTP(w, req)

	require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)

	var p pd.Problem
	var b []byte
	b, err = io.ReadAll(w.Result().Body)
	require.NoError(t, err)
	err = json.Unmarshal(b, &p)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, p.Status)
	require.Equal(t, "about:blank", p.Type)
	require.Equal(t, http.StatusText(http.StatusRequestEntityTooLarge), p.Title)
	require.Equal(t, "HTTP request body exceeded the maximum allowed size.", p.Detail)
}

func TestGetByURN(t *testing.T) {
	validURN := "urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79"
	invalidURN := "invalid-urn"

	validBOM := map[string]interface{}{
		"bomFormat":    "CycloneDX",
		"specVersion":  "1.6",
		"serialNumber": validURN,
		"version":      1,
	}

	tests := []struct {
		name           string
		urn            string
		version        string
		setupMocks     func(*mockS3.MockS3Contract)
		expectedStatus int
		prefix         string
	}{
		{
			name:           "missing URN parameter",
			urn:            "",
			setupMocks:     func(s3c *mockS3.MockS3Contract) {},
			expectedStatus: http.StatusBadRequest,
			prefix:         "/api",
		},
		{
			name:           "invalid URN format",
			urn:            invalidURN,
			setupMocks:     func(s3c *mockS3.MockS3Contract) {},
			expectedStatus: http.StatusBadRequest,
			prefix:         "/api",
		},
		{
			name:    "BOM not found",
			urn:     validURN,
			version: "1",
			setupMocks: func(s3c *mockS3.MockS3Contract) {
				s3c.EXPECT().GetObject(gomock.Any(), gomock.Any()).Return(nil, &types.NoSuchKey{})
			},
			expectedStatus: http.StatusNotFound,
			prefix:         "/api",
		},
		{
			name:    "successful retrieval with version",
			urn:     validURN,
			version: "1",
			setupMocks: func(s3c *mockS3.MockS3Contract) {
				bomJSON, _ := json.Marshal(validBOM)
				s3c.EXPECT().GetObject(gomock.Any(), gomock.Any()).Return(&s3.GetObjectOutput{
					Body: io.NopCloser(bytes.NewReader(bomJSON)),
				}, nil)
			},
			expectedStatus: http.StatusOK,
			prefix:         "/api",
		},
		{
			name:    "successful retrieval without version - custom prefix",
			urn:     validURN,
			version: "",
			setupMocks: func(s3c *mockS3.MockS3Contract) {
				s3c.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
					Contents: []types.Object{
						{Key: aws.String(validURN + "-1")},
						{Key: aws.String(validURN + "-2")},
					},
				}, nil)
				bomJSON, _ := json.Marshal(validBOM)
				s3c.EXPECT().GetObject(gomock.Any(), gomock.Any()).Return(&s3.GetObjectOutput{
					Body: io.NopCloser(bytes.NewReader(bomJSON)),
				}, nil)
			},
			expectedStatus: http.StatusOK,
			prefix:         "/v2",
		},
		{
			name:    "internal error",
			urn:     validURN,
			version: "1",
			setupMocks: func(s3c *mockS3.MockS3Contract) {
				s3c.EXPECT().GetObject(gomock.Any(), gomock.Any()).Return(nil, errors.New("internal error"))
			},
			expectedStatus: http.StatusInternalServerError,
			prefix:         "/api",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s3Mock := mockS3.NewMockS3Contract(ctrl)
			tt.setupMocks(s3Mock)

			st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
			svc, err := service.New(st, service.Config{CheckOnFetch: true})
			require.NoError(t, err)

			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(storageChecker)

			cfg := Config{Port: 8080, Prefix: tt.prefix}
			server := New(cfg, svc, healthSvc)

			url := fmt.Sprintf("%s/v1/bom/%s", tt.prefix, tt.urn)
			if tt.version != "" {
				url += fmt.Sprintf("?version=%s", tt.version)
			}

			req := httptest.NewRequest(http.MethodGet, url, nil)
			req = mux.SetURLVars(req, map[string]string{"urn": tt.urn})
			w := httptest.NewRecorder()

			server.GetByURN(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)

			if tt.expectedStatus == http.StatusOK {
				require.Equal(t, "application/vnd.cyclonedx+json", w.Header().Get("Content-Type"))
			}
		})
	}
}

func TestSearch(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name           string
		after          string
		setupMocks     func(*mockS3.MockS3Contract)
		expectedStatus int
		prefix         string
	}{
		{
			name:           "missing after parameter",
			after:          "",
			setupMocks:     func(s3c *mockS3.MockS3Contract) {},
			expectedStatus: http.StatusBadRequest,
			prefix:         "/api",
		},
		{
			name:           "invalid after parameter - not a number",
			after:          "invalid",
			setupMocks:     func(s3c *mockS3.MockS3Contract) {},
			expectedStatus: http.StatusBadRequest,
			prefix:         "/api",
		},
		{
			name:           "invalid after parameter - negative number",
			after:          "-1",
			setupMocks:     func(s3c *mockS3.MockS3Contract) {},
			expectedStatus: http.StatusBadRequest,
			prefix:         "/api",
		},
		{
			name:  "successful search",
			after: "1672531200",
			setupMocks: func(s3c *mockS3.MockS3Contract) {
				s3c.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
					Contents: []types.Object{
						{Key: aws.String("urn:uuid:1-1"), LastModified: &now},
						{Key: aws.String("urn:uuid:2-2"), LastModified: &now},
					},
				}, nil)
				s3c.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{
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
				s3c.EXPECT().HeadObject(gomock.Any(), &s3.HeadObjectInput{
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
			},
			expectedStatus: http.StatusOK,
			prefix:         "/api",
		},
		{
			name:  "successful search - empty prefix",
			after: "1672531200",
			setupMocks: func(s3c *mockS3.MockS3Contract) {
				s3c.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
					Contents: []types.Object{},
				}, nil)
			},
			expectedStatus: http.StatusOK,
			prefix:         "",
		},
		{
			name:  "internal error",
			after: "1672531200",
			setupMocks: func(s3c *mockS3.MockS3Contract) {
				s3c.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("internal error"))
			},
			expectedStatus: http.StatusInternalServerError,
			prefix:         "/custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s3Mock := mockS3.NewMockS3Contract(ctrl)
			tt.setupMocks(s3Mock)

			st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
			svc, err := service.New(st, service.Config{CheckOnFetch: true})
			require.NoError(t, err)

			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(storageChecker)

			cfg := Config{Port: 8080, Prefix: tt.prefix}
			server := New(cfg, svc, healthSvc)

			url := fmt.Sprintf("%s/v1/bom", tt.prefix)
			if tt.after != "" {
				url += fmt.Sprintf("?after=%s", tt.after)
			}

			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()

			server.Search(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)

			if tt.expectedStatus == http.StatusOK {
				require.Equal(t, "application/json", w.Header().Get("Content-Type"))
				var response []service.SearchRes
				err := json.NewDecoder(w.Body).Decode(&response)
				require.NoError(t, err)
			}
		})
	}
}

func TestNotFoundHandler(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		method string
		prefix string
	}{
		{
			name:   "not found with default prefix",
			path:   "/api/v1/invalid",
			method: http.MethodGet,
			prefix: "/api",
		},
		{
			name:   "not found with custom prefix",
			path:   "/custom/v1/invalid",
			method: http.MethodPost,
			prefix: "/custom",
		},
		{
			name:   "not found with empty prefix",
			path:   "/v1/invalid",
			method: http.MethodDelete,
			prefix: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(storageChecker)

			cfg := Config{Port: 8080, Prefix: tt.prefix}
			server := New(cfg, service.Service{}, healthSvc)

			router := server.Handler()

			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusNotFound, w.Code)
			bodyStr := w.Body.String()
			require.Contains(t, bodyStr, "There is no handler registered")
		})
	}
}

func TestIntegration_FullRouterWithPrefixes(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{
			name:   "default prefix",
			prefix: "/api",
		},
		{
			name:   "custom prefix v1",
			prefix: "/v1",
		},
		{
			name:   "custom prefix service",
			prefix: "/service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s3Mock := mockS3.NewMockS3Contract(ctrl)
			st := store.New(store.Config{Bucket: "bucket"}, s3Mock, nil)
			svc, err := service.New(st, service.Config{CheckOnFetch: true})
			require.NoError(t, err)

			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(storageChecker)

			cfg := Config{Port: 8080, Prefix: tt.prefix}
			server := New(cfg, svc, healthSvc)

			router := server.Handler()

			// Test health endpoint
			req := httptest.NewRequest(http.MethodGet, tt.prefix+"/v1/health", nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)

			// Test liveness endpoint
			req = httptest.NewRequest(http.MethodGet, tt.prefix+"/v1/health/liveness", nil)
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)

			// Test readiness endpoint
			req = httptest.NewRequest(http.MethodGet, tt.prefix+"/v1/health/readiness", nil)
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)
		})
	}
}

func TestMaxBodySizeMiddleware(t *testing.T) {
	tests := []struct {
		name           string
		maxBytes       int64
		body           string
		expectedStatus int
	}{
		{
			name:           "body within limit",
			maxBytes:       10,
			body:           "12345",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "body exceeds limit",
			maxBytes:       5,
			body:           "123456",
			expectedStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.ReadAll(r.Body)
				if err != nil {
					var maxBytesErr *http.MaxBytesError
					if errors.As(err, &maxBytesErr) {
						http.Error(w, "too large", http.StatusRequestEntityTooLarge)
						return
					}
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
			})

			middleware := maxBodySizeMiddleware(tt.maxBytes)
			handler := middleware(next)

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)
		})
	}
}

func TestSearch_LimitValidation(t *testing.T) {
	tests := []struct {
		name           string
		query          string
		expectList     bool
		expectedStatus int
		expectedDetail string
	}{
		{name: "absent limit is legacy mode", query: "after=1672531200", expectList: true, expectedStatus: http.StatusOK},
		{name: "limit lower bound", query: "after=1672531200&limit=1", expectList: true, expectedStatus: http.StatusOK},
		{name: "limit upper bound", query: "after=1672531200&limit=1000", expectList: true, expectedStatus: http.StatusOK},
		{name: "limit zero", query: "after=1672531200&limit=0", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "limit negative", query: "after=1672531200&limit=-3", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "limit above maximum is rejected, not clamped", query: "after=1672531200&limit=1001", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "limit not an integer", query: "after=1672531200&limit=ten", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "limit present but empty", query: "after=1672531200&limit=", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'limit' must be an integer between 1 and 1000"},
		{name: "after is validated before limit", query: "limit=5", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'after' must not be empty"},
		// `after` is a watermark, not a duration: 0 (the epoch, "everything") is a
		// legitimate value, so the message must not promise a *positive* integer.
		{name: "after zero is accepted", query: "after=0", expectList: true, expectedStatus: http.StatusOK},
		{name: "after not an integer", query: "after=abc", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'after' must be a non-negative integer (unixtime)"},
		{name: "after negative", query: "after=-1", expectedStatus: http.StatusBadRequest, expectedDetail: "query parameter 'after' must be a non-negative integer (unixtime)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s3Mock := mockS3.NewMockS3Contract(ctrl)
			if tt.expectList {
				s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{}, nil)
			}
			svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
			require.NoError(t, err)
			server := New(Config{Prefix: "/api"}, svc, health.NewService(mockChecker{name: "storage", status: health.StatusUp}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/bom?"+tt.query, nil)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedStatus == http.StatusOK {
				require.JSONEq(t, "[]", w.Body.String())
				return
			}
			require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
			var p pd.Problem
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
			require.Contains(t, p.Detail, tt.expectedDetail)
		})
	}
}

// The limit reaches the service: three objects in three seconds, limit=2 → two entries,
// oldest first.
func TestSearch_LimitIsApplied(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

	s3Mock := mockS3.NewMockS3Contract(ctrl)
	s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
		Contents: []types.Object{
			{Key: aws.String("urn:uuid:c-1"), LastModified: aws.Time(base.Add(3 * time.Second))},
			{Key: aws.String("urn:uuid:a-1"), LastModified: aws.Time(base.Add(1 * time.Second))},
			{Key: aws.String("urn:uuid:b-1"), LastModified: aws.Time(base.Add(2 * time.Second))},
		},
	}, nil)
	s3Mock.EXPECT().HeadObject(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			when := map[string]time.Time{"urn:uuid:a-1": base.Add(1 * time.Second), "urn:uuid:b-1": base.Add(2 * time.Second)}[aws.ToString(in.Key)]
			return &s3.HeadObjectOutput{
				ContentLength: aws.Int64(1), ContentType: aws.String("application/json"), LastModified: aws.Time(when),
				Metadata: map[string]string{store.MetaCryptoStatsKey: "{}"},
			}, nil
		}).Times(2)

	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
	require.NoError(t, err)
	server := New(Config{Prefix: "/api"}, svc, health.NewService(mockChecker{name: "storage", status: health.StatusUp}))

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/bom?after=%d&limit=2", base.Unix()), nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var response []service.SearchRes
	require.NoError(t, json.NewDecoder(w.Body).Decode(&response))
	require.Len(t, response, 2)
	require.Equal(t, "urn:uuid:a", response[0].SerialNumber)
	require.Equal(t, "urn:uuid:b", response[1].SerialNumber)
}

// GET /v1/bom/{urn} rejects a `version` query parameter that cannot name a stored
// object: only a positive integer without leading zeros, or "original", ever became an
// object key (see service.ValidVersion). Without the check the value went straight into
// the S3 key and came back as a 404 — indistinguishable from a BOM that once existed and
// was deleted.
//
// An absent value still means "latest version", and so does an empty or all-whitespace
// one: GetBOMByUrn has always treated `strings.TrimSpace(version) == ""` as "latest", so
// rejecting `?version=` would take that away from a caller it has been serving all along.
func TestGetByURN_VersionValidation(t *testing.T) {
	validURN := "urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79"
	tests := []struct {
		name           string
		query          string
		expectGet      bool
		expectLatest   bool
		expectedStatus int
	}{
		{name: "zero is not a version", query: "?version=0", expectedStatus: http.StatusBadRequest},
		{name: "not a number", query: "?version=foo", expectedStatus: http.StatusBadRequest},
		{name: "leading zero", query: "?version=01", expectedStatus: http.StatusBadRequest},
		{name: "negative", query: "?version=-2", expectedStatus: http.StatusBadRequest},
		{name: "present but empty means latest", query: "?version=", expectLatest: true, expectedStatus: http.StatusOK},
		{name: "whitespace means latest", query: "?version=%20", expectLatest: true, expectedStatus: http.StatusOK},
		{name: "numbered version", query: "?version=3", expectGet: true, expectedStatus: http.StatusOK},
		{name: "the original copy", query: "?version=original", expectGet: true, expectedStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s3Mock := mockS3.NewMockS3Contract(ctrl)
			if tt.expectLatest {
				// The latest-version path: list the URN's versions, then fetch the
				// highest numbered one.
				s3Mock.EXPECT().ListObjectsV2(gomock.Any(), gomock.Any(), gomock.Any()).Return(&s3.ListObjectsV2Output{
					Contents: []types.Object{
						{Key: aws.String(validURN + "-1")},
						{Key: aws.String(validURN + "-2")},
					},
				}, nil)
			}
			if tt.expectGet || tt.expectLatest {
				s3Mock.EXPECT().GetObject(gomock.Any(), gomock.Any()).Return(&s3.GetObjectOutput{
					Body: io.NopCloser(strings.NewReader(`{"bomFormat":"CycloneDX"}`)),
				}, nil)
			}
			svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3Mock, nil), service.Config{})
			require.NoError(t, err)
			server := New(Config{Prefix: "/api"}, svc, health.NewService(mockChecker{name: "storage", status: health.StatusUp}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/bom/"+validURN+tt.query, nil)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, req)

			require.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedStatus == http.StatusOK {
				return
			}
			require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
			var p pd.Problem
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
			require.Equal(t, "Request validation failed, query parameter 'version' must be a positive integer or 'original'.", p.Detail)
		})
	}
}
