package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	oidcCallbackPath = "/oidc/callback"
	oidcFlowCookie   = "__Host-minedash_oidc"
	oidcFlowTTL      = 5 * time.Minute
)

type oidcConfig struct {
	Issuer, ClientID, ClientSecret, CallbackURL, LogoutRedirectURL string
}

func (cfg oidcConfig) enabled() bool {
	return cfg.ClientID != ""
}

func (cfg oidcConfig) validate() error {
	if cfg.ClientID == "" && cfg.ClientSecret == "" && cfg.CallbackURL == "" && cfg.LogoutRedirectURL == "" {
		return nil
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.CallbackURL == "" {
		return fmt.Errorf("OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and OIDC_CALLBACK_URL must be set together")
	}
	issuer := httpsURL(cfg.Issuer)
	if issuer == nil || issuer.RawQuery != "" {
		return fmt.Errorf("OIDC_ISSUER must be an HTTPS issuer URL")
	}
	callback := httpsURL(cfg.CallbackURL)
	if callback == nil || callback.EscapedPath() != oidcCallbackPath || callback.RawQuery != "" {
		return fmt.Errorf("OIDC_CALLBACK_URL must be an HTTPS URL ending in %s", oidcCallbackPath)
	}
	if cfg.LogoutRedirectURL != "" && httpsURL(cfg.LogoutRedirectURL) == nil {
		return fmt.Errorf("OIDC_LOGOUT_REDIRECT_URL must be an HTTPS URL")
	}
	return nil
}

func httpsURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.ForceQuery {
		return nil
	}
	return u
}

type oidcFlow struct {
	nonce, verifier string
	expires         time.Time
}

type oidcLogin struct {
	cfg   Config
	mu    sync.Mutex
	flows map[string]oidcFlow
}

type oidcDiscovery struct {
	JWKS               string `json:"jwks_uri"`
	EndSessionEndpoint string `json:"end_session_endpoint"`
}

func (a *oidcLogin) discover(ctx context.Context) (*oidc.Provider, oidcDiscovery, error) {
	var discovery oidcDiscovery
	provider, err := oidc.NewProvider(ctx, a.cfg.OIDC.Issuer)
	if err != nil {
		return nil, discovery, err
	}
	if err := provider.Claims(&discovery); err != nil {
		return nil, discovery, err
	}
	endpoint := provider.Endpoint()
	for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL, provider.UserInfoEndpoint(), discovery.JWKS} {
		if httpsURL(raw) == nil {
			return nil, discovery, fmt.Errorf("OIDC discovery requires HTTPS endpoints")
		}
	}
	return provider, discovery, nil
}

func (a *oidcLogin) oauth(provider *oidc.Provider) oauth2.Config {
	endpoint := provider.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	return oauth2.Config{
		ClientID:     a.cfg.OIDC.ClientID,
		ClientSecret: a.cfg.OIDC.ClientSecret,
		RedirectURL:  a.cfg.OIDC.CallbackURL,
		Endpoint:     endpoint,
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
	}
}

func oidcCookie(w http.ResponseWriter, state string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: oidcFlowCookie, Value: state, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (a *oidcLogin) start(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	provider, _, err := a.discover(ctx)
	if err != nil {
		http.Error(w, "OIDC provider unavailable", http.StatusBadGateway)
		return
	}
	var random [64]byte
	rand.Read(random[:])
	state := base64.RawURLEncoding.EncodeToString(random[:32])
	now := time.Now()
	flow := oidcFlow{
		nonce:    base64.RawURLEncoding.EncodeToString(random[32:]),
		verifier: oauth2.GenerateVerifier(),
		expires:  now.Add(oidcFlowTTL),
	}

	a.mu.Lock()
	for key, pending := range a.flows {
		if !now.Before(pending.expires) {
			delete(a.flows, key)
		}
	}
	if previous, err := r.Cookie(oidcFlowCookie); err == nil {
		delete(a.flows, previous.Value)
	}
	if len(a.flows) >= 1024 {
		a.mu.Unlock()
		http.Error(w, "Too many pending OIDC logins", http.StatusServiceUnavailable)
		return
	}
	a.flows[state] = flow
	a.mu.Unlock()

	oidcCookie(w, state, int(oidcFlowTTL.Seconds()))
	oauth := a.oauth(provider)
	http.Redirect(w, r, oauth.AuthCodeURL(state, oidc.Nonce(flow.nonce), oauth2.S256ChallengeOption(flow.verifier)), http.StatusSeeOther)
}

func (a *oidcLogin) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	query := r.URL.Query()
	state := query.Get("state")
	cookie, err := r.Cookie(oidcFlowCookie)
	if err != nil || state == "" || len(query["state"]) != 1 || cookie.Value != state {
		http.Error(w, "Invalid OIDC login state", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	flow, found := a.flows[state]
	delete(a.flows, state)
	a.mu.Unlock()
	oidcCookie(w, "", -1)
	if !found || !time.Now().Before(flow.expires) {
		http.Error(w, "Expired or used OIDC login state", http.StatusBadRequest)
		return
	}

	code := query.Get("code")
	if query.Has("error") || code == "" || len(query["code"]) != 1 {
		http.Error(w, "OIDC authorization failed", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	provider, discovery, err := a.discover(ctx)
	if err != nil {
		http.Error(w, "OIDC provider unavailable", http.StatusBadGateway)
		return
	}
	oauth := a.oauth(provider)
	token, err := oauth.Exchange(ctx, code, oauth2.VerifierOption(flow.verifier))
	if err != nil {
		http.Error(w, "OIDC code exchange failed", http.StatusBadGateway)
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	id, err := provider.Verifier(&oidc.Config{
		ClientID: a.cfg.OIDC.ClientID, SupportedSigningAlgs: []string{oidc.RS256},
	}).Verify(ctx, rawIDToken)
	if err != nil || id.Nonce != flow.nonce || id.Subject == "" {
		http.Error(w, "Invalid OIDC ID token", http.StatusUnauthorized)
		return
	}

	user, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil || user.Subject != id.Subject {
		http.Error(w, "OIDC UserInfo verification failed", http.StatusBadGateway)
		return
	}
	var profile struct {
		URL string `json:"profile"`
	}
	_ = user.Claims(&profile)
	profileURL, issuerURL := httpsURL(profile.URL), httpsURL(id.Issuer)
	if profileURL == nil || issuerURL == nil || profileURL.Host != issuerURL.Host {
		profile.URL = ""
	}

	claims := sessionClaims{ProfileURL: profile.URL}
	claims.Issuer, claims.Subject = id.Issuer, id.Subject
	claims.ProviderLogout = httpsURL(discovery.EndSessionEndpoint) != nil
	if err := setSession(w, a.cfg, claims, true); err != nil {
		http.Error(w, "Failed to create session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (a *oidcLogin) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	claims := readSession(a.cfg, r)
	if claims == nil || claims.Issuer != a.cfg.OIDC.Issuer || !claims.ProviderLogout {
		http.Error(w, "OIDC session required", http.StatusUnauthorized)
		return
	}
	clearSession(w)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_, discovery, err := a.discover(ctx)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	target := httpsURL(discovery.EndSessionEndpoint)
	if target == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if redirect := a.cfg.OIDC.LogoutRedirectURL; redirect != "" {
		query := target.Query()
		query.Set("client_id", a.cfg.OIDC.ClientID)
		query.Set("post_logout_redirect_uri", redirect)
		target.RawQuery = query.Encode()
	}
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}
