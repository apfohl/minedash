package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestPublicPageSessionLinks(t *testing.T) {
	cfg := Config{JWTSecret: "test-secret"}
	tmpl, err := template.ParseFS(templates, "*.gohtml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		expiry time.Time
		secret string
		link   string
	}{
		{name: "anonymous", link: `/login`},
		{name: "authenticated", expiry: time.Now().Add(time.Hour), secret: cfg.JWTSecret, link: `/dashboard`},
		{name: "expired", expiry: time.Now().Add(-time.Hour), secret: cfg.JWTSecret, link: `/login`},
		{name: "invalid signature", expiry: time.Now().Add(time.Hour), secret: "wrong-secret", link: `/login`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.secret != "" {
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(tc.expiry)})
				signed, err := token.SignedString([]byte(tc.secret))
				if err != nil {
					t.Fatal(err)
				}
				r.AddCookie(&http.Cookie{Name: cookieName, Value: signed})
			}
			w := httptest.NewRecorder()
			pageHandler(tmpl, "status.gohtml", cfg).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), `href="`+tc.link+`"`) {
				t.Fatalf("missing %s link", tc.link)
			}
			if strings.Contains(w.Body.String(), "World Management") || strings.Contains(w.Body.String(), "/api/start") {
				t.Fatal("public page contains management controls")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("session-dependent page must not be cached")
			}
			called := false
			protected := jwtMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
			protected.ServeHTTP(httptest.NewRecorder(), r)
			if called != (tc.link == "/dashboard") {
				t.Fatal("protected route session check differs from public page")
			}
		})
	}
}
