package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// loginTimeout bounds how long we wait for the user to finish consenting.
const loginTimeout = 5 * time.Minute

// randomURLSafe returns n cryptographically random bytes, base64url encoded.
func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkce is an RFC 7636 verifier/challenge pair. Google requires PKCE for
// desktop clients, and it is what makes the loopback flow safe without
// relying on the client secret staying secret.
type pkce struct {
	verifier  string
	challenge string
}

func newPKCE() (pkce, error) {
	verifier, err := randomURLSafe(64)
	if err != nil {
		return pkce{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return pkce{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// callbackResult carries the outcome of the single loopback request.
type callbackResult struct {
	code string
	err  error
}

// Login runs the PKCE loopback flow and caches the resulting credential.
//
// Google removed the out-of-band (urn:ietf:wg:oauth:2.0:oob) flow, so a
// loopback listener on 127.0.0.1 is the supported path for CLIs. We bind
// port 0 and let the OS choose, which avoids collisions with anything else
// on the machine.
func (m *Manager) Login(ctx context.Context, messages io.Writer) (Tokens, error) {
	if m.ClientID == "" {
		return Tokens{}, errors.New("GWSG_CLIENT_ID is not set; see `gwsg auth setup`")
	}

	verifier, err := newPKCE()
	if err != nil {
		return Tokens{}, err
	}
	state, err := randomURLSafe(24)
	if err != nil {
		return Tokens{}, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Tokens{}, fmt.Errorf("could not open a loopback listener: %w", err)
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)

	results := make(chan callbackResult, 1)
	server := &http.Server{
		Handler:           http.HandlerFunc(m.callbackHandler(state, results)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := m.authCodeURL(redirectURI, state, verifier.challenge)
	fmt.Fprintf(messages, "Open this URL to authorize gwsg:\n\n%s\n\n", authURL)
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(messages, "(could not open a browser automatically: %v)\n", err)
	}
	fmt.Fprintln(messages, "Waiting for the redirect...")

	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var result callbackResult
	select {
	case result = <-results:
	case <-ctx.Done():
		return Tokens{}, fmt.Errorf("timed out waiting for authorization: %w", ctx.Err())
	}
	if result.err != nil {
		return Tokens{}, result.err
	}

	tokens, err := m.exchange(ctx, result.code, redirectURI, verifier.verifier)
	if err != nil {
		return Tokens{}, err
	}
	if err := m.Save(tokens); err != nil {
		return Tokens{}, err
	}
	fmt.Fprintf(messages, "Authorized. Credential cached at %s\n", m.CachePath)
	return tokens, nil
}

// authCodeURL builds the consent URL.
func (m *Manager) authCodeURL(redirectURI, state, challenge string) string {
	values := url.Values{
		"client_id":             {m.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {m.Scopes},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		// offline + consent are what actually produce a refresh token.
		"access_type": {"offline"},
		"prompt":      {"consent"},
	}
	return authEndpoint + "?" + values.Encode()
}

// callbackHandler validates state and captures the authorization code.
func (m *Manager) callbackHandler(state string, results chan<- callbackResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		reply := func(status int, body string, result callbackResult) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			select {
			case results <- result:
			default:
			}
		}
		if errCode := query.Get("error"); errCode != "" {
			reply(http.StatusBadRequest, "Authorization failed. You can close this tab.\n",
				callbackResult{err: fmt.Errorf("authorization denied: %s", errCode)})
			return
		}
		// Constant-time comparison is overkill here, but the check itself is
		// what prevents a cross-site request forging the callback.
		if query.Get("state") != state {
			reply(http.StatusBadRequest, "State mismatch. You can close this tab.\n",
				callbackResult{err: errors.New("state mismatch; aborting")})
			return
		}
		code := query.Get("code")
		if code == "" {
			reply(http.StatusBadRequest, "No authorization code. You can close this tab.\n",
				callbackResult{err: errors.New("callback contained no authorization code")})
			return
		}
		reply(http.StatusOK, "gwsg is authorized. You can close this tab.\n", callbackResult{code: code})
	}
}

// exchange trades the authorization code for tokens.
func (m *Manager) exchange(ctx context.Context, code, redirectURI, verifier string) (Tokens, error) {
	values := url.Values{
		"client_id":     {m.ClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	if m.ClientSecret != "" {
		values.Set("client_secret", m.ClientSecret)
	}
	var tokens Tokens
	if err := m.postForm(ctx, values, &tokens); err != nil {
		return Tokens{}, err
	}
	if tokens.RefreshToken == "" {
		return Tokens{}, errors.New("Google returned no refresh token; re-run with a fresh consent prompt")
	}
	tokens.SavedAt = m.Now().Unix()
	return tokens, nil
}

// openBrowser makes a best-effort attempt to launch the consent page.
// Headless machines are expected to fail here; the URL is always printed.
func openBrowser(target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		command = "xdg-open"
	}
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("%s not available", command)
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return exec.Command(command, append(args, target)...).Start()
	}
	return errors.New("refusing to open a non-http target")
}
