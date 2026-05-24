package expose

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/splattner/burrow/internal/logging"
)

// captureStdout redirects os.Stdout, calls fn, and returns everything written.
// Tests using this must not run in parallel.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w

	fnErr := fn()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String(), fnErr
}

// ---- storeAuthSecret / loadAuthSecret --------------------------------------

func TestStoreAuthSecret_CreatesSecretWithCorrectData(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctx := context.Background()
	labels := map[string]string{"app.kubernetes.io/managed-by": managedByLabel}

	err := storeAuthSecret(ctx, kc, "test-ns", "my-secret", []byte("secret-data"), labels)
	require.NoError(t, err)

	s, err := kc.CoreV1().Secrets("test-ns").Get(ctx, "my-secret", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("secret-data"), s.Data[authSecretDataKey])
	assert.Equal(t, labels, s.Labels)
}

func TestLoadAuthSecret_Success(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctx := context.Background()

	err := storeAuthSecret(ctx, kc, "test-ns", "my-secret", []byte("secret-data"), nil)
	require.NoError(t, err)

	data, err := loadAuthSecret(ctx, kc, "test-ns", "my-secret")
	require.NoError(t, err)
	assert.Equal(t, []byte("secret-data"), data)
}

func TestLoadAuthSecret_MissingSecret(t *testing.T) {
	_, err := loadAuthSecret(context.Background(), fake.NewSimpleClientset(), "test-ns", "nonexistent")
	require.Error(t, err)
}

func TestLoadAuthSecret_MissingKey(t *testing.T) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "test-ns"},
		Data:       map[string][]byte{"wrong-key": []byte("data")},
	}
	_, err := loadAuthSecret(context.Background(), fake.NewSimpleClientset(s), "test-ns", "my-secret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), authSecretDataKey)
}

// ---- resolveServerURL ------------------------------------------------------

func TestResolveServerURL_IngressMode(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	e := New(cfg, logging.NoOp())

	url, err := e.resolveServerURL(context.Background(), fake.NewSimpleClientset())
	require.NoError(t, err)
	assert.Equal(t, "wss://tunnel.example.com/ws", url)
}

func TestResolveServerURL_LoadBalancerMode(t *testing.T) {
	cfg := testCfg()
	cfg.ServerPort = 8080
	cfg.WaitTimeout = 5 * time.Second
	kc := fake.NewSimpleClientset(lbService(cfg.ResourceName(), cfg.Namespace, "1.2.3.4"))
	e := New(cfg, logging.NoOp())

	url, err := e.resolveServerURL(context.Background(), kc)
	require.NoError(t, err)
	assert.Equal(t, "ws://1.2.3.4:8080/ws", url)
}

func TestResolveServerURL_LoadBalancerTimeout(t *testing.T) {
	cfg := testCfg()
	cfg.WaitTimeout = 10 * time.Millisecond
	// Service exists but no LB address assigned.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.ResourceName(), Namespace: cfg.Namespace},
	}
	e := New(cfg, logging.NoOp())

	_, err := e.resolveServerURL(context.Background(), fake.NewSimpleClientset(svc))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

// ---- dryRun ----------------------------------------------------------------

func TestDryRun_ReturnsNilAndOutputsResourceName(t *testing.T) {
	cfg := testCfg()
	e := New(cfg, logging.NoOp())

	out, err := captureStdout(t, e.dryRun)
	require.NoError(t, err)
	assert.Contains(t, out, cfg.ResourceName())
}

func TestDryRun_DefaultsNamespaceToDefault(t *testing.T) {
	cfg := testCfg()
	cfg.Namespace = ""
	e := New(cfg, logging.NoOp())

	_, err := captureStdout(t, e.dryRun)
	require.NoError(t, err)
	assert.Equal(t, "default", cfg.Namespace)
}

func TestDryRun_IncludesIngressWhenHostnameSet(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	e := New(cfg, logging.NoOp())

	out, err := captureStdout(t, e.dryRun)
	require.NoError(t, err)
	assert.Contains(t, out, "tunnel.example.com")
}

func TestDryRun_ErrorOnInvalidDeploymentPatch(t *testing.T) {
	cfg := testCfg()
	cfg.PatchDeployment = `not-valid-json`
	e := New(cfg, logging.NoOp())

	_, err := captureStdout(t, e.dryRun)
	require.Error(t, err)
}

func TestDryRun_ErrorOnInvalidIngressPatch(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	cfg.PatchIngress = `not-valid-json`
	e := New(cfg, logging.NoOp())

	_, err := captureStdout(t, e.dryRun)
	require.Error(t, err)
}
