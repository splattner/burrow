package expose

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// ---- checkCollision --------------------------------------------------------

func TestCheckCollision_NoExistingDeployment(t *testing.T) {
	d := newDeployer(testCfg(), fake.NewSimpleClientset())
	err := d.checkCollision(context.Background())
	assert.NoError(t, err)
}

func TestCheckCollision_BurrowManagedDeployment(t *testing.T) {
	cfg := testCfg()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.ResourceName(),
			Namespace: cfg.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": managedByLabel},
		},
	}
	d := newDeployer(cfg, fake.NewSimpleClientset(dep))
	err := d.checkCollision(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--reuse")
}

func TestCheckCollision_ExternallyManagedDeployment(t *testing.T) {
	cfg := testCfg()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.ResourceName(),
			Namespace: cfg.Namespace,
			Labels:    map[string]string{}, // no managed-by
		},
	}
	d := newDeployer(cfg, fake.NewSimpleClientset(dep))
	err := d.checkCollision(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--reuse")
	assert.Contains(t, err.Error(), "not managed by burrow")
}

// ---- resolveIngressClass ---------------------------------------------------

func TestResolveIngressClass_AlreadySet(t *testing.T) {
	cfg := testCfg()
	cfg.IngressClass = "nginx"
	d := newDeployer(cfg, fake.NewSimpleClientset())
	d.resolveIngressClass(context.Background())
	assert.Equal(t, "nginx", cfg.IngressClass)
}

func TestResolveIngressClass_FindsDefault(t *testing.T) {
	cfg := testCfg()
	ic := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nginx",
			Annotations: map[string]string{
				"ingressclass.kubernetes.io/is-default-class": "true",
			},
		},
	}
	d := newDeployer(cfg, fake.NewSimpleClientset(ic))
	d.resolveIngressClass(context.Background())
	assert.Equal(t, "nginx", cfg.IngressClass)
}

func TestResolveIngressClass_NoDefault(t *testing.T) {
	cfg := testCfg()
	ic := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nginx",
			Annotations: map[string]string{}, // not marked as default
		},
	}
	d := newDeployer(cfg, fake.NewSimpleClientset(ic))
	d.resolveIngressClass(context.Background())
	assert.Equal(t, "", cfg.IngressClass)
}

func TestResolveIngressClass_NoClasses(t *testing.T) {
	cfg := testCfg()
	d := newDeployer(cfg, fake.NewSimpleClientset())
	d.resolveIngressClass(context.Background())
	assert.Equal(t, "", cfg.IngressClass)
}

// ---- Deploy ----------------------------------------------------------------

func TestDeploy_CreatesAllResourcesWithoutIngress(t *testing.T) {
	cfg := testCfg()
	kc := fake.NewSimpleClientset()
	d := newDeployer(cfg, kc)
	ctx := context.Background()

	err := d.Deploy(ctx, []byte("test-secret"))
	require.NoError(t, err)

	// auth Secret
	s, err := kc.CoreV1().Secrets(cfg.Namespace).Get(ctx, cfg.AuthSecretName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("test-secret"), s.Data[authSecretDataKey])

	// ServiceAccount
	_, err = kc.CoreV1().ServiceAccounts(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	require.NoError(t, err)

	// Role
	_, err = kc.RbacV1().Roles(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	require.NoError(t, err)

	// RoleBinding
	_, err = kc.RbacV1().RoleBindings(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	require.NoError(t, err)

	// Deployment
	_, err = kc.AppsV1().Deployments(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	require.NoError(t, err)

	// Service
	_, err = kc.CoreV1().Services(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	require.NoError(t, err)

	// Ingress should NOT exist (no hostname)
	_, err = kc.NetworkingV1().Ingresses(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	assert.Error(t, err)
}

func TestDeploy_CreatesIngressWhenHostnameSet(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	kc := fake.NewSimpleClientset()
	d := newDeployer(cfg, kc)
	ctx := context.Background()

	err := d.Deploy(ctx, []byte("test-secret"))
	require.NoError(t, err)

	_, err = kc.NetworkingV1().Ingresses(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	require.NoError(t, err)
}

func TestDeploy_StopsOnCollision(t *testing.T) {
	cfg := testCfg()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.ResourceName(),
			Namespace: cfg.Namespace,
			Labels:    map[string]string{},
		},
	}
	d := newDeployer(cfg, fake.NewSimpleClientset(dep))
	err := d.Deploy(context.Background(), []byte("secret"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not managed by burrow")
}

func TestDeploy_RejectsInvalidPatch(t *testing.T) {
	cfg := testCfg()
	cfg.PatchDeployment = `not-valid-json`
	d := newDeployer(cfg, fake.NewSimpleClientset())
	err := d.Deploy(context.Background(), []byte("secret"))
	require.Error(t, err)
}

// ---- Delete ----------------------------------------------------------------

func TestDelete_RemovesAllResources(t *testing.T) {
	cfg := testCfg()
	cfg.Hostname = "tunnel.example.com"
	kc := fake.NewSimpleClientset()
	d := newDeployer(cfg, kc)
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, []byte("test-secret")))
	require.NoError(t, d.Delete(ctx))

	_, err := kc.CoreV1().Secrets(cfg.Namespace).Get(ctx, cfg.AuthSecretName(), metav1.GetOptions{})
	assert.Error(t, err, "auth Secret should be deleted")

	_, err = kc.CoreV1().ServiceAccounts(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	assert.Error(t, err, "ServiceAccount should be deleted")

	_, err = kc.RbacV1().Roles(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	assert.Error(t, err, "Role should be deleted")

	_, err = kc.RbacV1().RoleBindings(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	assert.Error(t, err, "RoleBinding should be deleted")

	_, err = kc.AppsV1().Deployments(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	assert.Error(t, err, "Deployment should be deleted")

	_, err = kc.CoreV1().Services(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	assert.Error(t, err, "Service should be deleted")

	_, err = kc.NetworkingV1().Ingresses(cfg.Namespace).Get(ctx, cfg.ResourceName(), metav1.GetOptions{})
	assert.Error(t, err, "Ingress should be deleted")
}

func TestDelete_IgnoresNotFound(t *testing.T) {
	cfg := testCfg()
	d := newDeployer(cfg, fake.NewSimpleClientset())
	// Deleting on an empty cluster should not return an error.
	err := d.Delete(context.Background())
	require.NoError(t, err)
}

func TestDelete_CleansUpReconcilerClientServices(t *testing.T) {
	cfg := testCfg()
	kc := fake.NewSimpleClientset()
	ctx := context.Background()

	// Simulate a Service the server's kube reconciler created at runtime.
	reconcilerSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burrow-client-svc",
			Namespace: cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": managedByLabel,
				"burrow.dev/client-id":         cfg.ClientID,
			},
		},
	}
	_, err := kc.CoreV1().Services(cfg.Namespace).Create(ctx, reconcilerSvc, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, newDeployer(cfg, kc).Delete(ctx))

	_, err = kc.CoreV1().Services(cfg.Namespace).Get(ctx, reconcilerSvc.Name, metav1.GetOptions{})
	assert.Error(t, err, "reconciler Service should be deleted")
}
