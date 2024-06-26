// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package safeweb

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/csrf"
)

func TestCompleteCORSConfig(t *testing.T) {
	_, err := NewServer(Config{AccessControlAllowOrigin: []string{"https://foobar.com"}})
	if err == nil {
		t.Fatalf("expected error when AccessControlAllowOrigin is provided without AccessControlAllowMethods")
	}

	_, err = NewServer(Config{AccessControlAllowMethods: []string{"GET", "POST"}})
	if err == nil {
		t.Fatalf("expected error when AccessControlAllowMethods is provided without AccessControlAllowOrigin")
	}

	_, err = NewServer(Config{AccessControlAllowOrigin: []string{"https://foobar.com"}, AccessControlAllowMethods: []string{"GET", "POST"}})
	if err != nil {
		t.Fatalf("error creating server with complete CORS configuration: %v", err)
	}
}

func TestPostRequestContentTypeValidation(t *testing.T) {
	tests := []struct {
		name         string
		browserRoute bool
		contentType  string
		wantErr      bool
	}{
		{
			name:         "API routes should accept `application/json` content-type",
			browserRoute: false,
			contentType:  "application/json",
			wantErr:      false,
		},
		{
			name:         "API routes should reject `application/x-www-form-urlencoded` content-type",
			browserRoute: false,
			contentType:  "application/x-www-form-urlencoded",
			wantErr:      true,
		},
		{
			name:         "Browser routes should accept `application/x-www-form-urlencoded` content-type",
			browserRoute: true,
			contentType:  "application/x-www-form-urlencoded",
			wantErr:      false,
		},
		{
			name:         "non Browser routes should accept `application/json` content-type",
			browserRoute: true,
			contentType:  "application/json",
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &http.ServeMux{}
			h.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("ok"))
			}))
			var s *Server
			var err error
			if tt.browserRoute {
				s, err = NewServer(Config{BrowserMux: h})
			} else {
				s, err = NewServer(Config{APIMux: h})
			}
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("POST", "/", nil)
			req.Header.Set("Content-Type", tt.contentType)

			w := httptest.NewRecorder()
			s.h.Handler.ServeHTTP(w, req)
			resp := w.Result()
			if tt.wantErr && resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("content type validation failed: got %v; want %v", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestAPIMuxCrossOriginResourceSharingHeaders(t *testing.T) {
	tests := []struct {
		name            string
		httpMethod      string
		wantCORSHeaders bool
		corsOrigins     []string
		corsMethods     []string
	}{
		{
			name:            "do not set CORS headers for non-OPTIONS requests",
			corsOrigins:     []string{"https://foobar.com"},
			corsMethods:     []string{"GET", "POST", "HEAD"},
			httpMethod:      "GET",
			wantCORSHeaders: false,
		},
		{
			name:            "set CORS headers for non-OPTIONS requests",
			corsOrigins:     []string{"https://foobar.com"},
			corsMethods:     []string{"GET", "POST", "HEAD"},
			httpMethod:      "OPTIONS",
			wantCORSHeaders: true,
		},
		{
			name:            "do not serve CORS headers for OPTIONS requests with no configured origins",
			httpMethod:      "OPTIONS",
			wantCORSHeaders: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &http.ServeMux{}
			h.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("ok"))
			}))
			s, err := NewServer(Config{
				APIMux:                    h,
				AccessControlAllowOrigin:  tt.corsOrigins,
				AccessControlAllowMethods: tt.corsMethods,
			})
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(tt.httpMethod, "/", nil)
			w := httptest.NewRecorder()
			s.h.Handler.ServeHTTP(w, req)
			resp := w.Result()

			if (resp.Header.Get("Access-Control-Allow-Origin") == "") == tt.wantCORSHeaders {
				t.Fatalf("access-control-allow-origin want: %v; got: %v", tt.wantCORSHeaders, resp.Header.Get("Access-Control-Allow-Origin"))
			}
		})
	}
}

func TestCSRFProtection(t *testing.T) {
	tests := []struct {
		name          string
		apiRoute      bool
		passCSRFToken bool
		wantStatus    int
	}{
		{
			name:          "POST requests to non-API routes require CSRF token and fail if not provided",
			apiRoute:      false,
			passCSRFToken: false,
			wantStatus:    http.StatusForbidden,
		},
		{
			name:          "POST requests to non-API routes require CSRF token and pass if provided",
			apiRoute:      false,
			passCSRFToken: true,
			wantStatus:    http.StatusOK,
		},
		{
			name:          "POST requests to /api/ routes do not require CSRF token",
			apiRoute:      true,
			passCSRFToken: false,
			wantStatus:    http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &http.ServeMux{}
			h.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("ok"))
			}))
			var s *Server
			var err error
			if tt.apiRoute {
				s, err = NewServer(Config{APIMux: h})
			} else {
				s, err = NewServer(Config{BrowserMux: h})
			}
			if err != nil {
				t.Fatal(err)
			}

			// construct the test request
			req := httptest.NewRequest("POST", "/", nil)

			// send JSON for API routes, form data for browser routes
			if tt.apiRoute {
				req.Header.Set("Content-Type", "application/json")
			} else {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}

			// retrieve CSRF cookie & pass it in the test request
			// ref: https://github.com/gorilla/csrf/blob/main/csrf_test.go#L344-L347
			var token string
			if tt.passCSRFToken {
				h.Handle("/csrf", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					token = csrf.Token(r)
				}))
				get := httptest.NewRequest("GET", "/csrf", nil)
				w := httptest.NewRecorder()
				s.h.Handler.ServeHTTP(w, get)
				resp := w.Result()

				// pass the token & cookie in our subsequent test request
				req.Header.Set("X-CSRF-Token", token)
				for _, c := range resp.Cookies() {
					req.AddCookie(c)
				}
			}

			w := httptest.NewRecorder()
			s.h.Handler.ServeHTTP(w, req)
			resp := w.Result()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("csrf protection check failed: got %v; want %v", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestContentSecurityPolicyHeader(t *testing.T) {
	tests := []struct {
		name     string
		apiRoute bool
		wantCSP  bool
	}{
		{
			name:     "default routes get CSP headers",
			apiRoute: false,
			wantCSP:  true,
		},
		{
			name:     "`/api/*` routes do not get CSP headers",
			apiRoute: true,
			wantCSP:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &http.ServeMux{}
			h.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("ok"))
			}))
			var s *Server
			var err error
			if tt.apiRoute {
				s, err = NewServer(Config{APIMux: h})
			} else {
				s, err = NewServer(Config{BrowserMux: h})
			}
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			s.h.Handler.ServeHTTP(w, req)
			resp := w.Result()

			if (resp.Header.Get("Content-Security-Policy") == "") == tt.wantCSP {
				t.Fatalf("content security policy want: %v; got: %v", tt.wantCSP, resp.Header.Get("Content-Security-Policy"))
			}
		})
	}
}

func TestCSRFCookieSecureMode(t *testing.T) {
	tests := []struct {
		name       string
		secureMode bool
		wantSecure bool
	}{
		{
			name:       "CSRF cookie should be secure when server is in secure context",
			secureMode: true,
			wantSecure: true,
		},
		{
			name:       "CSRF cookie should not be secure when server is not in secure context",
			secureMode: false,
			wantSecure: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &http.ServeMux{}
			h.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("ok"))
			}))
			s, err := NewServer(Config{BrowserMux: h, SecureContext: tt.secureMode})
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			s.h.Handler.ServeHTTP(w, req)
			resp := w.Result()

			cookie := resp.Cookies()[0]
			if (cookie.Secure == tt.wantSecure) == false {
				t.Fatalf("csrf cookie secure flag want: %v; got: %v", tt.wantSecure, cookie.Secure)
			}
		})
	}
}

func TestRefererPolicy(t *testing.T) {
	tests := []struct {
		name              string
		browserRoute      bool
		wantRefererPolicy bool
	}{
		{
			name:              "BrowserMux routes get Referer-Policy headers",
			browserRoute:      true,
			wantRefererPolicy: true,
		},
		{
			name:              "APIMux routes do not get Referer-Policy headers",
			browserRoute:      false,
			wantRefererPolicy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &http.ServeMux{}
			h.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("ok"))
			}))
			var s *Server
			var err error
			if tt.browserRoute {
				s, err = NewServer(Config{BrowserMux: h})
			} else {
				s, err = NewServer(Config{APIMux: h})
			}
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			s.h.Handler.ServeHTTP(w, req)
			resp := w.Result()

			if (resp.Header.Get("Referer-Policy") == "") == tt.wantRefererPolicy {
				t.Fatalf("referer policy want: %v; got: %v", tt.wantRefererPolicy, resp.Header.Get("Referer-Policy"))
			}
		})
	}
}

func TestCSPAllowInlineStyles(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			h := &http.ServeMux{}
			h.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("ok"))
			}))
			s, err := NewServer(Config{BrowserMux: h, CSPAllowInlineStyles: allow})
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("GET", "/", nil)
			w := httptest.NewRecorder()
			s.h.Handler.ServeHTTP(w, req)
			resp := w.Result()

			csp := resp.Header.Get("Content-Security-Policy")
			allowsStyles := strings.Contains(csp, "style-src 'self' 'unsafe-inline'")
			if allowsStyles != allow {
				t.Fatalf("CSP inline styles want: %v; got: %v", allow, allowsStyles)
			}
		})
	}
}
