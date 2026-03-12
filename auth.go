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

		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		})

		signed, err := token.SignedString([]byte(cfg.JWTSecret))
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "failed to create session")
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    signed,
			Path:     "/",
			MaxAge:   int(jwtTTL.Seconds()),
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})

		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// logoutHandler clears the session cookie.
func logoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
	}
}

// jwtMiddleware validates the JWT cookie before passing to the next handler.
// If the cookie is missing or invalid it returns 401.
func jwtMiddleware(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			errorJSON(w, http.StatusUnauthorized, "not authenticated")
			return
		}

		token, err := jwt.ParseWithClaims(cookie.Value, &jwt.RegisteredClaims{}, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(cfg.JWTSecret), nil
		})
		if err != nil || !token.Valid {
			errorJSON(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}

		next.ServeHTTP(w, r)
	})
}
