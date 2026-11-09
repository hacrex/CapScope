// Package oidc implements OpenID Connect Discovery, PKCE authorization,
// token exchange and UserInfo for CapScope's AUTH_PROVIDER=oidc mode.
//
// It is deliberately dependency-free: no golang.org/x/oauth2, just the
// standard library. The issuer's Discovery document provides the
// authorization and token endpoints; PKCE (RFC 7636) protects the
// authorization code flow; the access token is used to fetch the UserInfo
// endpoint, whose "sub" claim becomes the CapScope username.
//
// Provider allowlist: the OIDC provider's "sub" claim is matched against
// AUTH_USER. If AUTH_USER is set and the sub does not match, login is
// rejected — this is the users.yml-equivalent allowlist from the roadmap.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// hashSha256 returns the SHA-256 hash of s.
func hashSha256(s string) []byte {
	h := sha256.New()
	h.Write([]byte(s))
	return h.Sum(nil)
}

// Provider is an OpenID Connect provider discovered via its issuer.
type Provider struct {
	Issuer           string
	AuthEndpoint     string
	TokenEndpoint    string
	UserInfoEndpoint string
	ClientID         string
	ClientSecret     string
	AllowName        string // AUTH_USER: the only "sub" allowed to log in
	log              *slog.Logger
}

// Discovery is the OIDC Discovery document.
type Discovery struct {
	Issuer            string   `json:"issuer"`
	AuthEndpoint      string   `json:"authorization_endpoint"`
	TokenEndpoint     string   `json:"token_endpoint"`
	UserInfoEndpoint  string   `json:"userinfo_endpoint"`
	JWKSURL           string   `json:"jwks_uri"`
	ResponseTypes     []string `json:"response_types_supported"`
	SubjectTypes      []string `json:"subject_types_supported"`
	IDTokenSigningAlg []string `json:"id_token_signing_alg_values_supported"`
}

// UserInfo is the OIDC UserInfo response.
type UserInfo struct {
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Tokens is the token endpoint response.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// NewProvider discovers an OIDC provider from its issuer URL.
func NewProvider(issuer, clientID, clientSecret, allowName string, log *slog.Logger) (*Provider, error) {
	if !strings.HasSuffix(issuer, "/") {
		issuer += "/"
	}
	discoveryURL := issuer + ".well-known/openid-configuration"
	req, err := http.NewRequest("GET", discoveryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("oidc discovery: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var d Discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("oidc discovery: invalid JSON: %w", err)
	}
	if d.AuthEndpoint == "" || d.TokenEndpoint == "" || d.UserInfoEndpoint == "" {
		return nil, errors.New("oidc discovery: missing authorization/token/userinfo endpoints")
	}
	return &Provider{
		Issuer:           issuer,
		AuthEndpoint:     d.AuthEndpoint,
		TokenEndpoint:    d.TokenEndpoint,
		UserInfoEndpoint: d.UserInfoEndpoint,
		ClientID:         clientID,
		ClientSecret:     clientSecret,
		AllowName:        allowName,
		log:              log,
	}, nil
}

// state stores PKCE code_verifier + nonce between authorization and callback.
type state struct {
	CodeVerifier string
	Nonce        string
	RedirectTo   string // where to send the user after login
}

// randString returns a URL-safe base64 string of n random bytes.
func randString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AuthorizationURL builds the authorization URL with PKCE. The state parameter
// carries the code_verifier and nonce (opaque to the provider).
func (p *Provider) AuthorizationURL(redirectURI, redirectTo string) (string, string, error) {
	codeVerifier, err := randString(32)
	if err != nil {
		return "", "", err
	}
	nonce, err := randString(16)
	if err != nil {
		return "", "", err
	}
	codeChallenge := base64.RawURLEncoding.EncodeToString(hashSha256(codeVerifier))

	st := state{CodeVerifier: codeVerifier, Nonce: nonce, RedirectTo: redirectTo}
	stJSON, err := json.Marshal(st)
	if err != nil {
		return "", "", err
	}
	stateParam := base64.RawURLEncoding.EncodeToString(stJSON)

	u, err := url.Parse(p.AuthEndpoint)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid email profile")
	q.Set("state", stateParam)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("nonce", nonce)
	u.RawQuery = q.Encode()
	return u.String(), stateParam, nil
}

// Callback validates the authorization response, exchanges the code for
// tokens, fetches UserInfo and returns the authenticated username together
// with the post-login redirect target captured in the state parameter.
func (p *Provider) Callback(redirectURI, stateParam, code string) (name, redirectTo string, err error) {
	st, err := decodeState(stateParam)
	if err != nil {
		return "", "", fmt.Errorf("oidc: invalid state: %w", err)
	}

	// Exchange the code for tokens.
	tokens, err := p.exchangeCode(code, redirectURI, st.CodeVerifier)
	if err != nil {
		return "", "", fmt.Errorf("oidc: token exchange failed: %w", err)
	}

	// Fetch UserInfo.
	userInfo, err := p.fetchUserInfo(tokens.AccessToken)
	if err != nil {
		return "", "", fmt.Errorf("oidc: userinfo failed: %w", err)
	}

	// Allowlist check: the sub must match AUTH_USER (if AUTH_USER is set).
	if p.AllowName != "" && userInfo.Sub != p.AllowName {
		return "", "", fmt.Errorf("oidc: user %q is not authorized", userInfo.Sub)
	}

	name = userInfo.Name
	if name == "" {
		name = userInfo.Email
	}
	if name == "" {
		name = userInfo.Sub
	}
	return name, st.RedirectTo, nil
}

func decodeState(s string) (*state, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (p *Provider) exchangeCode(code, redirectURI, codeVerifier string) (*Tokens, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", redirectURI)
	data.Set("client_id", p.ClientID)
	data.Set("client_secret", p.ClientSecret)
	data.Set("code_verifier", codeVerifier)

	req, err := http.NewRequest("POST", p.TokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("oidc: token exchange HTTP %d: %s", resp.StatusCode, body)
	}
	var tok Tokens
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func (p *Provider) fetchUserInfo(accessToken string) (*UserInfo, error) {
	req, err := http.NewRequest("GET", p.UserInfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("oidc: userinfo HTTP %d: %s", resp.StatusCode, body)
	}
	var ui UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&ui); err != nil {
		return nil, err
	}
	return &ui, nil
}
