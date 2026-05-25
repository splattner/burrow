package kube

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const (
	maxServiceNameLen   = 63
	serviceNamePrefix   = "burrow-client-"
	labelManagedByKey   = "app.kubernetes.io/managed-by"
	labelManagedByValue = "burrow"
	labelClientIDKey    = "burrow.dev/client-id"
	annotationTargetKey = "burrow.dev/target"
	envEnableKubeAPI    = "BURROW_ENABLE_KUBE_API"

	defaultServicePortName = "tcp"
	defaultServicePort     = 80
)

type ServiceRecord struct {
	Name        string
	Namespace   string
	ClientID    string
	Target      string
	Labels      map[string]string
	Annotations map[string]string
	UpdatedAt   time.Time
}

type clientState struct {
	ServiceName    string
	Target         string
	Connected      bool
	LastConnected  time.Time
	DisconnectedAt time.Time
}

type Reconciler struct {
	namespace string
	client    kubernetes.Interface

	mu       sync.RWMutex
	services map[string]ServiceRecord
	clients  map[string]clientState
}

func NewReconciler(namespace string) *Reconciler {
	return NewReconcilerWithOptions(namespace, nil)
}

// NewReconcilerWithOptions creates a Reconciler with optional Kubernetes API access.
func NewReconcilerWithOptions(namespace string, enableKubeAPI *bool) *Reconciler {
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	client, _ := newKubeClient(enableKubeAPI)
	return NewReconcilerWithClient(namespace, client)
}

// NewReconcilerWithBridge is kept for backward compatibility; the bridgeAddr
// parameter is ignored — bridge ports are now passed per EnsureClientService call.
func NewReconcilerWithBridge(namespace, _ string) *Reconciler {
	return NewReconcilerWithOptions(namespace, nil)
}

func NewReconcilerWithClient(namespace string, client kubernetes.Interface) *Reconciler {
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}

	return &Reconciler{
		namespace: namespace,
		client:    client,
		services:  make(map[string]ServiceRecord),
		clients:   make(map[string]clientState),
	}
}

func (r *Reconciler) EnsureInfrastructure(ctx context.Context) error {
	_ = ctx
	return nil
}

func (r *Reconciler) EnsureClientService(ctx context.Context, clientID, target string, bridgePort int32) (ServiceRecord, error) {
	_ = ctx
	if strings.TrimSpace(clientID) == "" {
		return ServiceRecord{}, fmt.Errorf("client ID is required")
	}

	now := time.Now()
	name := ServiceNameForClient(clientID)
	record := ServiceRecord{
		Name:      name,
		Namespace: r.namespace,
		ClientID:  clientID,
		Target:    target,
		Labels: map[string]string{
			labelManagedByKey: labelManagedByValue,
			labelClientIDKey:  clientID,
		},
		Annotations: map[string]string{
			annotationTargetKey: target,
		},
		UpdatedAt: now,
	}

	r.mu.Lock()
	r.services[clientID] = record
	r.clients[clientID] = clientState{
		ServiceName:    name,
		Target:         target,
		Connected:      true,
		LastConnected:  now,
		DisconnectedAt: time.Time{},
	}
	r.mu.Unlock()

	if r.client != nil {
		if err := r.applyServiceRecord(ctx, record, bridgePort); err != nil {
			return ServiceRecord{}, err
		}
	}

	return copyRecord(record), nil
}

func (r *Reconciler) MarkClientDisconnected(ctx context.Context, clientID string) error {
	_ = ctx
	if strings.TrimSpace(clientID) == "" {
		return nil
	}

	r.mu.Lock()
	state, ok := r.clients[clientID]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	state.Connected = false
	state.DisconnectedAt = time.Now()
	r.clients[clientID] = state
	r.mu.Unlock()
	return nil
}

func (r *Reconciler) SweepStaleServices(ctx context.Context, maxDisconnectedAge time.Duration, now time.Time) []string {
	_ = ctx
	if maxDisconnectedAge <= 0 {
		return nil
	}

	deleted := make([]string, 0)
	r.mu.Lock()
	for clientID, state := range r.clients {
		if state.Connected || state.DisconnectedAt.IsZero() {
			continue
		}
		if now.Sub(state.DisconnectedAt) < maxDisconnectedAge {
			continue
		}
		delete(r.clients, clientID)
		delete(r.services, clientID)
		if r.client != nil {
			_ = r.client.CoreV1().Services(r.namespace).Delete(ctx, state.ServiceName, metav1.DeleteOptions{})
		}
		deleted = append(deleted, clientID)
	}
	r.mu.Unlock()

	sort.Strings(deleted)
	return deleted
}

func (r *Reconciler) GetServiceForClient(clientID string) (ServiceRecord, bool) {
	r.mu.RLock()
	record, ok := r.services[clientID]
	r.mu.RUnlock()
	if !ok {
		return ServiceRecord{}, false
	}
	return copyRecord(record), true
}

func ServiceNameForClient(clientID string) string {
	slug := sanitizeDNSLabel(clientID)
	if slug == "" {
		slug = "client"
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(clientID))
	hash := fmt.Sprintf("%08x", h.Sum32())

	maxSlugLen := maxServiceNameLen - len(serviceNamePrefix) - 1 - len(hash)
	if maxSlugLen < 1 {
		maxSlugLen = 1
	}
	if len(slug) > maxSlugLen {
		slug = slug[:maxSlugLen]
		slug = strings.Trim(slug, "-")
		if slug == "" {
			slug = "client"
		}
	}

	return serviceNamePrefix + slug + "-" + hash
}

func sanitizeDNSLabel(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(in)) {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	return out
}

func copyRecord(in ServiceRecord) ServiceRecord {
	out := in
	out.Labels = copyMap(in.Labels)
	out.Annotations = copyMap(in.Annotations)
	return out
}

func copyMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *Reconciler) applyServiceRecord(ctx context.Context, record ServiceRecord, bridgePort int32) error {
	svcClient := r.client.CoreV1().Services(r.namespace)
	existing, err := svcClient.Get(ctx, record.Name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get service %s: %w", record.Name, err)
		}

		_, createErr := svcClient.Create(ctx, buildService(record, bridgePort), metav1.CreateOptions{})
		if createErr != nil {
			if !apierrors.IsAlreadyExists(createErr) {
				return fmt.Errorf("create service %s: %w", record.Name, createErr)
			}

			existing, err = svcClient.Get(ctx, record.Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("get service after already-exists %s: %w", record.Name, err)
			}
		} else {
			return nil
		}
	}

	desired := buildService(record, bridgePort)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, getErr := svcClient.Get(ctx, existing.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}

		latest.Labels = copyMap(desired.Labels)
		latest.Annotations = copyMap(desired.Annotations)
		latest.Spec.Ports = desired.Spec.Ports
		latest.Spec.Selector = copyMap(desired.Spec.Selector)

		_, updateErr := svcClient.Update(ctx, latest, metav1.UpdateOptions{})
		return updateErr
	})
}

// targetPortFromAddress parses the port from a host:port address string.
// Falls back to defaultServicePort if parsing fails.
func targetPortFromAddress(addr string) int32 {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return defaultServicePort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return defaultServicePort
	}
	return int32(port)
}

func buildService(record ServiceRecord, bridgePort int32) *corev1.Service {
	svcPort := targetPortFromAddress(record.Target)
	targetPort := bridgePort
	if targetPort <= 0 {
		targetPort = svcPort
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        record.Name,
			Namespace:   record.Namespace,
			Labels:      copyMap(record.Labels),
			Annotations: copyMap(record.Annotations),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app": "burrow-server",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       defaultServicePortName,
					Port:       svcPort,
					TargetPort: intstr.FromInt32(targetPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

func newKubeClient(explicitEnable *bool) (kubernetes.Interface, error) {
	enable := ""
	if explicitEnable != nil {
		if *explicitEnable {
			enable = "true"
		} else {
			enable = "false"
		}
	} else {
		enable = strings.ToLower(strings.TrimSpace(os.Getenv(envEnableKubeAPI)))
	}
	if enable == "false" || enable == "0" || enable == "no" {
		return nil, nil
	}

	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}

	if enable != "true" && enable != "1" && enable != "yes" {
		return nil, nil
	}

	kubeconfig := os.Getenv("KUBECONFIG")
	if strings.TrimSpace(kubeconfig) == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr == nil && home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	if strings.TrimSpace(kubeconfig) == "" {
		return nil, fmt.Errorf("kubeconfig path not available")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
