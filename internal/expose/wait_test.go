package expose

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// availableDeployment returns a Deployment that satisfies deploymentAvailable.
func availableDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
}

// lbService returns a Service with a LoadBalancer IP already assigned.
func lbService(name, namespace, ip string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: ip}},
			},
		},
	}
}

// ---- deploymentAvailable ---------------------------------------------------

func TestDeploymentAvailable_True(t *testing.T) {
	dep := availableDeployment("test", "default")
	assert.True(t, deploymentAvailable(dep))
}

func TestDeploymentAvailable_ConditionFalse(t *testing.T) {
	dep := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse},
			},
		},
	}
	assert.False(t, deploymentAvailable(dep))
}

func TestDeploymentAvailable_NoConditions(t *testing.T) {
	assert.False(t, deploymentAvailable(&appsv1.Deployment{}))
}

// ---- waitForDeployment -----------------------------------------------------

func TestWaitForDeployment_AlreadyAvailable(t *testing.T) {
	cfg := testCfg()
	dep := availableDeployment(cfg.ResourceName(), cfg.Namespace)
	kc := fake.NewSimpleClientset(dep)

	err := waitForDeployment(context.Background(), kc, cfg.Namespace, cfg.ResourceName(), 5*time.Second)
	require.NoError(t, err)
}

func TestWaitForDeployment_Timeout(t *testing.T) {
	cfg := testCfg()
	// Deployment exists but never transitions to Available.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.ResourceName(), Namespace: cfg.Namespace},
	}
	kc := fake.NewSimpleClientset(dep)

	err := waitForDeployment(context.Background(), kc, cfg.Namespace, cfg.ResourceName(), 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestWaitForDeployment_ContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForDeployment(ctx, fake.NewSimpleClientset(), "ns", "deploy", 5*time.Second)
	require.Error(t, err)
}

// ---- waitForLoadBalancer ---------------------------------------------------

func TestWaitForLoadBalancer_IPAvailable(t *testing.T) {
	cfg := testCfg()
	kc := fake.NewSimpleClientset(lbService(cfg.ResourceName(), cfg.Namespace, "1.2.3.4"))

	addr, err := waitForLoadBalancer(context.Background(), kc, cfg.Namespace, cfg.ResourceName(), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "1.2.3.4", addr)
}

func TestWaitForLoadBalancer_HostnameAvailable(t *testing.T) {
	cfg := testCfg()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.ResourceName(), Namespace: cfg.Namespace},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: "lb.example.com"}},
			},
		},
	}
	kc := fake.NewSimpleClientset(svc)

	addr, err := waitForLoadBalancer(context.Background(), kc, cfg.Namespace, cfg.ResourceName(), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "lb.example.com", addr)
}

func TestWaitForLoadBalancer_PreferHostnameOverIP(t *testing.T) {
	cfg := testCfg()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.ResourceName(), Namespace: cfg.Namespace},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: "lb.example.com", IP: "1.2.3.4"}},
			},
		},
	}
	kc := fake.NewSimpleClientset(svc)

	addr, err := waitForLoadBalancer(context.Background(), kc, cfg.Namespace, cfg.ResourceName(), 5*time.Second)
	require.NoError(t, err)
	// hostname takes precedence because it's checked first
	assert.Equal(t, "lb.example.com", addr)
}

func TestWaitForLoadBalancer_Timeout(t *testing.T) {
	cfg := testCfg()
	// Service exists but no LB ingress assigned yet.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.ResourceName(), Namespace: cfg.Namespace},
	}
	kc := fake.NewSimpleClientset(svc)

	_, err := waitForLoadBalancer(context.Background(), kc, cfg.Namespace, cfg.ResourceName(), 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestWaitForLoadBalancer_ContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitForLoadBalancer(ctx, fake.NewSimpleClientset(), "ns", "svc", 5*time.Second)
	require.Error(t, err)
}
