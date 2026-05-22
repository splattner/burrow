package kube

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestServiceNameForClientIsDeterministicAndBounded(t *testing.T) {
	clientID := "Client_A/Very-Long-Name-With-Many-Characters-And-Extra-Symbols-1234567890"
	name1 := ServiceNameForClient(clientID)
	name2 := ServiceNameForClient(clientID)

	if name1 != name2 {
		t.Fatalf("service name should be deterministic: %q != %q", name1, name2)
	}
	if len(name1) > 63 {
		t.Fatalf("service name length must be <=63, got %d (%q)", len(name1), name1)
	}
	if name1[:len(serviceNamePrefix)] != serviceNamePrefix {
		t.Fatalf("service name should start with prefix %q, got %q", serviceNamePrefix, name1)
	}
}

func TestReconcilerEnsureDisconnectAndSweep(t *testing.T) {
	r := NewReconcilerWithBridge("default", "127.0.0.1:1111")
	ctx := context.Background()

	rec, err := r.EnsureClientService(ctx, "client-a", "127.0.0.1:5432")
	if err != nil {
		t.Fatalf("ensure client service: %v", err)
	}
	if rec.Name == "" || rec.Namespace != "default" {
		t.Fatalf("unexpected record: %#v", rec)
	}
	if rec.Labels[labelClientIDKey] != "client-a" {
		t.Fatalf("expected client label to be set")
	}

	stored, ok := r.GetServiceForClient("client-a")
	if !ok {
		t.Fatal("expected service to be stored")
	}
	if stored.Annotations[annotationTargetKey] != "127.0.0.1:5432" {
		t.Fatalf("expected target annotation to be stored")
	}

	if err := r.MarkClientDisconnected(ctx, "client-a"); err != nil {
		t.Fatalf("mark disconnected: %v", err)
	}

	deleted := r.SweepStaleServices(ctx, 10*time.Minute, time.Now().Add(2*time.Minute))
	if len(deleted) != 0 {
		t.Fatalf("expected no deletions for fresh disconnect, got %v", deleted)
	}

	deleted = r.SweepStaleServices(ctx, 1*time.Minute, time.Now().Add(2*time.Hour))
	if len(deleted) != 1 || deleted[0] != "client-a" {
		t.Fatalf("expected client-a to be deleted, got %v", deleted)
	}
	if _, ok := r.GetServiceForClient("client-a"); ok {
		t.Fatal("expected service to be removed after stale sweep")
	}
}

func TestReconcilerKubeClientCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewSimpleClientset()
	r := NewReconcilerWithClient("default", "127.0.0.1:1111", fakeClient)

	firstTarget := "127.0.0.1:5432"
	rec, err := r.EnsureClientService(ctx, "client-a", firstTarget)
	if err != nil {
		t.Fatalf("ensure first service: %v", err)
	}

	svc, err := fakeClient.CoreV1().Services("default").Get(ctx, rec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created service: %v", err)
	}
	if svc.Annotations[annotationTargetKey] != firstTarget {
		t.Fatalf("expected target annotation %q, got %q", firstTarget, svc.Annotations[annotationTargetKey])
	}
	if svc.Spec.Ports[0].TargetPort.IntVal != 1111 {
		t.Fatalf("expected service targetPort 1111, got %d", svc.Spec.Ports[0].TargetPort.IntVal)
	}

	updatedTarget := "127.0.0.1:15432"
	_, err = r.EnsureClientService(ctx, "client-a", updatedTarget)
	if err != nil {
		t.Fatalf("ensure updated service: %v", err)
	}

	svc, err = fakeClient.CoreV1().Services("default").Get(ctx, rec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated service: %v", err)
	}
	if svc.Annotations[annotationTargetKey] != updatedTarget {
		t.Fatalf("expected updated target annotation %q, got %q", updatedTarget, svc.Annotations[annotationTargetKey])
	}
	if svc.Labels[labelClientIDKey] != "client-a" {
		t.Fatalf("expected label %q to stay set", labelClientIDKey)
	}
}

func TestReconcilerKubeClientSweepDeletesService(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewSimpleClientset()
	r := NewReconcilerWithClient("default", "127.0.0.1:1111", fakeClient)

	rec, err := r.EnsureClientService(ctx, "client-b", "127.0.0.1:9000")
	if err != nil {
		t.Fatalf("ensure service: %v", err)
	}

	if err := r.MarkClientDisconnected(ctx, "client-b"); err != nil {
		t.Fatalf("mark disconnected: %v", err)
	}

	deleted := r.SweepStaleServices(ctx, 1*time.Minute, time.Now().Add(2*time.Hour))
	if len(deleted) != 1 || deleted[0] != "client-b" {
		t.Fatalf("expected deleted client-b, got %v", deleted)
	}

	_, err = fakeClient.CoreV1().Services("default").Get(ctx, rec.Name, metav1.GetOptions{})
	if err == nil {
		t.Fatalf("expected service %q to be deleted", rec.Name)
	}
}
