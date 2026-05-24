package expose

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	exposepkg "github.com/splattner/burrow/internal/expose"
	"github.com/splattner/burrow/internal/logging"
	"github.com/splattner/burrow/internal/version"
)

func NewCommand(ctx context.Context, v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "expose",
		Short: "Deploy a burrow server to Kubernetes and tunnel a local service through it",
		Example: `  # Expose a local PostgreSQL through an Ingress
  burrow expose --client-id pg --local-target 127.0.0.1:5432 --hostname tunnel.example.com

  # Expose via LoadBalancer (no Ingress)
  burrow expose --client-id api --local-target 127.0.0.1:8080

  # Dry-run: print resources without deploying
  burrow expose --client-id api --local-target 127.0.0.1:8080 --dry-run

  # Deploy a named server, then reuse it for a second client
  burrow expose --server-name prod-tunnel --client-id pg --local-target 127.0.0.1:5432
  burrow expose --server-name prod-tunnel --client-id redis --local-target 127.0.0.1:6379 --reuse

  # Re-use an existing server deployment (single client)
  burrow expose --client-id api --local-target 127.0.0.1:8080 --reuse

  # Set resource requests/limits on the server container via a strategic merge patch
  burrow expose --client-id api --local-target 127.0.0.1:8080 \
    --patch-deployment '{"spec":{"template":{"spec":{"containers":[{"name":"server","resources":{"requests":{"cpu":"50m","memory":"64Mi"},"limits":{"cpu":"200m","memory":"128Mi"}}}]}}}}'

  # Apply a non-root securityContext to the server container
  burrow expose --client-id api --local-target 127.0.0.1:8080 \
    --patch-deployment '{"spec":{"template":{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65534},"containers":[{"name":"server","securityContext":{"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}}}]}}}}'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logLevel := v.GetString("log-level")
			logger := logging.New(logLevel)

			cfg, err := buildConfig(cmd)
			if err != nil {
				return err
			}

			return exposepkg.New(cfg, logger).Run(ctx)
		},
	}

	defaultImage := fmt.Sprintf("ghcr.io/splattner/burrow:%s", version.Version)

	flags := cmd.Flags()
	flags.String("client-id", "", "Unique client ID (required)")
	flags.String("server-name", "", "Kubernetes resource name for the server deployment (defaults to --client-id; use to share one server across multiple clients)")
	flags.String("local-target", "", "Local host:port to forward tunnel traffic to (required)")
	flags.String("kube-context", "", "Kubernetes context to use (default: current context)")
	flags.String("namespace", "", "Kubernetes namespace (default: context namespace)")
	flags.String("hostname", "", "Hostname for Ingress mode; omit to use LoadBalancer")
	flags.String("tls-secret", "", "TLS Secret name for Ingress (optional; uses controller default if unset)")
	flags.String("ingress-class", "", "IngressClass name (default: auto-detect cluster default)")
	flags.StringArray("ingress-annotation", nil, "Extra Ingress annotation in key=value format (repeatable)")
	flags.String("image", defaultImage, "Container image for the burrow server")
	flags.Int("server-port", 8080, "Port the server listens on inside the container")
	flags.Bool("reuse", false, "Connect to an existing burrow server deployment instead of creating one")
	flags.Bool("keep", false, "Leave server resources in Kubernetes after the tunnel closes")
	flags.Duration("wait-timeout", 2*time.Minute, "Maximum time to wait for the server to become available")
	flags.Bool("dry-run", false, "Print Kubernetes resources without deploying")
	flags.String("patch-deployment", "", "JSON strategic merge patch applied to the Deployment")
	flags.String("patch-service", "", "JSON strategic merge patch applied to the Service")
	flags.String("patch-ingress", "", "JSON strategic merge patch applied to the Ingress")

	_ = cmd.MarkFlagRequired("client-id")
	// --local-target is required for actual tunnelling but not for --dry-run;
	// validation is done in buildConfig instead of via MarkFlagRequired.

	cmd.AddCommand(newDeleteCommand(ctx, v))

	return cmd
}

func newDeleteCommand(ctx context.Context, v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete Kubernetes resources created by burrow expose",
		Example: `  # Delete by client ID (backward compatible)
  burrow expose delete --client-id pg

  # Delete by server name (shared server deployment)
  burrow expose delete --server-name prod-tunnel

  # Delete in a specific namespace and context
  burrow expose delete --server-name prod-tunnel --namespace staging --kube-context prod`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := cmd.Flags()
			clientID, _ := flags.GetString("client-id")
			serverName, _ := flags.GetString("server-name")
			namespace, _ := flags.GetString("namespace")
			kubeContext, _ := flags.GetString("kube-context")
			if serverName == "" {
				serverName = clientID
			}
			if serverName == "" {
				return fmt.Errorf("either --server-name or --client-id is required")
			}
			logger := logging.New(v.GetString("log-level"))
			return exposepkg.DeleteSession(ctx, serverName, clientID, namespace, kubeContext, logger)
		},
	}

	flags := cmd.Flags()
	flags.String("server-name", "", "Server name to delete (use the --server-name given at deploy time)")
	flags.String("client-id", "", "Client ID used at deploy time; fallback if --server-name is not set")
	flags.String("namespace", "", "Kubernetes namespace (default: context namespace)")
	flags.String("kube-context", "", "Kubernetes context to use (default: current context)")
	return cmd
}

func buildConfig(cmd *cobra.Command) (*exposepkg.Config, error) {
	flags := cmd.Flags()

	clientID, _ := flags.GetString("client-id")
	serverName, _ := flags.GetString("server-name")
	if serverName == "" {
		serverName = clientID
	}
	localTarget, _ := flags.GetString("local-target")
	kubeContext, _ := flags.GetString("kube-context")
	namespace, _ := flags.GetString("namespace")
	hostname, _ := flags.GetString("hostname")
	tlsSecret, _ := flags.GetString("tls-secret")
	ingressClass, _ := flags.GetString("ingress-class")
	rawAnnotations, _ := flags.GetStringArray("ingress-annotation")
	image, _ := flags.GetString("image")
	serverPort, _ := flags.GetInt("server-port")
	reuse, _ := flags.GetBool("reuse")
	keep, _ := flags.GetBool("keep")
	waitTimeout, _ := flags.GetDuration("wait-timeout")
	patchDeployment, _ := flags.GetString("patch-deployment")
	patchService, _ := flags.GetString("patch-service")
	patchIngress, _ := flags.GetString("patch-ingress")

	annotations := make(map[string]string, len(rawAnnotations))
	for _, a := range rawAnnotations {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --ingress-annotation %q: expected key=value", a)
		}
		annotations[k] = v
	}

	dryRun, _ := flags.GetBool("dry-run")
	if localTarget == "" && !dryRun {
		return nil, fmt.Errorf("required flag \"local-target\" not set")
	}

	return &exposepkg.Config{
		ClientID:           clientID,
		ServerName:         serverName,
		LocalTarget:        localTarget,
		KubeContext:        kubeContext,
		Namespace:          namespace,
		Hostname:           hostname,
		TLSSecret:          tlsSecret,
		IngressClass:       ingressClass,
		IngressAnnotations: annotations,
		Image:              image,
		ServerPort:         serverPort,
		Reuse:              reuse,
		Keep:               keep,
		WaitTimeout:        waitTimeout,
		DryRun:             dryRun,
		PatchDeployment:    patchDeployment,
		PatchService:       patchService,
		PatchIngress:       patchIngress,
	}, nil
}
