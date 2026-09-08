package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OmniTrustILM/cbom-repository/internal/health"
	"github.com/OmniTrustILM/cbom-repository/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name           string
		cfg            Config
		expectedPrefix string
	}{
		{
			name: "default prefix",
			cfg: Config{
				Port:   8080,
				Prefix: "/api",
			},
			expectedPrefix: "/api",
		},
		{
			name: "prefix with trailing slash",
			cfg: Config{
				Port:   8080,
				Prefix: "/v1/",
			},
			expectedPrefix: "/v1",
		},
		{
			name: "prefix without leading slash",
			cfg: Config{
				Port:   8080,
				Prefix: "api",
			},
			expectedPrefix: "/api",
		},
		{
			name: "empty prefix",
			cfg: Config{
				Port:   8080,
				Prefix: "",
			},
			expectedPrefix: "",
		},
		{
			name: "custom prefix",
			cfg: Config{
				Port:   9090,
				Prefix: "/custom/path",
			},
			expectedPrefix: "/custom/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(storageChecker)

			server := New(tt.cfg, service.Service{}, healthSvc)

			require.Equal(t, tt.expectedPrefix, server.cfg.Prefix)
			require.Equal(t, tt.cfg.Port, server.cfg.Port)
		})
	}
}

func TestHandler(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{
			name:   "default prefix",
			prefix: "/api",
		},
		{
			name:   "custom prefix",
			prefix: "/v2",
		},
		{
			name:   "empty prefix",
			prefix: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Port:   8080,
				Prefix: tt.prefix,
			}

			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			healthSvc := health.NewService(storageChecker)

			server := New(cfg, service.Service{}, healthSvc)
			router := server.Handler()

			require.NotNil(t, router)
			require.NotNil(t, router.NotFoundHandler)
		})
	}
}

// mockChecker is a mock implementation of the health.Checker interface used by health.NewService
type mockChecker struct {
	name    string
	status  health.Status
	details map[string]any
}

func (m mockChecker) Name() string { return m.name }
func (m mockChecker) Check(ctx context.Context) health.Component {
	return health.Component{Status: m.status, Details: m.details}
}

func TestCORSHeadersOnSimpleRequest(t *testing.T) {
	tests := []struct {
		name            string
		allowedOrigins  []string
		requestOrigin   string
		wantAllowOrigin string
		wantVary        string
	}{
		{
			name:            "cors disabled emits no headers",
			allowedOrigins:  nil,
			requestOrigin:   "http://localhost:8000",
			wantAllowOrigin: "",
			wantVary:        "",
		},
		{
			// `APP_HTTP_CORS_ALLOWED_ORIGINS=","` parses into entries that are
			// all empty, which must leave CORS off rather than install a
			// middleware that can never match an origin.
			name:            "entries that are all blank keep cors disabled",
			allowedOrigins:  []string{"", " "},
			requestOrigin:   "http://localhost:8000",
			wantAllowOrigin: "",
			wantVary:        "",
		},
		{
			name:            "allowed origin is echoed",
			allowedOrigins:  []string{"http://localhost:8000"},
			requestOrigin:   "http://localhost:8000",
			wantAllowOrigin: "http://localhost:8000",
			wantVary:        "Origin",
		},
		{
			name:            "unlisted origin gets vary but no allow header",
			allowedOrigins:  []string{"http://localhost:8000"},
			requestOrigin:   "http://attacker.example",
			wantAllowOrigin: "",
			wantVary:        "Origin",
		},
		{
			name:            "wildcard echoes the request origin",
			allowedOrigins:  []string{"*"},
			requestOrigin:   "http://localhost:8000",
			wantAllowOrigin: "http://localhost:8000",
			wantVary:        "Origin",
		},
		{
			name:            "origin matching is case-insensitive",
			allowedOrigins:  []string{"http://LocalHost:8000"},
			requestOrigin:   "http://localhost:8000",
			wantAllowOrigin: "http://localhost:8000",
			wantVary:        "Origin",
		},
		{
			name:            "allowed origin entry with surrounding whitespace still matches",
			allowedOrigins:  []string{" http://localhost:8000 "},
			requestOrigin:   "http://localhost:8000",
			wantAllowOrigin: "http://localhost:8000",
			wantVary:        "Origin",
		},
		{
			name:            "same-origin request without an origin header is untouched",
			allowedOrigins:  []string{"http://localhost:8000"},
			requestOrigin:   "",
			wantAllowOrigin: "",
			wantVary:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Port:               8080,
				Prefix:             "/api",
				MaxBodySize:        20971520,
				CORSAllowedOrigins: tt.allowedOrigins,
			}
			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			server := New(cfg, service.Service{}, health.NewService(storageChecker))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			if tt.requestOrigin != "" {
				req.Header.Set("Origin", tt.requestOrigin)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, tt.wantAllowOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
			require.Equal(t, tt.wantVary, rec.Header().Get("Vary"))
			// A browser only lets the page read the Link header (the paged search's
			// next-page cursor) when it is listed; without it a browser client would
			// see every page as the last one.
			wantExpose := ""
			if tt.wantAllowOrigin != "" {
				wantExpose = "Link"
			}
			require.Equal(t, wantExpose, rec.Header().Get("Access-Control-Expose-Headers"))
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	tests := []struct {
		name            string
		allowedOrigins  []string
		path            string
		wantStatus      int
		wantAllowOrigin string
		wantMethods     string
	}{
		{
			name:            "allowed origin preflight is answered",
			allowedOrigins:  []string{"http://localhost:8000"},
			path:            "/api/v1/health",
			wantStatus:      http.StatusNoContent,
			wantAllowOrigin: "http://localhost:8000",
			wantMethods:     "GET, POST, OPTIONS",
		},
		{
			name:            "upload endpoint preflight is answered",
			allowedOrigins:  []string{"http://localhost:8000"},
			path:            "/api/v1/bom",
			wantStatus:      http.StatusNoContent,
			wantAllowOrigin: "http://localhost:8000",
			wantMethods:     "GET, POST, OPTIONS",
		},
		{
			name:            "unlisted origin preflight carries no allow header",
			allowedOrigins:  []string{"http://localhost:8000"},
			path:            "/api/v1/health",
			wantStatus:      http.StatusNoContent,
			wantAllowOrigin: "",
			wantMethods:     "",
		},
		{
			name:            "cors disabled keeps the current 405 behavior",
			allowedOrigins:  nil,
			path:            "/api/v1/health",
			wantStatus:      http.StatusMethodNotAllowed,
			wantAllowOrigin: "",
			wantMethods:     "",
		},
		{
			// Blank entries carry no origin to allow, so the preflight route
			// must stay unregistered and the 405 behavior must survive.
			name:            "entries that are all blank keep the 405 behavior",
			allowedOrigins:  []string{"", " "},
			path:            "/api/v1/health",
			wantStatus:      http.StatusMethodNotAllowed,
			wantAllowOrigin: "",
			wantMethods:     "",
		},
		{
			// The catch-all preflight route matches any path, registered or
			// not, so an OPTIONS to a nonexistent path also gets a 204 with
			// CORS headers instead of the problem+json 404 the
			// NotFoundHandler would otherwise produce.
			name:            "preflight on an unregistered path is still answered",
			allowedOrigins:  []string{"http://localhost:8000"},
			path:            "/api/v1/not-a-real-path",
			wantStatus:      http.StatusNoContent,
			wantAllowOrigin: "http://localhost:8000",
			wantMethods:     "GET, POST, OPTIONS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestOrigin := "http://localhost:8000"
			if tt.wantAllowOrigin == "" && len(tt.allowedOrigins) != 0 {
				requestOrigin = "http://attacker.example"
			}

			cfg := Config{
				Port:               8080,
				Prefix:             "/api",
				MaxBodySize:        20971520,
				CORSAllowedOrigins: tt.allowedOrigins,
			}
			storageChecker := mockChecker{name: "storage", status: health.StatusUp, details: map[string]any{"latencyMs": 1}}
			server := New(cfg, service.Service{}, health.NewService(storageChecker))

			req := httptest.NewRequest(http.MethodOptions, tt.path, nil)
			req.Header.Set("Origin", requestOrigin)
			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			require.Equal(t, tt.wantStatus, rec.Code)
			require.Equal(t, tt.wantAllowOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
			require.Equal(t, tt.wantMethods, rec.Header().Get("Access-Control-Allow-Methods"))
		})
	}
}
