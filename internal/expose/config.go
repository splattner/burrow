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

	// Server ports inside the container
	ServerPort int
	BridgePort int

	// Lifecycle
	Reuse       bool
	Keep        bool
	WaitTimeout time.Duration
	DryRun      bool

	// Strategic merge patches (JSON) applied to resources before creation
	PatchDeployment string
	PatchService    string
	PatchIngress    string
}

// ResourceName returns the base Kubernetes resource name for this session.
func (c *Config) ResourceName() string {
	return "burrow-" + c.ClientID
}

// AuthSecretName returns the name of the Kubernetes Secret holding the HMAC key.
func (c *Config) AuthSecretName() string {
	return "burrow-" + c.ClientID + "-auth"
}

// IngressMode reports whether the session uses Ingress (hostname set) or LoadBalancer.
func (c *Config) IngressMode() bool {
	return c.Hostname != ""
}

func (c *Config) commonLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       c.ResourceName(),
		"app.kubernetes.io/managed-by": managedByLabel,
		"burrow.dev/client-id":         c.ClientID,
	}
}
