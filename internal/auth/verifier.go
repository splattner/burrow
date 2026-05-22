package auth

import (
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
	"github.com/sebastian/k8s-reverse-tunnel/internal/config"
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
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
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
			return nil, fmt.Errorf("KRT_JWKS_URL cannot be used with symmetric JWT alg %q", alg)
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
		return nil, fmt.Errorf("JWT verifier source missing: set KRT_JWT_HMAC_SECRET or KRT_JWT_PUBLIC_KEY_FILE or KRT_JWKS_URL")
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
	defer resp.Body.Close()

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

		key, err := parseJWKRSAKey(jwk)
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

func parseJWKRSAKey(jwk jwkKey) (any, error) {
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
