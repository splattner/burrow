package expose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	clientimpl "github.com/splattner/burrow/internal/client"
	"github.com/splattner/burrow/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Exposer orchestrates deploying the burrow server to Kubernetes and running
// the burrow client locally to form a complete reverse tunnel.
type Exposer struct {
	cfg    *Config
	logger *logrus.Logger
}

// New creates a new Exposer.
func New(cfg *Config, logger *logrus.Logger) *Exposer {
	return &Exposer{cfg: cfg, logger: logger}
}

// Run orchestrates the full expose lifecycle.
func (e *Exposer) Run(ctx context.Context) error {
	if e.cfg.DryRun {
		return e.dryRun()
	}

	kc, err := e.kubeClient()
	if err != nil {
		return fmt.Errorf("building Kubernetes client: %w", err)
	}

	var serverURL string
	d := newDeployer(e.cfg, kc)

	if e.cfg.Reuse {
		e.logger.Info("--reuse set: connecting to existing server deployment")
		serverURL, err = e.handleReuse(ctx, kc, d)
		if err != nil {
			return err
		}
	} else {
		serverURL, err = e.handleFreshDeploy(ctx, kc, d)
		if err != nil {
			// Best-effort cleanup of any partially created resources.
			e.logger.Warnf("deployment failed; cleaning up partial resources")
			if delErr := d.Delete(context.Background()); delErr != nil {
				e.logger.Warnf("cleanup after failed deploy: %v", delErr)
			}
			return err
		}
	}

	if !e.cfg.Keep {
		defer func() {
			e.logger.Info("cleaning up server resources")
			if delErr := d.Delete(context.Background()); delErr != nil {
				e.logger.Warnf("cleanup: %v", delErr)
			}
		}()
	}

	secret, err := loadAuthSecret(ctx, kc, e.cfg.Namespace, e.cfg.AuthSecretName())
	if err != nil {
		return fmt.Errorf("loading auth secret: %w", err)
	}
	token, err := mintToken(secret, e.cfg.ClientID)
	if err != nil {
		return fmt.Errorf("minting token: %w", err)
	}

	e.logger.Infof("connecting to %s", serverURL)
	// Start from LoadFromViper defaults (empty viper ⇒ all defaults apply),
	// then override the fields specific to this expose session.
	clientCfg, err := config.LoadFromViper(viper.New())
	if err != nil {
		return fmt.Errorf("building client config: %w", err)
	}
	clientCfg.BearerToken = token
	clientCfg.ServerURL = serverURL
	clientCfg.ClientID = e.cfg.ClientID
	clientCfg.LocalTarget = e.cfg.LocalTarget
	clientCfg.ConnectAddr = e.cfg.ConnectAddr
	return clientimpl.New(clientCfg, e.logger).Run(ctx)
}

func (e *Exposer) handleFreshDeploy(ctx context.Context, _ kubernetes.Interface, d *deployer) (string, error) {
	secret, err := generateHMACSecret()
	if err != nil {
		return "", err
	}

	e.logger.Infof("deploying server resources in namespace %q", e.cfg.Namespace)
	if err := d.Deploy(ctx, secret); err != nil {
		return "", fmt.Errorf("deploying server: %w", err)
	}

	e.logger.Info("waiting for server deployment to become available")
	if err := waitForDeployment(ctx, d.kc, e.cfg.Namespace, e.cfg.ResourceName(), e.cfg.WaitTimeout); err != nil {
		return "", err
	}

	return e.resolveServerURL(ctx, d.kc)
}

func (e *Exposer) handleReuse(ctx context.Context, kc kubernetes.Interface, _ *deployer) (string, error) {
	// Verify the Deployment exists and is burrow-managed.
	dep, err := kc.AppsV1().Deployments(e.cfg.Namespace).Get(ctx, e.cfg.ResourceName(), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting Deployment %q: %w", e.cfg.ResourceName(), err)
	}
	if dep.Labels["app.kubernetes.io/managed-by"] != managedByLabel {
		return "", fmt.Errorf(
			"deployment %q in namespace %q is not managed by burrow; cannot --reuse it",
			e.cfg.ResourceName(), e.cfg.Namespace,
		)
	}

	// Verify the auth Secret exists before spending time waiting for the Deployment.
	if _, err := kc.CoreV1().Secrets(e.cfg.Namespace).Get(ctx, e.cfg.AuthSecretName(), metav1.GetOptions{}); err != nil {
		return "", fmt.Errorf("auth Secret %q not found in namespace %q (was the deployment created by burrow expose?): %w",
			e.cfg.AuthSecretName(), e.cfg.Namespace, err)
	}

	e.logger.Info("waiting for existing deployment to become available")
	if err := waitForDeployment(ctx, kc, e.cfg.Namespace, e.cfg.ResourceName(), e.cfg.WaitTimeout); err != nil {
		return "", err
	}
	return e.resolveServerURL(ctx, kc)
}

func (e *Exposer) resolveServerURL(ctx context.Context, kc kubernetes.Interface) (string, error) {
	if e.cfg.IngressMode() {
		return "wss://" + e.cfg.Hostname + "/ws", nil
	}
	addr, err := waitForLoadBalancer(ctx, kc, e.cfg.Namespace, e.cfg.ResourceName(), e.cfg.WaitTimeout)
	if err != nil {
		return "", err
	}
	return "ws://" + addr + ":" + strconv.Itoa(e.cfg.ServerPort) + "/ws", nil
}

func (e *Exposer) kubeClient() (kubernetes.Interface, error) {
	kc, ns, err := buildKubeClient(e.cfg.KubeContext, e.cfg.Namespace)
	if err != nil {
		return nil, err
	}
	e.cfg.Namespace = ns
	return kc, nil
}

// buildKubeClient builds a Kubernetes client from the default kubeconfig,
// optionally overriding the context. It also resolves the namespace: if
// namespace is empty the context's namespace is used and the resolved value is
// returned as the second return value.
func buildKubeClient(kubeContext, namespace string) (kubernetes.Interface, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	if namespace == "" {
		ns, _, err := clientConfig.Namespace()
		if err != nil {
			return nil, "", fmt.Errorf("resolving namespace from kubeconfig: %w", err)
		}
		namespace = ns
	}

	restCfg, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("building REST config: %w", err)
	}
	kc, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, "", fmt.Errorf("building Kubernetes client: %w", err)
	}
	return kc, namespace, nil
}

// DeleteSession deletes all Kubernetes resources created by a previous
// burrow expose session. serverName identifies the Kubernetes resources
// (was the --server-name or --client-id used at deploy time). clientID is
// optional: when non-empty, reconciler-created client Services labelled with
// that client ID are also removed.
func DeleteSession(ctx context.Context, serverName, clientID, namespace, kubeContext string, logger *logrus.Logger) error {
	kc, resolvedNS, err := buildKubeClient(kubeContext, namespace)
	if err != nil {
		return fmt.Errorf("building Kubernetes client: %w", err)
	}
	cfg := &Config{ServerName: serverName, ClientID: clientID, Namespace: resolvedNS}
	logger.Infof("deleting expose resources for server %q in namespace %q", serverName, resolvedNS)
	return newDeployer(cfg, kc).Delete(ctx)
}

// dryRun prints the Kubernetes resources that would be created.
func (e *Exposer) dryRun() error {
	if e.cfg.Namespace == "" {
		e.cfg.Namespace = "default"
	}

	// The auth Secret is auto-generated at deploy time; show it with a
	// placeholder so the dry-run output is complete and the Deployment's
	// secretKeyRef can be traced back to a concrete resource.
	authSecret := e.cfg.buildDryRunAuthSecret()

	resources := []interface{}{
		authSecret,
		e.cfg.buildServiceAccount(),
		e.cfg.buildRole(),
		e.cfg.buildRoleBinding(),
	}

	dep, err := e.cfg.buildDeployment()
	if err != nil {
		return fmt.Errorf("building Deployment for dry-run: %w", err)
	}
	resources = append(resources, dep)

	svc, err := e.cfg.buildService()
	if err != nil {
		return fmt.Errorf("building Service for dry-run: %w", err)
	}
	resources = append(resources, svc)

	if e.cfg.IngressMode() {
		ing, err := e.cfg.buildIngress()
		if err != nil {
			return fmt.Errorf("building Ingress for dry-run: %w", err)
		}
		if ing != nil {
			resources = append(resources, ing)
		}
	}

	for i, r := range resources {
		if i > 0 {
			fmt.Println("---")
		}
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling resource: %w", err)
		}
		if _, err := os.Stdout.Write(b); err != nil {
			return fmt.Errorf("writing resource: %w", err)
		}
		fmt.Println()
	}
	return nil
}
