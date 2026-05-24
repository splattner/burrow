package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/splattner/burrow/internal/config"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writePEMFile(t *testing.T, keyType string, der []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "key*.pem")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: keyType, Bytes: der}); err != nil {
		t.Fatalf("pem encode: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

func rsaPublicKeyPEMFile(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal RSA public key: %v", err)
	}
	return writePEMFile(t, "PUBLIC KEY", der)
}

func ecPublicKeyPEMFile(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal EC public key: %v", err)
	}
	return writePEMFile(t, "PUBLIC KEY", der)
}

func edPublicKeyPEMFile(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal EdDSA public key: %v", err)
	}
	return writePEMFile(t, "PUBLIC KEY", der)
}

func signedRSATokenFull(key *rsa.PrivateKey, kid, issuer, audience, subject string, nbf, exp time.Time) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    issuer,
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(exp),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		NotBefore: jwt.NewNumericDate(nbf),
		Subject:   subject,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if kid != "" {
		t.Header["kid"] = kid
	}
	return t.SignedString(key)
}

func signedECToken(key *ecdsa.PrivateKey, audience, subject string) (string, error) {
	claims := jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(2 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		NotBefore: jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		Subject:   subject,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	return t.SignedString(key)
}

func signedEdDSAToken(key ed25519.PrivateKey, audience, subject string) (string, error) {
	claims := jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(2 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		NotBefore: jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		Subject:   subject,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	return t.SignedString(key)
}

func authRequest(token string) *http.Request {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// ---------------------------------------------------------------------------
// newJWTVerifier constructor error paths
// ---------------------------------------------------------------------------

func TestNewVerifier_NoKeySource(t *testing.T) {
	_, err := NewVerifier(config.Config{JWTAlg: "HS256"})
	if err == nil {
		t.Fatal("expected error when no key source is configured")
	}
}

func TestNewVerifier_JWKSWithHMACAlg(t *testing.T) {
	_, err := NewVerifier(config.Config{
		JWTAlg:  "HS256",
		JWKSURL: "https://idp.example/.well-known/jwks.json",
	})
	if err == nil {
		t.Fatal("expected error: JWKS cannot be used with symmetric alg")
	}
}

func TestNewVerifier_PublicKeyFileNotFound(t *testing.T) {
	_, err := NewVerifier(config.Config{
		JWTAlg:           "RS256",
		JWTPublicKeyFile: "/nonexistent/path/key.pem",
	})
	if err == nil {
		t.Fatal("expected error for missing key file")
	}
}

func TestNewVerifier_PublicKeyFileInvalidPEM_RS256(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "bad*.pem")
	_, _ = f.WriteString("this is not a pem")
	_ = f.Close()

	_, err := NewVerifier(config.Config{JWTAlg: "RS256", JWTPublicKeyFile: f.Name()})
	if err == nil {
		t.Fatal("expected parse error for invalid RSA PEM")
	}
}

func TestNewVerifier_PublicKeyFileInvalidPEM_ES256(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "bad*.pem")
	_, _ = f.WriteString("not a real pem block")
	_ = f.Close()

	_, err := NewVerifier(config.Config{JWTAlg: "ES256", JWTPublicKeyFile: f.Name()})
	if err == nil {
		t.Fatal("expected parse error for invalid EC PEM")
	}
}

func TestNewVerifier_PublicKeyFileInvalidPEM_EdDSA(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "bad*.pem")
	_, _ = f.WriteString("not a real pem block")
	_ = f.Close()

	_, err := NewVerifier(config.Config{JWTAlg: "EdDSA", JWTPublicKeyFile: f.Name()})
	if err == nil {
		t.Fatal("expected parse error for invalid EdDSA PEM")
	}
}

func TestNewVerifier_UnsupportedAlg(t *testing.T) {
	// Provide a key file so we reach the algorithm switch — must be valid PEM
	// but the alg "XS512" is not supported.
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemFile := rsaPublicKeyPEMFile(t, rsaKey)

	_, err := NewVerifier(config.Config{JWTAlg: "XS512", JWTPublicKeyFile: pemFile})
	if err == nil {
		t.Fatal("expected error for unsupported alg")
	}
}

// ---------------------------------------------------------------------------
// Key file verification round-trips
// ---------------------------------------------------------------------------

func TestVerifier_RSAPublicKeyFile(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	pemFile := rsaPublicKeyPEMFile(t, rsaKey)

	v, err := NewVerifier(config.Config{
		JWTAlg:           "RS256",
		JWTPublicKeyFile: pemFile,
		JWTAudience:      "burrow-server",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	now := time.Now()
	token, err := signedRSATokenFull(rsaKey, "", "", "burrow-server", "alice",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	id, err := v.Authenticate(authRequest(token))
	if err != nil {
		t.Fatalf("expected auth success: %v", err)
	}
	if id.Subject != "alice" {
		t.Fatalf("expected subject alice, got %q", id.Subject)
	}
}

func TestVerifier_DefaultAlgIsRS256(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemFile := rsaPublicKeyPEMFile(t, rsaKey)

	// Empty JWTAlg should default to RS256
	v, err := NewVerifier(config.Config{JWTPublicKeyFile: pemFile})
	if err != nil {
		t.Fatalf("NewVerifier with empty alg: %v", err)
	}

	now := time.Now()
	token, err := signedRSATokenFull(rsaKey, "", "", "", "bob",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	id, err := v.Authenticate(authRequest(token))
	if err != nil {
		t.Fatalf("expected auth success: %v", err)
	}
	if id.Subject != "bob" {
		t.Fatalf("expected subject bob, got %q", id.Subject)
	}
}

func TestVerifier_ECPublicKeyFile(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}

	pemFile := ecPublicKeyPEMFile(t, ecKey)

	v, err := NewVerifier(config.Config{
		JWTAlg:           "ES256",
		JWTPublicKeyFile: pemFile,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	token, err := signedECToken(ecKey, "", "carol")
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	id, err := v.Authenticate(authRequest(token))
	if err != nil {
		t.Fatalf("expected auth success: %v", err)
	}
	if id.Subject != "carol" {
		t.Fatalf("expected subject carol, got %q", id.Subject)
	}
}

func TestVerifier_EdDSAPublicKeyFile(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate EdDSA key: %v", err)
	}

	pemFile := edPublicKeyPEMFile(t, pub)

	v, err := NewVerifier(config.Config{
		JWTAlg:           "EdDSA",
		JWTPublicKeyFile: pemFile,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	token, err := signedEdDSAToken(priv, "", "dave")
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	id, err := v.Authenticate(authRequest(token))
	if err != nil {
		t.Fatalf("expected auth success: %v", err)
	}
	if id.Subject != "dave" {
		t.Fatalf("expected subject dave, got %q", id.Subject)
	}
}

// ---------------------------------------------------------------------------
// JWT verify edge cases
// ---------------------------------------------------------------------------

func TestVerifier_WrongIssuer(t *testing.T) {
	v, err := NewVerifier(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "secret",
		JWTIssuer:     "https://expected.example",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	token, err := signedHMACToken("secret", "https://other.example", "")
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if err := v.Authorize(authRequest(token)); err == nil {
		t.Fatal("expected rejection for wrong issuer")
	}
}

func TestVerifier_WrongAudience(t *testing.T) {
	v, err := NewVerifier(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "secret",
		JWTAudience:   "burrow-server",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	token, err := signedHMACToken("secret", "", "other-audience")
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if err := v.Authorize(authRequest(token)); err == nil {
		t.Fatal("expected rejection for wrong audience")
	}
}

func TestVerifier_ExpiredToken(t *testing.T) {
	v, err := NewVerifier(config.Config{JWTAlg: "HS256", JWTHMACSecret: "secret"})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Use an expiry well past the 30s leeway
	claims := jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-2 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Minute)),
		NotBefore: jwt.NewNumericDate(time.Now().Add(-5 * time.Minute)),
		Subject:   "eve",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token, err := tok.SignedString([]byte("secret"))
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}

	err = v.Authorize(authRequest(token))
	if err == nil {
		t.Fatal("expected rejection for expired token")
	}
	if ClassifyError(err) != ErrorCodeTokenExpired {
		t.Fatalf("expected ErrorCodeTokenExpired, got %v", ClassifyError(err))
	}
}

func TestVerifier_TokenNotYetValid(t *testing.T) {
	v, err := NewVerifier(config.Config{JWTAlg: "HS256", JWTHMACSecret: "secret"})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// NotBefore well into the future, past the 30s leeway
	claims := jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		NotBefore: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
		Subject:   "frank",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token, err := tok.SignedString([]byte("secret"))
	if err != nil {
		t.Fatalf("sign not-yet-valid token: %v", err)
	}

	err = v.Authorize(authRequest(token))
	if err == nil {
		t.Fatal("expected rejection for not-yet-valid token")
	}
	if ClassifyError(err) != ErrorCodeTokenNotYetValid {
		t.Fatalf("expected ErrorCodeTokenNotYetValid, got %v", ClassifyError(err))
	}
}

// ---------------------------------------------------------------------------
// Authenticate with nil jwtVerifier
// ---------------------------------------------------------------------------

func TestAuthenticate_NilJWTVerifier(t *testing.T) {
	v := &Verifier{jwtVerifier: nil}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer sometoken")

	_, err := v.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for nil jwtVerifier")
	}
	if ClassifyError(err) != ErrorCodeVerifierConfig {
		t.Fatalf("expected ErrorCodeVerifierConfig, got %v", ClassifyError(err))
	}
}

// ---------------------------------------------------------------------------
// refreshDue
// ---------------------------------------------------------------------------

func TestRefreshDue_ZeroTime(t *testing.T) {
	j := &jwksVerifier{keys: make(map[string]any)}
	if !j.refreshDue() {
		t.Fatal("expected refreshDue=true when lastRefresh is zero")
	}
}

func TestRefreshDue_Recent(t *testing.T) {
	j := &jwksVerifier{
		keys:            make(map[string]any),
		refreshInterval: 5 * time.Minute,
		lastRefresh:     time.Now(),
	}
	if j.refreshDue() {
		t.Fatal("expected refreshDue=false when recently refreshed")
	}
}

func TestRefreshDue_Overdue(t *testing.T) {
	j := &jwksVerifier{
		keys:            make(map[string]any),
		refreshInterval: 5 * time.Minute,
		lastRefresh:     time.Now().Add(-10 * time.Minute),
	}
	if !j.refreshDue() {
		t.Fatal("expected refreshDue=true when interval has elapsed")
	}
}

// ---------------------------------------------------------------------------
// keyForToken via full Authenticate flow
// ---------------------------------------------------------------------------

func TestVerifierJWKS_MissingKidHeader(t *testing.T) {
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	jwksBody := map[string]any{
		"keys": []map[string]string{jwkFromRSA("kid-1", &privateKey.PublicKey)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwksBody)
	}))
	defer srv.Close()

	v, err := NewVerifier(config.Config{JWTAlg: "RS256", JWKSURL: srv.URL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Sign a token without setting kid in the header
	now := time.Now()
	token, err := signedRSATokenFull(privateKey, "", "", "", "test-sub",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if err := v.Authorize(authRequest(token)); err == nil {
		t.Fatal("expected rejection for missing kid header")
	}
}

func TestVerifierJWKS_UnknownKid(t *testing.T) {
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	jwksBody := map[string]any{
		"keys": []map[string]string{jwkFromRSA("kid-1", &privateKey.PublicKey)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwksBody)
	}))
	defer srv.Close()

	v, err := NewVerifier(config.Config{JWTAlg: "RS256", JWKSURL: srv.URL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Sign a token with a kid that doesn't exist in the JWKS
	now := time.Now()
	token, err := signedRSATokenFull(privateKey, "unknown-kid", "", "", "test-sub",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if err := v.Authorize(authRequest(token)); err == nil {
		t.Fatal("expected rejection for unknown kid")
	}
}

func TestVerifierJWKS_RefreshServesCachedKeyOnFetchError(t *testing.T) {
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		if callCount == 1 {
			// First request succeeds — populates the cache
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{jwkFromRSA("kid-1", &privateKey.PublicKey)},
			})
		} else {
			// Subsequent requests fail — cache should be served
			http.Error(w, "server error", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	v, err := NewVerifier(config.Config{
		JWTAlg:      "RS256",
		JWKSURL:     srv.URL,
		JWKSRefresh: 0, // interval=0 means refreshDue is always true after first fetch
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	now := time.Now()
	token, err := signedRSATokenFull(privateKey, "kid-1", "", "", "subject",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	// First call: populates cache
	if err := v.Authorize(authRequest(token)); err != nil {
		t.Fatalf("first auth failed: %v", err)
	}

	// Second call: refresh fails, but key was found in cache → should still succeed
	if err := v.Authorize(authRequest(token)); err != nil {
		t.Fatalf("second auth should succeed using cached key: %v", err)
	}
}

func TestVerifierJWKS_CacheHit(t *testing.T) {
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{jwkFromRSA("kid-1", &privateKey.PublicKey)},
		})
	}))
	defer srv.Close()

	v, err := NewVerifier(config.Config{
		JWTAlg:      "RS256",
		JWKSURL:     srv.URL,
		JWKSRefresh: time.Hour, // long interval → cache is used after first fetch
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	now := time.Now()
	token, err := signedRSATokenFull(privateKey, "kid-1", "", "", "cached-sub",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	// First call: populates the cache
	if err := v.Authorize(authRequest(token)); err != nil {
		t.Fatalf("first auth failed: %v", err)
	}

	// Second call: should hit the cache (refreshDue=false) and return early
	if err := v.Authorize(authRequest(token)); err != nil {
		t.Fatalf("second auth (cache hit) failed: %v", err)
	}
}

func TestVerifierJWKS_RefreshFailsWithNoCache(t *testing.T) {
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	// Server is closed before any request — refresh will fail on first attempt
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()

	v, err := NewVerifier(config.Config{
		JWTAlg:  "RS256",
		JWKSURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	now := time.Now()
	token, err := signedRSATokenFull(privateKey, "kid-1", "", "", "nobody",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	// Refresh fails and kid is not in cache → authentication must fail
	if err := v.Authorize(authRequest(token)); err == nil {
		t.Fatal("expected auth failure when refresh fails with empty cache")
	}
}

// ---------------------------------------------------------------------------
// refresh error paths
// ---------------------------------------------------------------------------

func TestRefresh_SkipsUnparsableKey(t *testing.T) {
	// One bad key (invalid N) + one good key in the same JWKS response.
	// The bad key should be silently skipped (continue) and the good key used.
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				// Bad key: valid RSA kty + kid but invalid base64 for N → parseJWKRSAKey fails
				{"kid": "bad-1", "kty": "RSA", "use": "sig", "n": "!!!invalid!!!", "e": "AQAB"},
				// Good key: well-formed RSA JWK (use map[string]any to satisfy slice type)
				func() map[string]any {
					m := make(map[string]any)
					for k, v := range jwkFromRSA("good-1", &privateKey.PublicKey) {
						m[k] = v
					}
					return m
				}(),
			},
		})
	}))
	defer srv.Close()

	v, err := NewVerifier(config.Config{JWTAlg: "RS256", JWKSURL: srv.URL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	now := time.Now()
	token, err := signedRSATokenFull(privateKey, "good-1", "", "", "subject",
		now.Add(-5*time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	// Should succeed using the good key; bad key was skipped without error
	if err := v.Authorize(authRequest(token)); err != nil {
		t.Fatalf("expected auth success despite bad key in JWKS: %v", err)
	}
}

func TestRefresh_HTTPFetchError(t *testing.T) {
	// Point at a closed server so the HTTP request fails
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()

	j := &jwksVerifier{
		url:        srv.URL,
		httpClient: &http.Client{Timeout: time.Second},
		keys:       make(map[string]any),
	}

	if err := j.refresh("RS256"); err == nil {
		t.Fatal("expected error for HTTP fetch failure")
	}
}

func TestRefresh_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	j := &jwksVerifier{
		url:        srv.URL,
		httpClient: &http.Client{Timeout: time.Second},
		keys:       make(map[string]any),
	}

	if err := j.refresh("RS256"); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestRefresh_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{bad json"))
	}))
	defer srv.Close()

	j := &jwksVerifier{
		url:        srv.URL,
		httpClient: &http.Client{Timeout: time.Second},
		keys:       make(map[string]any),
	}

	if err := j.refresh("RS256"); err == nil {
		t.Fatal("expected error for invalid JSON body")
	}
}

func TestRefresh_NoUsableKeys(t *testing.T) {
	// Return a JWKS with a non-RSA key (unsupported kty)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{"kid": "ec-1", "kty": "EC", "use": "sig", "alg": "ES256"},
			},
		})
	}))
	defer srv.Close()

	j := &jwksVerifier{
		url:        srv.URL,
		httpClient: &http.Client{Timeout: time.Second},
		keys:       make(map[string]any),
	}

	if err := j.refresh("RS256"); err == nil {
		t.Fatal("expected error when no usable keys in JWKS response")
	}
}

func TestRefresh_SkipsKeysWithWrongAlg(t *testing.T) {
	// The JWKS contains one key with the wrong alg and one with no kid — both skipped
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{"kid": "k1", "kty": "RSA", "use": "sig", "alg": "RS384", "n": "AA", "e": "AQAB"},
				{"kid": "", "kty": "RSA", "use": "sig", "n": "AA", "e": "AQAB"},
			},
		})
	}))
	defer srv.Close()

	j := &jwksVerifier{
		url:        srv.URL,
		httpClient: &http.Client{Timeout: time.Second},
		keys:       make(map[string]any),
	}

	if err := j.refresh("RS256"); err == nil {
		t.Fatal("expected error when all keys are filtered out")
	}
}

func TestRefresh_SkipsEncryptionKeys(t *testing.T) {
	// A JWKS key with use=enc should be skipped
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{"kid": "enc-1", "kty": "RSA", "use": "enc", "n": "AA", "e": "AQAB"},
			},
		})
	}))
	defer srv.Close()

	j := &jwksVerifier{
		url:        srv.URL,
		httpClient: &http.Client{Timeout: time.Second},
		keys:       make(map[string]any),
	}

	if err := j.refresh("RS256"); err == nil {
		t.Fatal("expected error: enc key should be skipped → no usable keys")
	}
}

// ---------------------------------------------------------------------------
// parseJWKRSAKey
// ---------------------------------------------------------------------------

func TestParseJWKRSAKey_NonRSA(t *testing.T) {
	_, err := parseJWKRSAKey(jwkKey{Kty: "EC"})
	if err == nil {
		t.Fatal("expected error for non-RSA kty")
	}
}

func TestParseJWKRSAKey_BadN(t *testing.T) {
	_, err := parseJWKRSAKey(jwkKey{Kty: "RSA", N: "!!!invalid!!!", E: "AQAB"})
	if err == nil {
		t.Fatal("expected error for invalid base64 N")
	}
}

func TestParseJWKRSAKey_BadE(t *testing.T) {
	n := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x02, 0x03})
	_, err := parseJWKRSAKey(jwkKey{Kty: "RSA", N: n, E: "!!!invalid!!!"})
	if err == nil {
		t.Fatal("expected error for invalid base64 E")
	}
}

func TestParseJWKRSAKey_EmptyExponent(t *testing.T) {
	n := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x02, 0x03})
	// Empty string decodes to []byte{} (len 0) → "invalid jwk exponent"
	_, err := parseJWKRSAKey(jwkKey{Kty: "RSA", N: n, E: ""})
	if err == nil {
		t.Fatal("expected error for empty exponent bytes")
	}
}

func TestParseJWKRSAKey_ZeroExponent(t *testing.T) {
	n := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x02, 0x03})
	e := base64.RawURLEncoding.EncodeToString([]byte{0x00})
	_, err := parseJWKRSAKey(jwkKey{Kty: "RSA", N: n, E: e})
	if err == nil {
		t.Fatal("expected error for zero exponent value")
	}
}

func TestParseJWKRSAKey_ZeroModulus(t *testing.T) {
	n := base64.RawURLEncoding.EncodeToString([]byte{0x00})
	e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}) // 65537
	_, err := parseJWKRSAKey(jwkKey{Kty: "RSA", N: n, E: e})
	if err == nil {
		t.Fatal("expected error for zero modulus")
	}
}

// ---------------------------------------------------------------------------
// bearerToken
// ---------------------------------------------------------------------------

func TestBearerToken_NonBearerScheme(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := bearerToken(req); got != "" {
		t.Fatalf("expected empty string for non-Bearer scheme, got %q", got)
	}
}

func TestBearerToken_EmptyHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	if got := bearerToken(req); got != "" {
		t.Fatalf("expected empty string for missing Authorization, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// audContains
// ---------------------------------------------------------------------------

func TestAudContains_EmptyList(t *testing.T) {
	if audContains(jwt.ClaimStrings{}, "burrow-server") {
		t.Fatal("expected false for empty audience list")
	}
}

func TestAudContains_NoMatch(t *testing.T) {
	if audContains(jwt.ClaimStrings{"a", "b", "c"}, "burrow-server") {
		t.Fatal("expected false when audience not in list")
	}
}

func TestAudContains_Match(t *testing.T) {
	if !audContains(jwt.ClaimStrings{"a", "burrow-server", "c"}, "burrow-server") {
		t.Fatal("expected true when audience is in list")
	}
}
