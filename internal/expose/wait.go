package expose

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const pollInterval = 3 * time.Second

// waitForDeployment blocks until the named Deployment has at least one Available
// replica, or until timeout is reached.
func waitForDeployment(ctx context.Context, kc kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		d, err := kc.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && deploymentAvailable(d) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Deployment %s/%s to become available", namespace, name)
		case <-time.After(pollInterval):
		}
	}
}

func deploymentAvailable(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// waitForLoadBalancer blocks until the named Service has a LoadBalancer IP or
// hostname assigned, and returns the address.
func waitForLoadBalancer(ctx context.Context, kc kubernetes.Interface, namespace, name string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		svc, err := kc.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, ing := range svc.Status.LoadBalancer.Ingress {
				if ing.Hostname != "" {
					return ing.Hostname, nil
				}
				if ing.IP != "" {
					return ing.IP, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for Service %s/%s to receive a LoadBalancer address", namespace, name)
		case <-time.After(pollInterval):
		}
	}
}
