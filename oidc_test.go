package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

type oidcTestTransport func(*http.Request) (*http.Response, error)

func (f oidcTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOIDCLogin(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const callback = "https://dashboard.example/oidc/callback"
	const issuer = "https://provider.example"
	var nonce, challenge string
	exchanges := 0
	provider := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(w, http.StatusOK, map[string]string{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
				"userinfo_endpoint": issuer + "/userinfo", "end_session_endpoint": issuer + "/logout",
			})
		case "/token":
			exchanges++
			id, secret, basic := r.BasicAuth()
			if !basic || id != "dashboard" || secret != "secret" ||
				r.FormValue("redirect_uri") != callback || r.FormValue("code") != "test-code" ||
				oauth2.S256ChallengeFromVerifier(r.FormValue("code_verifier")) != challenge {
				t.Error("unexpected token exchange")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
				"iss": issuer, "sub": "test-user", "aud": "dashboard", "nonce": nonce,
				"exp": time.Now().Add(time.Hour).Unix(),
			})
			signed, err := token.SignedString(key)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"id_token": signed, "access_token": "access", "token_type": "Bearer",
			})
		case "/keys":
			writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
			}}})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer access" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"sub": "test-user", "profile": issuer + "/account"})
		default:
			http.NotFound(w, r)
		}
	})
	client := &http.Client{Transport: oidcTestTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		provider.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	cfg := Config{JWTSecret: "test-key", OIDC: oidcConfig{
		Issuer: issuer, ClientID: "dashboard", ClientSecret: "secret",
		CallbackURL: callback, LogoutRedirectURL: "https://dashboard.example/",
	}}
	login := &oidcLogin{cfg: cfg, flows: make(map[string]oidcFlow)}
	request := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		return r.WithContext(oidc.ClientContext(r.Context(), client))
	}

	start := httptest.NewRecorder()
	login.start(start, request(http.MethodGet, "/oidc/login"))
	location, err := start.Result().Location()
	if err != nil || start.Code != http.StatusSeeOther {
		t.Fatalf("login start: %d %v", start.Code, err)
	}
	query := location.Query()
	state := query.Get("state")
	nonce, challenge = query.Get("nonce"), query.Get("code_challenge")
	if state == "" || nonce == "" || challenge == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatal("missing state, nonce or PKCE S256")
	}
	flowCookie := start.Result().Cookies()[0]
	if flowCookie.Value != state || !flowCookie.Secure || !flowCookie.HttpOnly || flowCookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("invalid flow cookie")
	}

	finish := func(state string) *httptest.ResponseRecorder {
		r := request(http.MethodGet, callback+"?code=test-code&state="+url.QueryEscape(state))
		r.AddCookie(flowCookie)
		w := httptest.NewRecorder()
		login.callback(w, r)
		return w
	}
	if w := finish("wrong"); w.Code != http.StatusBadRequest {
		t.Fatalf("incorrect state returned %d", w.Code)
	}
	w := finish(state)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard" {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	replay := finish(state)
	if replay.Code != http.StatusBadRequest || exchanges != 1 {
		t.Fatalf("replayed callback returned %d after %d exchanges", replay.Code, exchanges)
	}
	session := w.Result().Cookies()[1]
	r := request(http.MethodPost, "/oidc/logout")
	r.AddCookie(session)
	claims := readSession(cfg, r)
	if !session.Secure || claims == nil || claims.Issuer != issuer || claims.Subject != "test-user" || claims.ProfileURL != issuer+"/account" {
		t.Fatalf("invalid OIDC session")
	}
	localLogout := httptest.NewRecorder()
	logoutHandler().ServeHTTP(localLogout, r)
	if localLogout.Code != http.StatusOK || localLogout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("local logout did not clear the OIDC session cookie")
	}

	logout := httptest.NewRecorder()
	login.logout(logout, r)
	target, err := logout.Result().Location()
	if err != nil || target.Path != "/logout" || target.Query().Get("client_id") != "dashboard" ||
		target.Query().Get("post_logout_redirect_uri") != cfg.OIDC.LogoutRedirectURL ||
		logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("provider logout: %d %v", logout.Code, err)
	}
}
