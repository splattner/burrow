package server

import (
	"context"

	"github.com/sebastian/k8s-reverse-tunnel/internal/config"
	"github.com/sebastian/k8s-reverse-tunnel/internal/logging"
	serverimpl "github.com/sebastian/k8s-reverse-tunnel/internal/server"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func NewCommand(ctx context.Context, v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the reverse tunnel server",
		Example: `  # Local dev with HS256 JWT verification
  k8s-reverse-tunnel server --jwt-hmac-secret dev-secret --server-addr :8080 --bridge-addr :1111

  # In-cluster style with explicit kube API mode
  k8s-reverse-tunnel server --jwks-url https://issuer.example/.well-known/jwks.json --enable-kube-api true

  # Equivalent with environment variables
  KRT_JWT_HMAC_SECRET=dev-secret KRT_SERVER_ADDR=:8080 KRT_BRIDGE_ADDR=:1111 k8s-reverse-tunnel server`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.LoadFromViper(v)
			if err != nil {
				return err
			}
			if err := config.ValidateServer(cfg); err != nil {
				return err
			}

			logger := logging.New(cfg.LogLevel)
			logger.Info("starting k8s-reverse-tunnel server")

			return Run(ctx, cfg, logger)
		},
	}

	flags := cmd.Flags()
	flags.String("jwt-alg", "RS256", "JWT signing algorithm")
	flags.String("jwt-hmac-secret", "", "JWT HMAC secret (dev/test)")
	flags.String("jwt-public-key-file", "", "Path to JWT public key PEM")
	flags.String("jwks-url", "", "JWKS endpoint URL (kid-based key lookup)")
	flags.String("jwks-refresh", "5m", "JWKS refresh interval")
	flags.String("jwt-issuer", "", "Expected JWT issuer")
	flags.String("jwt-audience", "", "Expected JWT audience")
	flags.String("server-addr", ":8080", "Server listen address")
	flags.String("bridge-addr", "", "TCP bridge listen address for pod-side traffic")
	flags.String("enable-kube-api", "", "Enable Kubernetes Service API reconciliation (true|false, empty=auto)")
	flags.String("namespace", "default", "Namespace for service reconciliation")
	flags.String("sweep-interval", "1m", "Periodic stale-service sweep interval")
	flags.String("stale-service-age", "10m", "Max disconnected age before service cleanup")
	flags.String("heartbeat-interval", "10s", "Heartbeat interval for liveness")
	flags.String("heartbeat-timeout", "30s", "Heartbeat timeout before stale detection")

	for _, key := range []string{
		"jwt-alg",
		"jwt-hmac-secret",
		"jwt-public-key-file",
		"jwks-url",
		"jwks-refresh",
		"jwt-issuer",
		"jwt-audience",
		"server-addr",
		"bridge-addr",
		"enable-kube-api",
		"namespace",
		"sweep-interval",
		"stale-service-age",
		"heartbeat-interval",
		"heartbeat-timeout",
	} {
		if err := v.BindPFlag(key, flags.Lookup(key)); err != nil {
			panic(err)
		}
	}

	return cmd
}

func Run(ctx context.Context, cfg config.Config, logger *logrus.Logger) error {
	svc := serverimpl.New(cfg, logger)
	return svc.Run(ctx)
}
