package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/splattner/burrow/internal/config"
)

func TestVerifierJWTRejectsMissingBearer(t *testing.T) {
	v, err := NewVerifier(config.Config{JWTAlg: "HS256", JWTHMACSecret: "jwt-secret"})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	req := httptest.NewRequest("GET", "/ws", nil)
	if err := v.Authorize(req); err == nil {
		t.Fatal("expected missing bearer auth failure")
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{name: "missing bearer", err: errMissingBearer, want: ErrorCodeMissingBearer},
		{name: "expired", err: fmt.Errorf("wrap: %w", errTokenExpired), want: ErrorCodeTokenExpired},
		{name: "not yet valid", err: fmt.Errorf("wrap: %w", errTokenNotYetValid), want: ErrorCodeTokenNotYetValid},
		{name: "invalid", err: fmt.Errorf("wrap: %w", errInvalidToken), want: ErrorCodeInvalidToken},
		{name: "unknown", err: fmt.Errorf("something else"), want: ErrorCodeUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			if got != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}

func TestVerifierJWT(t *testing.T) {
	v, err := NewVerifier(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "jwt-secret",
		JWTIssuer:     "https://issuer.example",
		JWTAudience:   "burrow-server",
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	token, err := signedHMACToken("jwt-secret", "https://issuer.example", "burrow-server")
	if err != nil {
		t.Fatalf("build token: %v", err)
	}

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	authID, err := v.Authenticate(req)
	if err != nil {
		t.Fatalf("expected jwt auth pass: %v", err)
	}
	if authID.Method != "jwt" {
		t.Fatalf("expected jwt method, got %q", authID.Method)
	}
	if authID.Subject != "client-a" {
		t.Fatalf("expected jwt subject client-a, got %q", authID.Subject)
	}
}

func TestVerifierJWTRejectsInvalidToken(t *testing.T) {
	v, err := NewVerifier(config.Config{
		JWTAlg:        "HS256",
		JWTHMACSecret: "jwt-secret",
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer invalid")

	if err := v.Authorize(req); err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestVerifierJWTWithJWKS(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	jwks := map[string]any{
		"keys": []map[string]string{jwkFromRSA("kid-1", &privateKey.PublicKey)},
	}

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer jwksServer.Close()

	v, err := NewVerifier(config.Config{
		JWTAlg:      "RS256",
		JWKSURL:     jwksServer.URL,
		JWTAudience: "burrow-server",
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	token, err := signedRSAToken(privateKey, "kid-1", "burrow-server", "client-a")
	if err != nil {
		t.Fatalf("build token: %v", err)
	}

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	authID, err := v.Authenticate(req)
	if err != nil {
		t.Fatalf("expected jwks jwt auth pass: %v", err)
	}
	if authID.Method != "jwt" {
		t.Fatalf("expected jwt method, got %q", authID.Method)
	}
	if authID.Subject != "client-a" {
		t.Fatalf("expected jwt subject client-a, got %q", authID.Subject)
	}
}

func signedHMACToken(secret, issuer, audience string) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    issuer,
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(2 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		NotBefore: jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		Subject:   "client-a",
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(secret))
}

func signedRSAToken(key *rsa.PrivateKey, kid, audience, subject string) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    "https://issuer.example",
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(2 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		NotBefore: jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		Subject:   subject,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = kid
	return t.SignedString(key)
}

func jwkFromRSA(kid string, key *rsa.PublicKey) map[string]string {
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := key.E
	eBytes := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	if e <= 0xFFFF {
		eBytes = eBytes[1:]
	}
	if e <= 0xFF {
		eBytes = eBytes[2:]
	}

	return map[string]string{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   n,
		"e":   base64.RawURLEncoding.EncodeToString(eBytes),
	}
}
