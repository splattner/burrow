package auth

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/splattner/burrow/internal/config"
)

type Verifier struct {
	jwtVerifier *jwtVerifier
}

type ErrorCode string

const (
	ErrorCodeUnknown          ErrorCode = "unknown"
	ErrorCodeMissingBearer    ErrorCode = "missing_bearer"
	ErrorCodeTokenExpired     ErrorCode = "token_expired"
	ErrorCodeTokenNotYetValid ErrorCode = "token_not_yet_valid"
	ErrorCodeInvalidToken     ErrorCode = "invalid_token"
	ErrorCodeVerifierConfig   ErrorCode = "verifier_config"
)

var errMissingBearer = errors.New("missing bearer token")
var errTokenExpired = errors.New("token expired")
var errTokenNotYetValid = errors.New("token not yet valid")
var errInvalidToken = errors.New("invalid token")
var errVerifierConfig = errors.New("verifier config")

type Identity struct {
	Method  string
	Subject string
}

type jwtVerifier struct {
	alg      string
	issuer   string
	audience string
	key      any
	jwks     *jwksVerifier
}

type jwksVerifier struct {
	url             string
	refreshInterval time.Duration
	httpClient      *http.Client

	mu          sync.RWMutex
	keys        map[string]any
	lastRefresh time.Time
}

type jwksDocument struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg,omitempty"`
	// RSA fields
	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`
	// EC / OKP (EdDSA) fields
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

type tunnelClaims struct {
	jwt.RegisteredClaims
}

func NewVerifier(cfg config.Config) (*Verifier, error) {
	jv, err := newJWTVerifier(cfg)
	if err != nil {
		return nil, err
	}
	return &Verifier{jwtVerifier: jv}, nil
}

func (v *Verifier) Authorize(r *http.Request) error {
	_, err := v.Authenticate(r)
	return err
}

func (v *Verifier) Authenticate(r *http.Request) (Identity, error) {
	bearer := bearerToken(r)
	if v.jwtVerifier == nil {
		return Identity{}, fmt.Errorf("%w: jwt verifier not configured", errVerifierConfig)
	}
	if bearer == "" {
		return Identity{}, errMissingBearer
	}
	claims, err := v.jwtVerifier.verify(bearer)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Method: "jwt", Subject: claims.Subject}, nil
}

func newJWTVerifier(cfg config.Config) (*jwtVerifier, error) {
	alg := strings.TrimSpace(cfg.JWTAlg)
	if alg == "" {
		alg = "RS256"
	}

	jv := &jwtVerifier{
		alg:      alg,
		issuer:   strings.TrimSpace(cfg.JWTIssuer),
		audience: strings.TrimSpace(cfg.JWTAudience),
	}

	if strings.TrimSpace(cfg.JWKSURL) != "" {
		if strings.HasPrefix(alg, "HS") {
			return nil, fmt.Errorf("BURROW_JWKS_URL cannot be used with symmetric JWT alg %q", alg)
		}
		jv.jwks = &jwksVerifier{
			url:             strings.TrimSpace(cfg.JWKSURL),
			refreshInterval: cfg.JWKSRefresh,
			httpClient:      &http.Client{Timeout: 5 * time.Second},
			keys:            make(map[string]any),
		}
		return jv, nil
	}

	if strings.TrimSpace(cfg.JWTHMACSecret) != "" {
		jv.key = []byte(cfg.JWTHMACSecret)
		return jv, nil
	}
	if strings.TrimSpace(cfg.JWTPublicKeyFile) == "" {
		return nil, fmt.Errorf("JWT verifier source missing: set BURROW_JWT_HMAC_SECRET or BURROW_JWT_PUBLIC_KEY_FILE or BURROW_JWKS_URL")
	}

	pem, err := os.ReadFile(cfg.JWTPublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read JWT public key file: %w", err)
	}

	switch {
	case strings.HasPrefix(alg, "RS"):
		key, parseErr := jwt.ParseRSAPublicKeyFromPEM(pem)
		if parseErr != nil {
			return nil, fmt.Errorf("parse RSA public key: %w", parseErr)
		}
		jv.key = key
	case strings.HasPrefix(alg, "ES"):
		key, parseErr := jwt.ParseECPublicKeyFromPEM(pem)
		if parseErr != nil {
			return nil, fmt.Errorf("parse EC public key: %w", parseErr)
		}
		jv.key = key
	case alg == "EdDSA":
		key, parseErr := jwt.ParseEdPublicKeyFromPEM(pem)
		if parseErr != nil {
			return nil, fmt.Errorf("parse EdDSA public key: %w", parseErr)
		}
		jv.key = key
	default:
		return nil, fmt.Errorf("unsupported JWT algorithm %q", alg)
	}

	return jv, nil
}

func (v *jwtVerifier) verify(rawToken string) (tunnelClaims, error) {
	claims := &tunnelClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{v.alg}),
		jwt.WithLeeway(30*time.Second),
	)

	token, err := parser.ParseWithClaims(rawToken, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != v.alg {
			return nil, fmt.Errorf("unexpected JWT alg %q", token.Method.Alg())
		}
		if v.jwks != nil {
			return v.jwks.keyForToken(token, v.alg)
		}
		return v.key, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return tunnelClaims{}, fmt.Errorf("%w: %v", errTokenExpired, err)
		}
		if errors.Is(err, jwt.ErrTokenNotValidYet) {
			return tunnelClaims{}, fmt.Errorf("%w: %v", errTokenNotYetValid, err)
		}
		return tunnelClaims{}, fmt.Errorf("verify JWT: %w", err)
	}
	if !token.Valid {
		return tunnelClaims{}, fmt.Errorf("%w: JWT validity check failed", errInvalidToken)
	}
	if v.issuer != "" && claims.Issuer != v.issuer {
		return tunnelClaims{}, fmt.Errorf("unexpected JWT issuer %q", claims.Issuer)
	}
	if v.audience != "" && !audContains(claims.Audience, v.audience) {
		return tunnelClaims{}, fmt.Errorf("missing expected JWT audience %q", v.audience)
	}
	return *claims, nil
}

func ClassifyError(err error) ErrorCode {
	if err == nil {
		return ErrorCodeUnknown
	}
	if errors.Is(err, errMissingBearer) {
		return ErrorCodeMissingBearer
	}
	if errors.Is(err, errTokenExpired) {
		return ErrorCodeTokenExpired
	}
	if errors.Is(err, errTokenNotYetValid) {
		return ErrorCodeTokenNotYetValid
	}
	if errors.Is(err, errVerifierConfig) {
		return ErrorCodeVerifierConfig
	}
	if errors.Is(err, errInvalidToken) || strings.Contains(strings.ToLower(err.Error()), "invalid") {
		return ErrorCodeInvalidToken
	}
	return ErrorCodeUnknown
}

func (j *jwksVerifier) keyForToken(token *jwt.Token, expectedAlg string) (any, error) {
	kidRaw, ok := token.Header["kid"]
	if !ok {
		return nil, fmt.Errorf("missing JWT kid header")
	}
	kid, ok := kidRaw.(string)
	if !ok || strings.TrimSpace(kid) == "" {
		return nil, fmt.Errorf("invalid JWT kid header")
	}

	if key, ok := j.lookupKey(kid); ok && !j.refreshDue() {
		return key, nil
	}

	if err := j.refresh(expectedAlg); err != nil {
		if key, ok := j.lookupKey(kid); ok {
			return key, nil
		}
		return nil, err
	}

	key, ok := j.lookupKey(kid)
	if !ok {
		return nil, fmt.Errorf("no jwks key found for kid %q", kid)
	}
	return key, nil
}

func (j *jwksVerifier) lookupKey(kid string) (any, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	key, ok := j.keys[kid]
	return key, ok
}

func (j *jwksVerifier) refreshDue() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.lastRefresh.IsZero() {
		return true
	}
	return time.Since(j.lastRefresh) >= j.refreshInterval
}

func (j *jwksVerifier) refresh(expectedAlg string) error {
	resp, err := j.httpClient.Get(j.url)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS returned status %d", resp.StatusCode)
	}

	var doc jwksDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]any)
	for _, jwk := range doc.Keys {
		if strings.TrimSpace(jwk.Kid) == "" {
			continue
		}
		if jwk.Use != "" && jwk.Use != "sig" {
			continue
		}
		if jwk.Alg != "" && jwk.Alg != expectedAlg {
			continue
		}

		var key any
		var err error
		switch jwk.Kty {
		case "RSA":
			key, err = parseJWKRSAKey(jwk)
		case "EC":
			key, err = parseJWKECKey(jwk)
		case "OKP":
			key, err = parseJWKEdDSAKey(jwk)
		default:
			continue
		}
		if err != nil {
			continue
		}
		keys[jwk.Kid] = key
	}

	if len(keys) == 0 {
		return fmt.Errorf("jwks contains no usable keys")
	}

	j.mu.Lock()
	j.keys = keys
	j.lastRefresh = time.Now()
	j.mu.Unlock()

	return nil
}

func parseJWKRSAKey(jwk jwkKey) (*rsa.PublicKey, error) {
	if jwk.Kty != "RSA" {
		return nil, fmt.Errorf("unsupported jwk kty %q", jwk.Kty)
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil {
		return nil, fmt.Errorf("decode jwk n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil {
		return nil, fmt.Errorf("decode jwk e: %w", err)
	}
	if len(eBytes) == 0 {
		return nil, fmt.Errorf("invalid jwk exponent")
	}

	exponent := 0
	for _, b := range eBytes {
		exponent = exponent<<8 + int(b)
	}
	if exponent <= 0 {
		return nil, fmt.Errorf("invalid jwk exponent value")
	}

	modulus := new(big.Int).SetBytes(nBytes)
	if modulus.Sign() <= 0 {
		return nil, fmt.Errorf("invalid jwk modulus")
	}

	return &rsa.PublicKey{N: modulus, E: exponent}, nil
}

// parseJWKECKey parses an EC public key from a JWK with kty=="EC".
// Supported curves: P-256, P-384, P-521.
func parseJWKECKey(jwk jwkKey) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	var ecdhCurve ecdh.Curve
	switch jwk.Crv {
	case "P-256":
		curve = elliptic.P256()
		ecdhCurve = ecdh.P256()
	case "P-384":
		curve = elliptic.P384()
		ecdhCurve = ecdh.P384()
	case "P-521":
		curve = elliptic.P521()
		ecdhCurve = ecdh.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", jwk.Crv)
	}

	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil || len(xBytes) == 0 {
		return nil, fmt.Errorf("invalid jwk EC x coordinate")
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil || len(yBytes) == 0 {
		return nil, fmt.Errorf("invalid jwk EC y coordinate")
	}

	// Validate the point is on the curve using crypto/ecdh (crypto/elliptic.IsOnCurve
	// is deprecated since Go 1.21). Uncompressed point format: 0x04 || x || y.
	uncompressed := make([]byte, 1+len(xBytes)+len(yBytes))
	uncompressed[0] = 0x04
	copy(uncompressed[1:], xBytes)
	copy(uncompressed[1+len(xBytes):], yBytes)
	if _, err := ecdhCurve.NewPublicKey(uncompressed); err != nil {
		return nil, fmt.Errorf("jwk EC point is not on curve %s: %w", jwk.Crv, err)
	}

	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}

// parseJWKEdDSAKey parses an Ed25519 public key from a JWK with kty=="OKP".
func parseJWKEdDSAKey(jwk jwkKey) (ed25519.PublicKey, error) {
	if jwk.Crv != "Ed25519" {
		return nil, fmt.Errorf("unsupported OKP curve %q", jwk.Crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("invalid jwk EdDSA x: %w", err)
	}
	if len(xBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid Ed25519 key size: got %d want %d", len(xBytes), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(xBytes), nil
}

func bearerToken(r *http.Request) string {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(authz, prefix))
}

func audContains(aud jwt.ClaimStrings, expected string) bool {
	for _, got := range aud {
		if got == expected {
			return true
		}
	}
	return false
}
