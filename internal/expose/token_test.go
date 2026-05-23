package expose

import (
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateHMACSecret_Length(t *testing.T) {
	s, err := generateHMACSecret()
	require.NoError(t, err)
	// 32 random bytes hex-encoded = 64 characters
	assert.Len(t, s, 64)
}

func TestGenerateHMACSecret_Unique(t *testing.T) {
	s1, err := generateHMACSecret()
	require.NoError(t, err)
	s2, err := generateHMACSecret()
	require.NoError(t, err)
	assert.NotEqual(t, s1, s2, "two generated secrets should not be equal")
}

func TestMintToken_ValidJWT(t *testing.T) {
	secret, err := generateHMACSecret()
	require.NoError(t, err)

	tok, err := mintToken(secret, "my-client")
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (interface{}, error) {
		return secret, nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithAudience(jwtAudience),
		jwt.WithIssuer(jwtIssuer),
	)
	require.NoError(t, err)
	assert.True(t, parsed.Valid)
}

func TestMintToken_Claims(t *testing.T) {
	secret, err := generateHMACSecret()
	require.NoError(t, err)

	tok, err := mintToken(secret, "my-client")
	require.NoError(t, err)

	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (interface{}, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	require.NoError(t, err)

	sub, err := parsed.Claims.GetSubject()
	require.NoError(t, err)
	assert.Equal(t, "my-client", sub)

	iss, err := parsed.Claims.GetIssuer()
	require.NoError(t, err)
	assert.Equal(t, jwtIssuer, iss)

	aud, err := parsed.Claims.GetAudience()
	require.NoError(t, err)
	assert.Contains(t, aud, jwtAudience)
}

func TestMintToken_NoExpiry(t *testing.T) {
	secret, err := generateHMACSecret()
	require.NoError(t, err)

	tok, err := mintToken(secret, "no-expiry-client")
	require.NoError(t, err)

	// Parse without exp validation — should succeed even without exp claim
	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (interface{}, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	// exp claim is absent, so WithExpirationRequired should make this fail
	assert.Error(t, err, "token should have no exp claim")
	_ = parsed
}

func TestMintToken_WrongSecretFails(t *testing.T) {
	secret, err := generateHMACSecret()
	require.NoError(t, err)

	tok, err := mintToken(secret, "client")
	require.NoError(t, err)

	other, err := generateHMACSecret()
	require.NoError(t, err)

	_, err = jwt.Parse(tok, func(t *jwt.Token) (interface{}, error) {
		return other, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	assert.Error(t, err, "verification with a different secret should fail")
}
