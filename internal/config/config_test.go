package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadFromEnvDurationDefaults(t *testing.T) {
	t.Setenv("BURROW_JWT_HMAC_SECRET", "dev-secret")
	clearDurationEnv(t)

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.HeartbeatInterval != 10*time.Second {
		t.Fatalf("unexpected heartbeat interval: %v", cfg.HeartbeatInterval)
	}
	if cfg.HeartbeatTimeout != 30*time.Second {
		t.Fatalf("unexpected heartbeat timeout: %v", cfg.HeartbeatTimeout)
	}
	if cfg.SweepInterval != time.Minute {
		t.Fatalf("unexpected sweep interval: %v", cfg.SweepInterval)
	}
	if cfg.StaleServiceAge != 10*time.Minute {
		t.Fatalf("unexpected stale service age: %v", cfg.StaleServiceAge)
	}
	if cfg.TokenRefreshWindow != 30*time.Second {
		t.Fatalf("unexpected token refresh window: %v", cfg.TokenRefreshWindow)
	}
	if cfg.RetryInterval != time.Second {
		t.Fatalf("unexpected client retry interval: %v", cfg.RetryInterval)
	}
	if cfg.AuthRetryInterval != 5*time.Second {
		t.Fatalf("unexpected client auth retry interval: %v", cfg.AuthRetryInterval)
	}
}

func TestLoadFromEnvDurationOverrides(t *testing.T) {
	t.Setenv("BURROW_JWT_HMAC_SECRET", "dev-secret")
	t.Setenv("BURROW_HEARTBEAT_INTERVAL", "5s")
	t.Setenv("BURROW_HEARTBEAT_TIMEOUT", "45s")
	t.Setenv("BURROW_SWEEP_INTERVAL", "30s")
	t.Setenv("BURROW_STALE_SERVICE_AGE", "2m")
	t.Setenv("BURROW_TOKEN_REFRESH_WINDOW", "20s")
	t.Setenv("BURROW_CLIENT_RETRY_INTERVAL", "2s")
	t.Setenv("BURROW_CLIENT_AUTH_RETRY_INTERVAL", "7s")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.HeartbeatInterval != 5*time.Second || cfg.HeartbeatTimeout != 45*time.Second {
		t.Fatalf("unexpected heartbeat values: interval=%v timeout=%v", cfg.HeartbeatInterval, cfg.HeartbeatTimeout)
	}
	if cfg.SweepInterval != 30*time.Second || cfg.StaleServiceAge != 2*time.Minute {
		t.Fatalf("unexpected sweep values: interval=%v stale=%v", cfg.SweepInterval, cfg.StaleServiceAge)
	}
	if cfg.TokenRefreshWindow != 20*time.Second || cfg.RetryInterval != 2*time.Second || cfg.AuthRetryInterval != 7*time.Second {
		t.Fatalf("unexpected client retry/refresh values: refresh=%v retry=%v auth_retry=%v", cfg.TokenRefreshWindow, cfg.RetryInterval, cfg.AuthRetryInterval)
	}
}

func TestLoadFromEnvInvalidDuration(t *testing.T) {
	t.Setenv("BURROW_JWT_HMAC_SECRET", "dev-secret")
	t.Setenv("BURROW_SWEEP_INTERVAL", "not-a-duration")

	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
	if !strings.Contains(err.Error(), "BURROW_SWEEP_INTERVAL") {
		t.Fatalf("expected env key in error, got %v", err)
	}
}

func TestValidateServerRequiresJWTSource(t *testing.T) {
	cfg := Config{}
	err := ValidateServer(cfg)
	if err == nil {
		t.Fatal("expected validation error for empty server config")
	}
	for _, want := range []string{"--jwt-hmac-secret", "BURROW_JWT_HMAC_SECRET", "--jwks-url", "BURROW_JWKS_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q in error, got: %v", want, err)
		}
	}
}

func TestValidateServerAcceptsJWTSources(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"hmac-secret", Config{JWTHMACSecret: "dev-secret"}},
		{"public-key-file", Config{JWTPublicKeyFile: "/etc/burrow/jwt.pem"}},
		{"jwks-url", Config{JWKSURL: "https://issuer.example/.well-known/jwks.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateServer(tc.cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateClientRequiresAllFields(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "empty config",
			cfg:     Config{},
			wantErr: "--bearer-token",
		},
		{
			name:    "missing server-url",
			cfg:     Config{BearerToken: "tok", ClientID: "c", LocalTarget: "127.0.0.1:3000"},
			wantErr: "--server-url",
		},
		{
			name:    "missing client-id",
			cfg:     Config{BearerToken: "tok", ServerURL: "ws://x/ws", LocalTarget: "127.0.0.1:3000"},
			wantErr: "--client-id",
		},
		{
			name:    "missing local-target",
			cfg:     Config{BearerToken: "tok", ServerURL: "ws://x/ws", ClientID: "c"},
			wantErr: "--local-target",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateClient(tc.cfg)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q in error, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateClientAcceptsValidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			name: "inline bearer",
			cfg:  Config{BearerToken: "tok", ServerURL: "ws://x/ws", ClientID: "c", LocalTarget: "127.0.0.1:3000"},
		},
		{
			name: "bearer file",
			cfg:  Config{BearerTokenFile: "/var/run/token", ServerURL: "wss://x/ws", ClientID: "c", LocalTarget: "127.0.0.1:3000"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateClient(tc.cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadFromEnvAcceptsBearerOnly(t *testing.T) {
	t.Setenv("BURROW_BEARER_TOKEN", "dev-jwt")
	t.Setenv("BURROW_JWT_HMAC_SECRET", "")
	t.Setenv("BURROW_JWT_PUBLIC_KEY_FILE", "")
	t.Setenv("BURROW_JWKS_URL", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("expected bearer-only config to load: %v", err)
	}
	if cfg.BearerToken != "dev-jwt" {
		t.Fatalf("unexpected bearer token value: %q", cfg.BearerToken)
	}
}

func TestLoadFromEnvAcceptsBearerFileOnly(t *testing.T) {
	t.Setenv("BURROW_BEARER_TOKEN", "")
	t.Setenv("BURROW_BEARER_TOKEN_FILE", "/tmp/client-token.jwt")
	t.Setenv("BURROW_JWT_HMAC_SECRET", "")
	t.Setenv("BURROW_JWT_PUBLIC_KEY_FILE", "")
	t.Setenv("BURROW_JWKS_URL", "")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("expected bearer-file-only config to load: %v", err)
	}
	if cfg.BearerTokenFile != "/tmp/client-token.jwt" {
		t.Fatalf("unexpected bearer token file value: %q", cfg.BearerTokenFile)
	}
}

func TestLoadFromEnvJWTAcceptsJWKSURL(t *testing.T) {
	t.Setenv("BURROW_JWT_HMAC_SECRET", "")
	t.Setenv("BURROW_JWT_PUBLIC_KEY_FILE", "")
	t.Setenv("BURROW_JWKS_URL", "https://issuer.example/.well-known/jwks.json")

	if _, err := LoadFromEnv(); err != nil {
		t.Fatalf("expected config to accept JWKS URL: %v", err)
	}
}

func TestLoadFromEnvParsesEnableKubeAPI(t *testing.T) {
	t.Setenv("BURROW_JWT_HMAC_SECRET", "dev-secret")
	t.Setenv("BURROW_ENABLE_KUBE_API", "true")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.EnableKubeAPI == nil {
		t.Fatal("expected EnableKubeAPI to be set")
	}
	if !*cfg.EnableKubeAPI {
		t.Fatal("expected EnableKubeAPI to be true")
	}
}

func TestLoadFromEnvRejectsInvalidEnableKubeAPI(t *testing.T) {
	t.Setenv("BURROW_JWT_HMAC_SECRET", "dev-secret")
	t.Setenv("BURROW_ENABLE_KUBE_API", "maybe")

	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error for invalid BURROW_ENABLE_KUBE_API")
	}
	if !strings.Contains(err.Error(), "BURROW_ENABLE_KUBE_API") {
		t.Fatalf("expected env key in error, got %v", err)
	}
}

func clearDurationEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BURROW_HEARTBEAT_INTERVAL",
		"BURROW_HEARTBEAT_TIMEOUT",
		"BURROW_SWEEP_INTERVAL",
		"BURROW_STALE_SERVICE_AGE",
		"BURROW_TOKEN_REFRESH_WINDOW",
		"BURROW_CLIENT_RETRY_INTERVAL",
		"BURROW_CLIENT_AUTH_RETRY_INTERVAL",
	} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}
