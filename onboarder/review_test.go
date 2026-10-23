package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func unlabel(h *harness, ns, name string) {
	c := h.cluster(ns, name)
	c.SetLabels(nil)
	if err := h.client.Update(context.Background(), c); err != nil {
		h.t.Fatal(err)
	}
}

// #1: an onboarded cluster whose label is gone but finalizer remains still owns the name.
func TestReviewOffboardingClusterReservesName(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil), cluster("ns2", "c1", true, nil))
	h.reconcile("ns1", "c1")
	unlabel(h, "ns1", "c1")
	h.r.Ops = failingOffboard{h.r.Ops} // ns1 offboarding stuck
	_, _ = h.r.Reconcile(context.Background(), req("ns1", "c1"))
	h.reconcile("ns2", "c1")
	if n := h.ops.count("POST c1"); n != 1 {
		t.Fatalf("POST %d times: ns2 onboarded while ns1 still holds the name", n)
	}
}

// #2: adoption must not let a duplicate take the finalizer.
func TestReviewAdoptionRespectsDuplicates(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil), cluster("ns2", "c1", true, nil))
	h.reconcile("ns1", "c1")
	seed(h, "ns2", "c1", "kubectl", "other-secret")
	h.reconcile("ns2", "c1")
	if controllerutil.ContainsFinalizer(h.cluster("ns2", "c1"), finalizer) {
		t.Fatal("duplicate adopted and got the finalizer")
	}
}

// #3a: endpoints failure after POST must not cause a second POST on retry.
func TestReviewEndpointsFailureDoesNotRotateTwice(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	real := h.r.Ops
	h.r.Ops = failingEndpoints{real}
	_, _ = h.r.Reconcile(context.Background(), req("ns1", "c1"))
	h.r.Ops = real
	h.reconcile("ns1", "c1")
	if n := h.ops.count("POST c1"); n != 1 {
		t.Fatalf("POST %d times after an endpoints failure", n)
	}
}

// #5: offboarding an adopted AddonConfig must remove the credentials even if another manager co-owned them.
func TestReviewOffboardAfterAdoptionRemovesCredentials(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	seed(h, "ns1", "c1", "kubectl", "kept-secret")
	h.reconcile("ns1", "c1")
	unlabel(h, "ns1", "c1")
	h.reconcile("ns1", "c1")
	if s := h.secretValue("ns1", "c1", "clientSecret"); s != "" {
		t.Fatalf("revoked secret %q left in the Secret", s)
	}
	if s := helmValue(h.addonConfig("ns1", "c1"), "vcfOps", "clientSecret"); s != "" {
		t.Fatalf("revoked secret %q left in the AddonConfig", s)
	}
}

// #6: a secret without a client ID must not trigger a POST.
func TestReviewPartialCredentialsDoNotRotate(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	seed(h, "ns1", "c1", "kubectl", "kept-secret")
	ac := h.addonConfig("ns1", "c1")
	unstructured.RemoveNestedField(ac.Object, "spec", "values", "helmValues", "vcfOps", "clientId")
	if err := h.client.Update(context.Background(), ac); err != nil {
		t.Fatal(err)
	}
	_, _ = h.r.Reconcile(context.Background(), req("ns1", "c1"))
	if n := h.ops.count("POST c1"); n != 0 {
		t.Fatalf("POST %d times with a secret already present", n)
	}
}

func seed(h *harness, ns, name, manager, secret string) {
	existing := newAddonConfig()
	existing.SetNamespace(ns)
	existing.SetName(name + "-platform-observability")
	existing.Object["spec"] = map[string]any{"values": map[string]any{"helmValues": map[string]any{
		"clusterName": name,
		"vcfOps": map[string]any{
			"instanceFqdn": "shared01.example", "platformFqdn": "vsp01.example",
			"instanceCA": "INSTANCE-CA", "platformCA": "PLATFORM-CA",
			"clientId": "id-" + name, "clientSecret": secret,
		},
	}}}
	if err := h.client.Apply(context.Background(), client.ApplyConfigurationFromUnstructured(existing),
		client.FieldOwner(manager), client.ForceOwnership); err != nil {
		h.t.Fatal(err)
	}
}

type failingOffboard struct{ OpsAPI }

func (failingOffboard) Offboard(context.Context, string) error { return errTest }

type failingEndpoints struct{ OpsAPI }

func (failingEndpoints) Endpoints(context.Context) (Endpoints, error) { return Endpoints{}, errTest }

var errTest = errorString("ops unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }

// A write failing after the POST: the retry writes the same secret, it does
// not issue another.
func TestReviewFailedWriteReusesIssuedSecret(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	var failed atomic.Bool
	h.r.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if failed.CompareAndSwap(false, true) {
				return errTest
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	if _, err := h.r.Reconcile(context.Background(), req("ns1", "c1")); err == nil {
		t.Fatal("want the first reconcile to fail on the write")
	}
	h.reconcile("ns1", "c1")
	if n := h.ops.count("POST c1"); n != 1 {
		t.Fatalf("POST %d times, want 1", n)
	}
	if s := h.secretValue("ns1", "c1", "clientSecret"); s != "secret-c1-1" {
		t.Fatalf("clientSecret %q, want the first one issued", s)
	}
}

// A rotation whose secret was written but whose annotation was not cleared:
// the retry only clears the annotation.
func TestReviewRotationIsNotRepeatedWhenClearFails(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")
	c := h.cluster("ns1", "c1")
	c.SetAnnotations(map[string]string{rotateAnnotation: "req-1"})
	if err := h.client.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	base := h.client.(client.WithWatch)
	h.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetObjectKind().GroupVersionKind() == clusterGVK && patch.Type() == types.JSONPatchType &&
				failed.CompareAndSwap(false, true) {
				return errTest
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	if _, err := h.r.Reconcile(context.Background(), req("ns1", "c1")); err == nil {
		t.Fatal("want the clear to fail")
	}
	h.reconcile("ns1", "c1")
	if n := h.ops.count("POST c1"); n != 2 {
		t.Fatalf("POST %d times, want 2 (onboard + one rotation)", n)
	}
	if _, ok := h.cluster("ns1", "c1").GetAnnotations()[rotateAnnotation]; ok {
		t.Fatal("rotate annotation still set")
	}
	if got := h.secret("ns1", "c1").GetAnnotations()[rotatedForAnnotation]; got != "req-1" {
		t.Fatalf("rotated-for %q", got)
	}
}

// #7: concurrent cache misses share one GET.
func TestReviewConcurrentEndpointMissesShareOneGET(t *testing.T) {
	f, ops := newFakeOps(t)
	if _, err := ops.Onboard(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ops.Endpoints(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count("GET"); n != 1 {
		t.Fatalf("GET %d times for 8 concurrent misses, want 1", n)
	}
}
