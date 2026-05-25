package expose

import "time"

const (
	// authSecretDataKey is the key in the Kubernetes Secret that holds the HMAC secret.
	authSecretDataKey = "hmac-secret"

	// JWTAudience and JWTIssuer are fixed for all expose-managed sessions.
	jwtAudience = "burrow-server"
	jwtIssuer   = "burrow-expose"

	managedByLabel = "burrow"
)

// Config holds all configuration for the expose command.
type Config struct {
	// Required
	ClientID    string
	LocalTarget string

	// ServerName is the prefix used for all Kubernetes resource names created
	// by expose (Deployment, Service, ServiceAccount, etc.). Defaults to
	// ClientID when unset. Use --server-name to share a single server
	// deployment across multiple clients (--reuse).
	ServerName string

	// Kubernetes target
	KubeContext string
	Namespace   string

	// Exposure mode: Hostname set → Ingress; unset → LoadBalancer
	Hostname           string
	TLSSecret          string // optional; omit to use ingress controller default cert
	IngressClass       string // empty = auto-detect cluster default
	IngressAnnotations map[string]string

	// Container image; defaults to ghcr.io/splattner/burrow:<version>
	Image string

	// Server HTTP port inside the container
	ServerPort int

	// Lifecycle
	Reuse       bool
	Keep        bool
	WaitTimeout time.Duration
	DryRun      bool

	// ConnectAddr overrides the TCP dial address for the client connection.
	// When set the client connects to this IP (or host:port) instead of
	// resolving the hostname from --hostname. The server URL keeps the hostname
	// for TLS SNI and the HTTP Host header. Only meaningful in Ingress mode.
	ConnectAddr string

	// Strategic merge patches (JSON) applied to resources before creation
	PatchDeployment string
	PatchService    string
	PatchIngress    string
}

// ResourceName returns the base Kubernetes resource name for this session.
func (c *Config) ResourceName() string {
	return "burrow-" + c.ServerName
}

// AuthSecretName returns the name of the Kubernetes Secret holding the HMAC key.
func (c *Config) AuthSecretName() string {
	return "burrow-" + c.ServerName + "-auth"
}

// IngressMode reports whether the session uses Ingress (hostname set) or LoadBalancer.
func (c *Config) IngressMode() bool {
	return c.Hostname != ""
}

func (c *Config) commonLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       c.ResourceName(),
		"app.kubernetes.io/managed-by": managedByLabel,
		"burrow.dev/server-name":       c.ServerName,
	}
}
