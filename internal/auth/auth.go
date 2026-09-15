// Package auth obtains Google OAuth2 access tokens for the gwsg CLI.
//
// Three credential sources are supported, in precedence order:
//
//  1. GWSG_TOKEN            a pre-minted access token (e.g. gcloud auth print-access-token)
//  2. a service account key referenced by GOOGLE_APPLICATION_CREDENTIALS,
//     optionally impersonating a user via GWSG_SUBJECT / --subject (domain-wide delegation)
//  3. a cached user credential obtained by `gwsg auth login` (PKCE loopback flow)
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"

	// defaultScopes covers the services a personal user typically drives from a
	// terminal. Override with GWSG_SCOPES for admin or read-only footprints.
	defaultScopes = "https://www.googleapis.com/auth/gmail.modify " +
		"https://www.googleapis.com/auth/calendar " +
		"https://www.googleapis.com/auth/drive " +
		"https://www.googleapis.com/auth/spreadsheets " +
		"https://www.googleapis.com/auth/documents " +
		"https://www.googleapis.com/auth/tasks " +
		"https://www.googleapis.com/auth/contacts " +
		"https://www.googleapis.com/auth/userinfo.email"

	// expiryskew renews slightly early so a token cannot expire mid-flight.
	expirySkew = 60 * time.Second
)

// Tokens is the on-disk credential cache.
type Tokens struct {
	TokenType    string `json:"token_type,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	SavedAt      int64  `json:"saved_at,omitempty"`
}

// Expired reports whether the cached access token is unusable at time now.
//
// A zero ExpiresIn is treated as expired rather than as "never expires":
// caching a token with no expiry is how a CLI ends up permanently 401ing.
func (t Tokens) Expired(now time.Time) bool {
	if t.AccessToken == "" || t.SavedAt == 0 || t.ExpiresIn == 0 {
		return true
	}
	expiry := time.Unix(t.SavedAt, 0).Add(time.Duration(t.ExpiresIn) * time.Second)
	return !now.Add(expirySkew).Before(expiry)
}

// OAuthError is the error body returned by Google's token endpoint.
type OAuthError struct {
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (e OAuthError) failure() error {
	if e.ErrorDescription != "" {
		return fmt.Errorf("oauth error %s: %s", e.ErrorCode, e.ErrorDescription)
	}
	return fmt.Errorf("oauth error %s", e.ErrorCode)
}

// Manager resolves access tokens from whichever credential source is configured.
type Manager struct {
	ClientID     string
	ClientSecret string
	Scopes       string
	CachePath    string
	// Subject impersonates a user via domain-wide delegation. Service accounts only.
	Subject string
	HTTP    *http.Client
	Now     func() time.Time
}

// New builds a Manager from the environment.
func New() *Manager {
	scopes := strings.TrimSpace(os.Getenv("GWSG_SCOPES"))
	if scopes == "" {
		scopes = defaultScopes
	}
	return &Manager{
		ClientID:     strings.TrimSpace(os.Getenv("GWSG_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("GWSG_CLIENT_SECRET")),
		Scopes:       scopes,
		Subject:      strings.TrimSpace(os.Getenv("GWSG_SUBJECT")),
		CachePath:    TokenCachePath(),
		HTTP:         &http.Client{Timeout: 60 * time.Second},
		Now:          time.Now,
	}
}

// ConfigDir is where gwsg keeps its token cache and Discovery cache.
func ConfigDir() string {
	if dir := os.Getenv("GWSG_CONFIG_DIR"); dir != "" {
		return dir
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "gwsg")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gwsg")
}

// TokenCachePath is the location of the cached user credential.
func TokenCachePath() string {
	if path := os.Getenv("GWSG_TOKEN_CACHE"); path != "" {
		return path
	}
	return filepath.Join(ConfigDir(), "tokens.json")
}

// Load reads the cached credential. A missing cache is not an error.
func (m *Manager) Load() (Tokens, error) {
	var tokens Tokens
	raw, err := os.ReadFile(m.CachePath)
	if errors.Is(err, os.ErrNotExist) {
		return tokens, nil
	}
	if err != nil {
		return tokens, err
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return Tokens{}, fmt.Errorf("token cache %s is corrupt: %w", m.CachePath, err)
	}
	return tokens, nil
}

// Save writes the credential cache with owner-only permissions.
func (m *Manager) Save(tokens Tokens) error {
	if tokens.SavedAt == 0 {
		tokens.SavedAt = m.Now().Unix()
	}
	if err := os.MkdirAll(filepath.Dir(m.CachePath), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a crash cannot leave a truncated cache behind.
	tmp := m.CachePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.CachePath)
}

// Logout removes the cached credential, reporting whether one was present.
func (m *Manager) Logout() (bool, error) {
	err := os.Remove(m.CachePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (m *Manager) postForm(ctx context.Context, values url.Values, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var oauthErr OAuthError
		if json.Unmarshal(raw, &oauthErr) == nil && oauthErr.ErrorCode != "" {
			return oauthErr.failure()
		}
		return fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, result)
}

// AccessToken returns a usable bearer token, refreshing or minting as needed.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	if token := strings.TrimSpace(os.Getenv("GWSG_TOKEN")); token != "" {
		return token, nil
	}
	if path := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); path != "" {
		return m.serviceAccountToken(ctx, path)
	}
	tokens, err := m.Load()
	if err != nil {
		return "", err
	}
	if !tokens.Expired(m.Now()) {
		return tokens.AccessToken, nil
	}
	if tokens.RefreshToken == "" {
		return "", errors.New("no usable credential; run `gwsg auth login`")
	}
	refreshed, err := m.refresh(ctx, tokens.RefreshToken)
	if err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

func (m *Manager) refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	if m.ClientID == "" {
		return Tokens{}, errors.New("GWSG_CLIENT_ID is not set; see `gwsg auth setup`")
	}
	values := url.Values{
		"client_id":     {m.ClientID},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	if m.ClientSecret != "" {
		values.Set("client_secret", m.ClientSecret)
	}
	var tokens Tokens
	if err := m.postForm(ctx, values, &tokens); err != nil {
		return Tokens{}, err
	}
	// Google omits the refresh token on refresh responses; carry it forward.
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	tokens.SavedAt = m.Now().Unix()
	if err := m.Save(tokens); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

// Status summarises the active credential without revealing it.
func (m *Manager) Status() map[string]any {
	status := map[string]any{"config_dir": ConfigDir(), "token_cache": m.CachePath}
	if os.Getenv("GWSG_TOKEN") != "" {
		status["source"] = "GWSG_TOKEN"
		return status
	}
	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		status["source"] = "service_account"
		status["key_file"] = path
		if m.Subject != "" {
			status["impersonating"] = m.Subject
		}
		return status
	}
	tokens, err := m.Load()
	switch {
	case err != nil:
		status["source"] = "error"
		status["error"] = err.Error()
	case tokens.AccessToken == "":
		status["source"] = "none"
		status["logged_in"] = false
	default:
		status["source"] = "user_oauth"
		status["logged_in"] = true
		status["expired"] = tokens.Expired(m.Now())
		status["scopes"] = strings.Fields(tokens.Scope)
	}
	return status
}
