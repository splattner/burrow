package client

import (
	"context"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	clientimpl "github.com/splattner/burrow/internal/client"
	"github.com/splattner/burrow/internal/config"
	"github.com/splattner/burrow/internal/logging"
)

func NewCommand(ctx context.Context, v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Run the reverse tunnel client",
		Example: `  # Client with inline bearer token
  burrow client --bearer-token "$JWT" --server-url ws://127.0.0.1:8080/ws --client-id client-a --local-target 127.0.0.1:3000

  # Client with rotating token file
  burrow client --bearer-token-file /var/run/secrets/burrow/token.jwt --server-url wss://burrow.example/ws --client-id edge-a --local-target 127.0.0.1:3000

  # Equivalent with environment variables
  BURROW_BEARER_TOKEN_FILE=/var/run/secrets/burrow/token.jwt BURROW_SERVER_URL=wss://burrow.example/ws BURROW_CLIENT_ID=edge-a BURROW_LOCAL_TARGET=127.0.0.1:3000 burrow client`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.LoadFromViper(v)
			if err != nil {
				return err
			}
			if err := config.ValidateClient(cfg); err != nil {
				return err
			}

			logger := logging.New(cfg.LogLevel)
			logger.Info("starting burrow client")

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
