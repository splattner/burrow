package expose

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type deployer struct {
	cfg *Config
	kc  kubernetes.Interface
}

func newDeployer(cfg *Config, kc kubernetes.Interface) *deployer {
	return &deployer{cfg: cfg, kc: kc}
}

// checkCollision returns an error if a Deployment with the same name already
// exists. It distinguishes between burrow-managed (suggest --reuse) and
// externally-managed (hard error) resources.
func (d *deployer) checkCollision(ctx context.Context) error {
	dep, err := d.kc.AppsV1().Deployments(d.cfg.Namespace).Get(ctx, d.cfg.ResourceName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking for existing Deployment: %w", err)
	}
	if dep.Labels["app.kubernetes.io/managed-by"] == managedByLabel {
		return fmt.Errorf(
			"deployment %q already exists in namespace %q (managed by burrow); use --reuse to connect to it",
			d.cfg.ResourceName(), d.cfg.Namespace,
		)
	}
	return fmt.Errorf(
		"deployment %q already exists in namespace %q and is not managed by burrow; delete it manually or choose a different --server-name",
		d.cfg.ResourceName(), d.cfg.Namespace,
	)
}

// resolveIngressClass auto-detects the cluster's default IngressClass and
// sets it on the config. A failure is non-fatal: the ingress controller's
// built-in default takes effect.
func (d *deployer) resolveIngressClass(ctx context.Context) {
	if d.cfg.IngressClass != "" {
		return
	}
	ics, err := d.kc.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, ic := range ics.Items {
		if ic.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true" {
			d.cfg.IngressClass = ic.Name
			return
		}
	}
}

// Deploy creates all Kubernetes resources required for the expose session in
// dependency order: auth Secret → ServiceAccount → Role → RoleBinding →
// Deployment → Service → Ingress (if applicable).
func (d *deployer) Deploy(ctx context.Context, secret []byte) error {
	if err := d.checkCollision(ctx); err != nil {
		return err
	}
	if d.cfg.IngressMode() {
		d.resolveIngressClass(ctx)
	}

	if err := storeAuthSecret(ctx, d.kc, d.cfg.Namespace, d.cfg.AuthSecretName(), secret, d.cfg.commonLabels()); err != nil {
		return err
	}

	sa := d.cfg.buildServiceAccount()
	if _, err := d.kc.CoreV1().ServiceAccounts(d.cfg.Namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating ServiceAccount: %w", err)
	}

	role := d.cfg.buildRole()
	if _, err := d.kc.RbacV1().Roles(d.cfg.Namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating Role: %w", err)
	}

	rb := d.cfg.buildRoleBinding()
	if _, err := d.kc.RbacV1().RoleBindings(d.cfg.Namespace).Create(ctx, rb, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating RoleBinding: %w", err)
	}

	dep, err := d.cfg.buildDeployment()
	if err != nil {
		return fmt.Errorf("building Deployment: %w", err)
	}
	if _, err := d.kc.AppsV1().Deployments(d.cfg.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating Deployment: %w", err)
	}

	svc, err := d.cfg.buildService()
	if err != nil {
		return fmt.Errorf("building Service: %w", err)
	}
	if _, err := d.kc.CoreV1().Services(d.cfg.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating Service: %w", err)
	}

	ing, err := d.cfg.buildIngress()
	if err != nil {
		return fmt.Errorf("building Ingress: %w", err)
	}
	if ing != nil {
		if _, err := d.kc.NetworkingV1().Ingresses(d.cfg.Namespace).Create(ctx, ing, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating Ingress: %w", err)
		}
	}

	return nil
}

// Delete removes all resources created for this expose session. Resources are
// deleted in reverse creation order. Not-found errors are ignored. Also cleans
// up any client Services the running server's reconciler may have created.
func (d *deployer) Delete(ctx context.Context) error {
	name := d.cfg.ResourceName()
	ns := d.cfg.Namespace
	del := metav1.DeleteOptions{}
	var errs []error

	ignore := func(err error) bool { return err == nil || apierrors.IsNotFound(err) }

	// Always attempt Ingress deletion — ignore NotFound so it's safe to call
	// regardless of whether Ingress mode was used when the session was created.
	if err := d.kc.NetworkingV1().Ingresses(ns).Delete(ctx, name, del); !ignore(err) {
		errs = append(errs, fmt.Errorf("deleting Ingress: %w", err))
	}
	if err := d.kc.CoreV1().Services(ns).Delete(ctx, name, del); !ignore(err) {
		errs = append(errs, fmt.Errorf("deleting Service: %w", err))
	}
	if err := d.kc.AppsV1().Deployments(ns).Delete(ctx, name, del); !ignore(err) {
		errs = append(errs, fmt.Errorf("deleting Deployment: %w", err))
	}
	if err := d.kc.RbacV1().RoleBindings(ns).Delete(ctx, name, del); !ignore(err) {
		errs = append(errs, fmt.Errorf("deleting RoleBinding: %w", err))
	}
	if err := d.kc.RbacV1().Roles(ns).Delete(ctx, name, del); !ignore(err) {
		errs = append(errs, fmt.Errorf("deleting Role: %w", err))
	}
	if err := d.kc.CoreV1().ServiceAccounts(ns).Delete(ctx, name, del); !ignore(err) {
		errs = append(errs, fmt.Errorf("deleting ServiceAccount: %w", err))
	}
	if err := d.kc.CoreV1().Secrets(ns).Delete(ctx, d.cfg.AuthSecretName(), del); !ignore(err) {
		errs = append(errs, fmt.Errorf("deleting auth Secret: %w", err))
	}

	// Also remove any client Services the server's reconciler created at runtime.
	// This is skipped if ClientID is unknown (e.g. delete by --server-name only).
	if d.cfg.ClientID != "" {
		svcs, err := d.kc.CoreV1().Services(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=" + managedByLabel + ",burrow.dev/client-id=" + d.cfg.ClientID,
		})
		if err == nil {
			for _, svc := range svcs.Items {
				if err := d.kc.CoreV1().Services(ns).Delete(ctx, svc.Name, del); !ignore(err) {
					errs = append(errs, fmt.Errorf("deleting client Service %q: %w", svc.Name, err))
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}
	return nil
}
