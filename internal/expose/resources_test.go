package expose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func testCfg() *Config {
	return &Config{
		ClientID:    "test-client",
		ServerName:  "test-client",
		LocalTarget: "127.0.0.1:5432",
		Namespace:   "test-ns",
		Image:       "ghcr.io/splattner/burrow:test",
		ServerPort:  8080,
	}
}

// ---- ResourceName / AuthSecretName -----------------------------------------

func TestResourceNames(t *testing.T) {
	cfg := &Config{ServerName: "my-client"}
	assert.Equal(t, "burrow-my-client", cfg.ResourceName())
	assert.Equal(t, "burrow-my-client-auth", cfg.AuthSecretName())
}

func TestIngressMode(t *testing.T) {
	cfg := testCfg()
	assert.False(t, cfg.IngressMode())
	cfg.Hostname = "tunnel.example.com"
	assert.True(t, cfg.IngressMode())
}

// ---- buildDeployment -------------------------------------------------------

func TestBuildDeployment_BasicShape(t *testing.T) {
	cfg := testCfg()
	dep, err := cfg.buildDeployment()
	require.NoError(t, err)

	assert.Equal(t, cfg.ResourceName(), dep.Name)
	assert.Equal(t, cfg.Namespace, dep.Namespace)
	assert.Equal(t, managedByLabel, dep.Labels["app.kubernetes.io/managed-by"])
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "server", dep.Spec.Template.Spec.Containers[0].Name)
	assert.Equal(t, cfg.Image, dep.Spec.Template.Spec.Containers[0].Image)
}

func TestBuildDeployment_EnvVars(t *testing.T) {
	cfg := testCfg()
	dep, err := cfg.buildDeployment()
	require.NoError(t, err)

	envMap := make(map[string]string)
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	assert.Equal(t, "HS256", envMap["BURROW_JWT_ALG"])
	assert.Equal(t, jwtAudience, envMap["BURROW_JWT_AUDIENCE"])
	assert.Equal(t, jwtIssuer, envMap["BURROW_JWT_ISSUER"])
	assert.Equal(t, ":8080", envMap["BURROW_SERVER_ADDR"])
	assert.Equal(t, "0.0.0.0", envMap["BURROW_BRIDGE_HOST"])
	assert.Equal(t, "true", envMap["BURROW_ENABLE_KUBE_API"])
}

func TestBuildDeployment_HMACSecretRef(t *testing.T) {
	cfg := testCfg()
	dep, err := cfg.buildDeployment()
	require.NoError(t, err)

	container := dep.Spec.Template.Spec.Containers[0]
	var hmacEnv *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == "BURROW_JWT_HMAC_SECRET" {
			hmacEnv = &container.Env[i]
			break
		}
	}
	require.NotNil(t, hmacEnv, "BURROW_JWT_HMAC_SECRET env var not found")
	require.NotNil(t, hmacEnv.ValueFrom)
	require.NotNil(t, hmacEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, cfg.AuthSecretName(), hmacEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, authSecretDataKey, hmacEnv.ValueFrom.SecretKeyRef.Key)
}

func TestBuildDeployment_Probes(t *testing.T) {
	cfg := testCfg()
	dep, err := cfg.buildDeployment()
	require.NoError(t, err)

	container := dep.Spec.Template.Spec.Containers[0]
	require.NotNil(t, container.ReadinessProbe)
	require.NotNil(t, container.LivenessProbe)
	assert.Equal(t, "/healthz", container.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, "/healthz", container.LivenessProbe.HTTPGet.Path)
}

func TestBuildDeployment_Patch(t *testing.T) {
	cfg := testCfg()
	cfg.PatchDeployment = `{"spec":{"template":{"spec":{"containers":[{"name":"server","resources":{"requests":{"cpu":"50m","memory":"64Mi"}}}]}}}}`

	dep, err := cfg.buildDeployment()
	require.NoError(t, err)

	container := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "50m", container.Resources.Requests.Cpu().String())
	assert.Equal(t, "64Mi", container.Resources.Requests.Memory().String())
}

func TestBuildDeployment_PatchInvalidJSON(t *testing.T) {
	cfg := testCfg()
	cfg.PatchDeployment = `{invalid json}`
	_, err := cfg.buildDeployment()
	assert.Error(t, err)
}

// ---- buildService ----------------------------------------------------------

func TestBuildService_LoadBalancerWithoutHostname(t *testing.T) {
	cfg := testCfg()
	svc, err := cfg.buildService()
	require.NoError(t, err)
	assert.Equal(t, corev1.ServiceTypeLoadBalancer, svc.Spec.Type)
	assert.Equal(t, int32(cfg.ServerPort), svc.Spec.Ports[0].Port)
}

func TestBuildService_ClusterIPInIngressMode(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	svc, err := cfg.buildService()
	require.NoError(t, err)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
}

func TestBuildService_ExplicitServiceTypeOverridesAuto(t *testing.T) {
	// NodePort explicitly, even with a hostname (Ingress mode)
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	cfg.ServiceType = "NodePort"
	svc, err := cfg.buildService()
	require.NoError(t, err)
	assert.Equal(t, corev1.ServiceTypeNodePort, svc.Spec.Type)
}

func TestBuildService_ExplicitLoadBalancerInIngressMode(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	cfg.ServiceType = "LoadBalancer"
	svc, err := cfg.buildService()
	require.NoError(t, err)
	assert.Equal(t, corev1.ServiceTypeLoadBalancer, svc.Spec.Type)
}

func TestBuildService_ExplicitClusterIPWithoutHostname(t *testing.T) {
	cfg := testCfg()
	cfg.ServiceType = "ClusterIP"
	svc, err := cfg.buildService()
	require.NoError(t, err)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
}

func TestBuildService_SelectorMatchesDeployment(t *testing.T) {
	cfg := testCfg()
	svc, err := cfg.buildService()
	require.NoError(t, err)
	assert.Equal(t, cfg.ResourceName(), svc.Spec.Selector["app.kubernetes.io/name"])
}

// ---- buildIngress ----------------------------------------------------------

func TestBuildIngress_NilWithoutHostname(t *testing.T) {
	cfg := testCfg()
	ing, err := cfg.buildIngress()
	require.NoError(t, err)
	assert.Nil(t, ing)
}

func TestBuildIngress_TLSAlwaysEnabled(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	ing, err := cfg.buildIngress()
	require.NoError(t, err)
	require.NotNil(t, ing)
	require.Len(t, ing.Spec.TLS, 1)
	assert.Equal(t, []string{"tunnel.example.com"}, ing.Spec.TLS[0].Hosts)
	assert.Empty(t, ing.Spec.TLS[0].SecretName) // no secret = controller default
}

func TestBuildIngress_TLSWithExplicitSecret(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	cfg.TLSSecret = "my-tls-secret"
	ing, err := cfg.buildIngress()
	require.NoError(t, err)
	assert.Equal(t, "my-tls-secret", ing.Spec.TLS[0].SecretName)
}

func TestBuildIngress_DefaultWebSocketAnnotations(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	ing, err := cfg.buildIngress()
	require.NoError(t, err)
	assert.Equal(t, "3600", ing.Annotations["nginx.ingress.kubernetes.io/proxy-read-timeout"])
	assert.Equal(t, "3600", ing.Annotations["nginx.ingress.kubernetes.io/proxy-send-timeout"])
}

func TestBuildIngress_UserAnnotationsOverrideDefaults(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	cfg.IngressAnnotations = map[string]string{
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "7200",
		"custom.example.com/foo":                         "bar",
	}
	ing, err := cfg.buildIngress()
	require.NoError(t, err)
	assert.Equal(t, "7200", ing.Annotations["nginx.ingress.kubernetes.io/proxy-read-timeout"])
	assert.Equal(t, "bar", ing.Annotations["custom.example.com/foo"])
}

func TestBuildIngress_IngressClassSet(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	cfg.IngressClass = "nginx"
	ing, err := cfg.buildIngress()
	require.NoError(t, err)
	require.NotNil(t, ing.Spec.IngressClassName)
	assert.Equal(t, "nginx", *ing.Spec.IngressClassName)
}

func TestBuildIngress_NoIngressClassWhenEmpty(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	ing, err := cfg.buildIngress()
	require.NoError(t, err)
	assert.Nil(t, ing.Spec.IngressClassName)
}

// ---- buildDryRunAuthSecret -------------------------------------------------

func TestBuildDryRunAuthSecret(t *testing.T) {
	cfg := testCfg()
	s := cfg.buildDryRunAuthSecret()
	assert.Equal(t, cfg.AuthSecretName(), s.Name)
	assert.Equal(t, cfg.Namespace, s.Namespace)
	val, ok := s.StringData[authSecretDataKey]
	require.True(t, ok)
	assert.Contains(t, val, "auto-generated")
}

// ---- commonLabels ----------------------------------------------------------

func TestCommonLabels(t *testing.T) {
	cfg := testCfg()
	labels := cfg.commonLabels()
	assert.Equal(t, cfg.ResourceName(), labels["app.kubernetes.io/name"])
	assert.Equal(t, managedByLabel, labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, cfg.ServerName, labels["burrow.dev/server-name"])
}
