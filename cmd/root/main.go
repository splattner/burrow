package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/splattner/k8s-reverse-tunnel/cmd/client"
	"github.com/splattner/k8s-reverse-tunnel/cmd/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	v := newViper()

	rootCmd := &cobra.Command{
		Use:           "k8s-reverse-tunnel",
		Short:         "Expose local TCP services to Kubernetes over a WebSocket tunnel",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	rootCmd.PersistentFlags().String("log-level", "info", "Log level (debug|info|warn|error)")
	must(v.BindPFlag("log-level", rootCmd.PersistentFlags().Lookup("log-level")))

	rootCmd.AddCommand(server.NewCommand(ctx, v))
	rootCmd.AddCommand(client.NewCommand(ctx, v))

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if err == context.Canceled {
			return
		}
		fmt.Fprintf(os.Stderr, "command error: %v\n", err)
		os.Exit(1)
	}
}

func newViper() *viper.Viper {
	v := viper.New()
	v.SetEnvPrefix("KRT")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	return v
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
