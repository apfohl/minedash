package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "minedash_session"
	jwtTTL     = 24 * time.Hour
)

// loginRequest is the expected JSON body for POST /login.
type loginRequest struct {
	Password string `json:"password"`
}

// loginHandler verifies the password against the bcrypt hash and issues a JWT cookie.
func loginHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(cfg.AuthPasswordHash), []byte(req.Password)); err != nil {
			errorJSON(w, http.StatusUnauthorized, "invalid password")
			return
		}

		if err := setSession(w, cfg, sessionClaims{}, false); err != nil {
			errorJSON(w, http.StatusInternalServerError, "failed to create session")
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// logoutHandler clears the session cookie.
func logoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clearSession(w)
		writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
	}
}

// jwtMiddleware validates the JWT cookie before passing to the next handler.
// If the cookie is missing or invalid it returns 401.
func jwtMiddleware(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasSession(cfg, r) {
			errorJSON(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}

		next.ServeHTTP(w, r)
	})
}

type sessionClaims struct {
	jwt.RegisteredClaims
	ProfileURL     string `json:"profile,omitempty"`
	ProviderLogout bool   `json:"provider_logout,omitempty"`
}

func setSession(w http.ResponseWriter, cfg Config, claims sessionClaims, secure bool) error {
	now := time.Now()
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(jwtTTL))
	claims.IssuedAt = jwt.NewNumericDate(now)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(cfg.JWTSecret))
	if err != nil {
		return err
	}
	sameSite := http.SameSiteStrictMode
	if secure {
		// The OIDC callback is reached through a cross-site navigation.
		sameSite = http.SameSiteLaxMode
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: signed, Path: "/", MaxAge: int(jwtTTL.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: sameSite,
	})
	return nil
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// hasSession checks the signature and expiry rather than just cookie presence.
func hasSession(cfg Config, r *http.Request) bool {
	return readSession(cfg, r) != nil
}

func readSession(cfg Config, r *http.Request) *sessionClaims {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	claims := &sessionClaims{}
	token, err := jwt.ParseWithClaims(cookie.Value, claims, func(t *jwt.Token) (any, error) {
		return []byte(cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return nil
	}
	return claims
}
