package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// jwtBearerGrant is the assertion grant type for service accounts.
const jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// serviceAccountKey is the subset of a Google service account JSON key we need.
type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

// loadServiceAccountKey reads and validates a service account key file.
func loadServiceAccountKey(path string) (serviceAccountKey, error) {
	var key serviceAccountKey
	raw, err := os.ReadFile(path)
	if err != nil {
		return key, fmt.Errorf("could not read service account key %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &key); err != nil {
		return key, fmt.Errorf("service account key %s is not valid JSON: %w", path, err)
	}
	if key.Type != "service_account" {
		return key, fmt.Errorf("%s is a %q credential, not a service account key", path, key.Type)
	}
	if key.ClientEmail == "" || key.PrivateKey == "" {
		return key, fmt.Errorf("service account key %s is missing client_email or private_key", path)
	}
	if key.TokenURI == "" {
		key.TokenURI = tokenEndpoint
	}
	return key, nil
}

// parsePrivateKey decodes the PEM-wrapped RSA key from a service account file.
func parsePrivateKey(pemBody string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemBody))
	if block == nil {
		return nil, errors.New("private_key is not valid PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private_key is a %T, want an RSA key", parsed)
		}
		return rsaKey, nil
	}
	// Older keys are emitted as PKCS#1.
	rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("could not parse private_key: %w", err)
	}
	return rsaKey, nil
}

// signedAssertion builds the RS256 JWT that is exchanged for an access token.
//
// Setting the "sub" claim is domain-wide delegation: the service account acts
// as that user. Without it a service account can only see its own (empty)
// Drive and has no mailbox at all, which is why --subject matters.
func (m *Manager) signedAssertion(key serviceAccountKey, now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if key.PrivateKeyID != "" {
		header["kid"] = key.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   key.ClientEmail,
		"scope": m.Scopes,
		"aud":   key.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	if m.Subject != "" {
		claims["sub"] = m.Subject
	}

	encode := func(value any) (string, error) {
		raw, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	encodedHeader, err := encode(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encode(claims)
	if err != nil {
		return "", err
	}

	signingInput := encodedHeader + "." + encodedClaims
	privateKey, err := parsePrivateKey(key.PrivateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(nil, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("could not sign assertion: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// serviceAccountCachePath keys the cache by identity, subject and scopes so
// that switching --subject cannot hand back the previous user's token.
func (m *Manager) serviceAccountCachePath(key serviceAccountKey) string {
	digest := sha256.Sum256([]byte(key.ClientEmail + "|" + m.Subject + "|" + m.Scopes))
	name := fmt.Sprintf("sa_%s.json", base64.RawURLEncoding.EncodeToString(digest[:12]))
	return filepath.Join(filepath.Dir(m.CachePath), name)
}

// serviceAccountToken mints (or reuses) an access token for a service account.
func (m *Manager) serviceAccountToken(ctx context.Context, path string) (string, error) {
	key, err := loadServiceAccountKey(path)
	if err != nil {
		return "", err
	}
	cachePath := m.serviceAccountCachePath(key)

	if raw, err := os.ReadFile(cachePath); err == nil {
		var cached Tokens
		if json.Unmarshal(raw, &cached) == nil && !cached.Expired(m.Now()) {
			return cached.AccessToken, nil
		}
	}

	now := m.Now()
	assertion, err := m.signedAssertion(key, now)
	if err != nil {
		return "", err
	}
	var tokens Tokens
	values := map[string][]string{
		"grant_type": {jwtBearerGrant},
		"assertion":  {assertion},
	}
	if err := m.postForm(ctx, values, &tokens); err != nil {
		return "", m.describeServiceAccountFailure(err)
	}
	tokens.SavedAt = now.Unix()

	if marshalled, err := json.MarshalIndent(tokens, "", "  "); err == nil {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err == nil {
			_ = os.WriteFile(cachePath, marshalled, 0o600)
		}
	}
	return tokens.AccessToken, nil
}

// describeServiceAccountFailure turns Google's opaque delegation errors into
// something actionable. "unauthorized_client" almost always means the key was
// never granted the requested scopes in the Admin console.
func (m *Manager) describeServiceAccountFailure(err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "unauthorized_client"):
		return fmt.Errorf("%w\n\nThe service account is not authorized for these scopes. In the "+
			"Admin console, go to Security > Access and data control > API controls > "+
			"Domain-wide delegation and grant the client ID exactly these scopes:\n  %s",
			err, strings.Join(strings.Fields(m.Scopes), "\n  "))
	case strings.Contains(message, "invalid_grant") && m.Subject != "":
		return fmt.Errorf("%w\n\nCheck that %q is a real user in the domain the service account "+
			"is delegated for", err, m.Subject)
	case strings.Contains(message, "invalid_grant"):
		return fmt.Errorf("%w\n\nService accounts have no mailbox or Drive of their own. Set "+
			"--subject (or GWSG_SUBJECT) to the user you want to act as", err)
	default:
		return err
	}
}
