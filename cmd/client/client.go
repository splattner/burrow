package client

import (
	"context"

	clientimpl "github.com/sebastian/k8s-reverse-tunnel/internal/client"
	"github.com/sebastian/k8s-reverse-tunnel/internal/config"
	"github.com/sebastian/k8s-reverse-tunnel/internal/logging"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func NewCommand(ctx context.Context, v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Run the reverse tunnel client",
		Example: `  # Client with inline bearer token
  k8s-reverse-tunnel client --bearer-token "$JWT" --server-url ws://127.0.0.1:8080/ws --client-id client-a --local-target 127.0.0.1:3000

  # Client with rotating token file
  k8s-reverse-tunnel client --bearer-token-file /var/run/secrets/krt/token.jwt --server-url wss://krt.example/ws --client-id edge-a --local-target 127.0.0.1:3000

  # Equivalent with environment variables
  KRT_BEARER_TOKEN_FILE=/var/run/secrets/krt/token.jwt KRT_SERVER_URL=wss://krt.example/ws KRT_CLIENT_ID=edge-a KRT_LOCAL_TARGET=127.0.0.1:3000 k8s-reverse-tunnel client`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.LoadFromViper(v)
			if err != nil {
				return err
			}
			if err := config.ValidateClient(cfg); err != nil {
				return err
			}

			logger := logging.New(cfg.LogLevel)
			logger.Info("starting k8s-reverse-tunnel client")

			return Run(ctx, cfg, logger)
		},
	}

	flags := cmd.Flags()
	flags.String("bearer-token", "", "Bearer JWT sent by client")
	flags.String("bearer-token-file", "", "File path for bearer JWT (reloaded on reconnect)")
	flags.String("token-refresh-window", "30s", "Proactive reconnect window before exp")
	flags.String("client-retry-interval", "1s", "Base retry interval for transport errors")
	flags.String("client-auth-retry-interval", "5s", "Base retry interval for auth failures")
	flags.String("server-url", "", "Server WebSocket URL for client mode")
	flags.String("client-id", "", "Unique client ID")
	flags.String("local-target", "", "Local host:port to expose")

	for _, key := range []string{
		"bearer-token",
		"bearer-token-file",
		"token-refresh-window",
		"client-retry-interval",
		"client-auth-retry-interval",
		"server-url",
		"client-id",
		"local-target",
	} {
		if err := v.BindPFlag(key, flags.Lookup(key)); err != nil {
			panic(err)
		}
	}

	return cmd
}

func Run(ctx context.Context, cfg config.Config, logger *logrus.Logger) error {
	c := clientimpl.New(cfg, logger)
	return c.Run(ctx)
}
