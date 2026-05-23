package expose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// generateHMACSecret creates a cryptographically random 32-byte hex-encoded secret.
func generateHMACSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generating HMAC secret: %w", err)
	}
	return []byte(hex.EncodeToString(b)), nil
}

// mintToken signs a JWT for the given clientID using the provided HMAC secret.
// Tokens have no expiry — they are tied to the ephemeral K8s Secret and are
// invalidated when that Secret is deleted.
func mintToken(secret []byte, clientID string) (string, error) {
	claims := jwt.MapClaims{
		"sub": clientID,
		"iss": jwtIssuer,
		"aud": jwt.ClaimStrings{jwtAudience},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}
	return tok, nil
}

// storeAuthSecret creates a Kubernetes Secret holding the HMAC secret.
func storeAuthSecret(ctx context.Context, kc kubernetes.Interface, namespace, name string, secret []byte, labels map[string]string) error {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Data: map[string][]byte{
			authSecretDataKey: secret,
		},
	}
	_, err := kc.CoreV1().Secrets(namespace).Create(ctx, s, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating auth Secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// loadAuthSecret retrieves the HMAC secret from an existing Kubernetes Secret.
func loadAuthSecret(ctx context.Context, kc kubernetes.Interface, namespace, name string) ([]byte, error) {
	s, err := kc.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting auth Secret %s/%s: %w", namespace, name, err)
	}
	val, ok := s.Data[authSecretDataKey]
	if !ok {
		return nil, fmt.Errorf("auth Secret %s/%s has no %q key", namespace, name, authSecretDataKey)
	}
	return val, nil
}
