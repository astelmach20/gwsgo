package auth

import (
	"testing"
	"time"
)

func TestTokensExpiredTreatsZeroExpiryAsExpired(t *testing.T) {
	// A cached token with no expiry is how a CLI ends up permanently 401ing:
	// treat it as expired so the refresh path runs.
	tokens := Tokens{AccessToken: "x", SavedAt: time.Now().Unix()}
	if !tokens.Expired(time.Now()) {
		t.Error("a token with ExpiresIn == 0 must be treated as expired")
	}
}

func TestTokensExpiredRespectsSkew(t *testing.T) {
	now := time.Now()
	// Expires in 30s, which is inside the 60s renewal skew.
	tokens := Tokens{AccessToken: "x", SavedAt: now.Unix(), ExpiresIn: 30}
	if !tokens.Expired(now) {
		t.Error("a token expiring inside the skew window must be treated as expired")
	}
}

func TestTokensValidWhenFresh(t *testing.T) {
	now := time.Now()
	tokens := Tokens{AccessToken: "x", SavedAt: now.Unix(), ExpiresIn: 3600}
	if tokens.Expired(now) {
		t.Error("a freshly minted token must not be treated as expired")
	}
}

func TestTokensExpiredWithoutAccessToken(t *testing.T) {
	if !(Tokens{}).Expired(time.Now()) {
		t.Error("an empty credential must be treated as expired")
	}
}

func TestPKCEChallengeIsDerivedFromVerifier(t *testing.T) {
	pair, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	if pair.verifier == "" || pair.challenge == "" {
		t.Fatal("PKCE pair must be populated")
	}
	if pair.verifier == pair.challenge {
		t.Error("the challenge must be a hash of the verifier, not the verifier itself")
	}
	// RFC 7636 requires a verifier of 43..128 characters.
	if len(pair.verifier) < 43 || len(pair.verifier) > 128 {
		t.Errorf("verifier length %d is outside the RFC 7636 range", len(pair.verifier))
	}
}

func TestPKCEPairsAreUnique(t *testing.T) {
	first, _ := newPKCE()
	second, _ := newPKCE()
	if first.verifier == second.verifier {
		t.Error("PKCE verifiers must not repeat")
	}
}

func TestAuthCodeURLRequestsOfflineAccessAndPKCE(t *testing.T) {
	m := &Manager{ClientID: "client", Scopes: "scope-a scope-b", Now: time.Now}
	got := m.authCodeURL("http://127.0.0.1:1234", "state-value", "challenge-value")
	for _, want := range []string{
		"code_challenge=challenge-value",
		"code_challenge_method=S256",
		"access_type=offline",
		"prompt=consent",
		"state=state-value",
	} {
		if !contains(got, want) {
			t.Errorf("auth URL is missing %q:\n%s", want, got)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
