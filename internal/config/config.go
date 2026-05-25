package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	BearerToken        string
	BearerTokenFile    string
	EnableKubeAPI      *bool
	TokenRefreshWindow time.Duration
	RetryInterval      time.Duration
	AuthRetryInterval  time.Duration
	JWTIssuer          string
	JWTAudience        string
	JWTAlg             string
	JWTHMACSecret      string
	JWTPublicKeyFile   string
	JWKSURL            string
	JWKSRefresh        time.Duration
	ServerAddr         string
	BridgeHost         string
	ServerURL          string
	ConnectAddr        string
	ClientID           string
	LocalTarget        string
	Namespace          string
	LogLevel           string
	HeartbeatInterval  time.Duration
	HeartbeatTimeout   time.Duration
	SweepInterval      time.Duration
	StaleServiceAge    time.Duration
}

func LoadFromEnv() (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("BURROW")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()

	return LoadFromViper(v)
}

func LoadFromViper(v *viper.Viper) (Config, error) {
	heartbeatInterval, err := parseDuration(v, "heartbeat-interval", "BURROW_HEARTBEAT_INTERVAL", 10*time.Second)
	if err != nil {
		return Config{}, err
	}

	heartbeatTimeout, err := parseDuration(v, "heartbeat-timeout", "BURROW_HEARTBEAT_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}

	sweepInterval, err := parseDuration(v, "sweep-interval", "BURROW_SWEEP_INTERVAL", 1*time.Minute)
	if err != nil {
		return Config{}, err
	}

	staleAge, err := parseDuration(v, "stale-service-age", "BURROW_STALE_SERVICE_AGE", 10*time.Minute)
	if err != nil {
		return Config{}, err
	}

	jwksRefresh, err := parseDuration(v, "jwks-refresh", "BURROW_JWKS_REFRESH", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}

	tokenRefreshWindow, err := parseDuration(v, "token-refresh-window", "BURROW_TOKEN_REFRESH_WINDOW", 30*time.Second)
	if err != nil {
		return Config{}, err
	}

	retryInterval, err := parseDuration(v, "client-retry-interval", "BURROW_CLIENT_RETRY_INTERVAL", 1*time.Second)
	if err != nil {
		return Config{}, err
	}

	authRetryInterval, err := parseDuration(v, "client-auth-retry-interval", "BURROW_CLIENT_AUTH_RETRY_INTERVAL", 5*time.Second)
	if err != nil {
		return Config{}, err
	}

	enableKubeAPI, err := parseOptionalBool(v, "enable-kube-api", "BURROW_ENABLE_KUBE_API")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		BearerToken:        strings.TrimSpace(v.GetString("bearer-token")),
		BearerTokenFile:    strings.TrimSpace(v.GetString("bearer-token-file")),
		EnableKubeAPI:      enableKubeAPI,
		TokenRefreshWindow: tokenRefreshWindow,
		RetryInterval:      retryInterval,
		AuthRetryInterval:  authRetryInterval,
		JWTIssuer:          strings.TrimSpace(v.GetString("jwt-issuer")),
		JWTAudience:        strings.TrimSpace(v.GetString("jwt-audience")),
		JWTAlg:             fallbackString(v.GetString("jwt-alg"), "RS256"),
		JWTHMACSecret:      strings.TrimSpace(v.GetString("jwt-hmac-secret")),
		JWTPublicKeyFile:   strings.TrimSpace(v.GetString("jwt-public-key-file")),
		JWKSURL:            strings.TrimSpace(v.GetString("jwks-url")),
		JWKSRefresh:        jwksRefresh,
		ServerAddr:         fallbackString(v.GetString("server-addr"), ":8080"),
		BridgeHost:         strings.TrimSpace(v.GetString("bridge-host")),
		ServerURL:          strings.TrimSpace(v.GetString("server-url")),
		ConnectAddr:        strings.TrimSpace(v.GetString("connect-addr")),
		ClientID:           strings.TrimSpace(v.GetString("client-id")),
		LocalTarget:        strings.TrimSpace(v.GetString("local-target")),
		Namespace:          fallbackString(v.GetString("namespace"), "default"),
		LogLevel:           fallbackString(v.GetString("log-level"), "info"),
		HeartbeatInterval:  heartbeatInterval,
		HeartbeatTimeout:   heartbeatTimeout,
		SweepInterval:      sweepInterval,
		StaleServiceAge:    staleAge,
	}

	return cfg, nil
}

// ValidateServer checks that cfg contains the settings required to run in server mode.
// It must be called after LoadFromViper in the server subcommand.
func ValidateServer(cfg Config) error {
	if cfg.JWTHMACSecret == "" && cfg.JWTPublicKeyFile == "" && cfg.JWKSURL == "" {
		return fmt.Errorf("server requires a JWT verifier source: set --jwt-hmac-secret / BURROW_JWT_HMAC_SECRET, --jwt-public-key-file / BURROW_JWT_PUBLIC_KEY_FILE, or --jwks-url / BURROW_JWKS_URL")
	}
	return nil
}

// ValidateClient checks that cfg contains the settings required to run in client mode.
// It must be called after LoadFromViper in the client subcommand.
func ValidateClient(cfg Config) error {
	var errs []string
	if cfg.BearerToken == "" && cfg.BearerTokenFile == "" {
		errs = append(errs, "bearer token required: set --bearer-token / BURROW_BEARER_TOKEN or --bearer-token-file / BURROW_BEARER_TOKEN_FILE")
	}
	if cfg.ServerURL == "" {
		errs = append(errs, "server URL required: set --server-url / BURROW_SERVER_URL")
	}
	if cfg.ClientID == "" {
		errs = append(errs, "client ID required: set --client-id / BURROW_CLIENT_ID")
	}
	if cfg.LocalTarget == "" {
		errs = append(errs, "local target required: set --local-target / BURROW_LOCAL_TARGET")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func fallbackString(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func parseDuration(v *viper.Viper, key, envKey string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(v.GetString(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s: %w", envKey, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration for %s must be > 0", envKey)
	}
	return d, nil
}

func parseOptionalBool(v *viper.Viper, key, envKey string) (*bool, error) {
	raw := strings.ToLower(strings.TrimSpace(v.GetString(key)))
	if raw == "" {
		return nil, nil
	}

	if raw == "true" || raw == "1" || raw == "yes" {
		value := true
		return &value, nil
	}
	if raw == "false" || raw == "0" || raw == "no" {
		value := false
		return &value, nil
	}

	return nil, fmt.Errorf("invalid boolean for %s: %q", envKey, raw)
}
