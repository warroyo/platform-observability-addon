package main

// Regression tests for the second review's findings.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func setRotate(h *harness, ns, name, value string) {
	h.t.Helper()
	c := h.cluster(ns, name)
	a := c.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[rotateAnnotation] = value
	c.SetAnnotations(a)
	if err := h.client.Update(context.Background(), c); err != nil {
		h.t.Fatal(err)
	}
}

func dropRotate(h *harness, ns, name string) {
	h.t.Helper()
	c := h.cluster(ns, name)
	a := c.GetAnnotations()
	delete(a, rotateAnnotation)
	c.SetAnnotations(a)
	if err := h.client.Update(context.Background(), c); err != nil {
		h.t.Fatal(err)
	}
}

func relabel(h *harness, ns, name string) {
	h.t.Helper()
	c := h.cluster(ns, name)
	c.SetLabels(map[string]string{"addons.kubernetes.vmware.com/platform-observability": "enabled"})
	if err := h.client.Update(context.Background(), c); err != nil {
		h.t.Fatal(err)
	}
}

// failApplies fails the next n applies.
func failApplies(h *harness, n int32) {
	var left atomic.Int32
	left.Store(n)
	h.r.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if left.Add(-1) >= 0 {
				return errTest
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
}

// #1: labelled again while offboarding is half done must not adopt a secret
// Ops already deleted.
func TestReview2RelabelDuringOffboardingOnboardsAgain(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")
	unlabel(h, "ns1", "c1")
	h.r.Ops = failingOffboard{h.r.Ops} // credentials removed, Ops DELETE fails
	_, _ = h.r.Reconcile(context.Background(), req("ns1", "c1"))
	h.r.Ops = h.r.Ops.(failingOffboard).OpsAPI
	relabel(h, "ns1", "c1")
	h.reconcile("ns1", "c1")
	if s := h.secretValue("ns1", "c1", "clientSecret"); s != "secret-c1-2" {
		t.Fatalf("clientSecret %q, want a newly issued one", s)
	}
}

// #2: the rotate request removed while its secret is pending: the issued
// secret is still written.
func TestReview2PendingRotationIsWrittenAfterRequestRemoved(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")
	setRotate(h, "ns1", "c1", "r1")
	failApplies(h, 1)
	_, _ = h.r.Reconcile(context.Background(), req("ns1", "c1"))
	dropRotate(h, "ns1", "c1")
	h.reconcile("ns1", "c1")
	if s := h.secretValue("ns1", "c1", "clientSecret"); s != "secret-c1-2" {
		t.Fatalf("clientSecret %q, want the issued secret-c1-2", s)
	}
}

// #3: a write that succeeded but reported an error leaves no stale pending
// entry for a later rotation to reuse.
func TestReview2CommittedPendingIsRetired(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	var lied atomic.Bool
	h.r.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			err := c.Apply(ctx, obj, opts...)
			if err == nil && lied.CompareAndSwap(false, true) {
				return errTest
			}
			return err
		},
	})
	_, _ = h.r.Reconcile(context.Background(), req("ns1", "c1"))
	h.reconcile("ns1", "c1")
	setRotate(h, "ns1", "c1", "r1")
	h.reconcile("ns1", "c1")
	if n := h.ops.count("POST c1"); n != 2 {
		t.Fatalf("POST %d times, want 2: the rotation reused a stale pending secret", n)
	}
	if s := h.secretValue("ns1", "c1", "clientSecret"); s != "secret-c1-2" {
		t.Fatalf("clientSecret %q", s)
	}
}

// #4: an empty value, and a value reused after a finished rotation, both ask.
func TestReview2EmptyAndReusedRotateValuesRotate(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")
	setRotate(h, "ns1", "c1", "")
	h.reconcile("ns1", "c1")
	h.reconcile("ns1", "c1") // drops the marker
	setRotate(h, "ns1", "c1", "now")
	h.reconcile("ns1", "c1")
	h.reconcile("ns1", "c1")
	setRotate(h, "ns1", "c1", "now")
	h.reconcile("ns1", "c1")
	if n := h.ops.count("POST c1"); n != 4 {
		t.Fatalf("POST %d times, want 4 (onboard + 3 rotations)", n)
	}
}

// #5: finishing request A leaves a newer request B in place.
func TestReview2ClearKeepsNewerRequest(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")
	setRotate(h, "ns1", "c1", "A")
	stale := h.cluster("ns1", "c1") // the reconciler's view: A
	setRotate(h, "ns1", "c1", "B")  // B lands before the clear
	if err := h.r.clearRotate(context.Background(), stale, "A"); err != nil {
		t.Fatal(err)
	}
	if v := h.cluster("ns1", "c1").GetAnnotations()[rotateAnnotation]; v != "B" {
		t.Fatalf("rotate %q, want B kept", v)
	}
}

// #7: the duplicate check lists Clusters by name only.
func TestReview2DuplicateCheckListsByName(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	var selector fields.Selector
	h.r.APIReader = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := (&client.ListOptions{}).ApplyOptions(opts)
			selector = lo.FieldSelector
			return c.List(ctx, list, opts...)
		},
	})
	h.reconcile("ns1", "c1")
	if selector == nil || selector.String() != "metadata.name=c1" {
		t.Fatalf("field selector %v, want metadata.name=c1", selector)
	}
}

// #8: concurrent misses against a failing Ops send one GET per backoff window.
func TestReview2FailingRefreshIsShared(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/suite-api/api/auth/token/acquire" {
			_, _ = w.Write([]byte(`{"token":"tok-1"}`))
			return
		}
		gets.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	ops := newOpsClient(srv.URL, "u", "p", srv.Client(), time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ops.Endpoints(context.Background()); err == nil {
				t.Error("want an error from a failing Ops")
			}
		}()
	}
	wg.Wait()
	if n := gets.Load(); n != 1 {
		t.Fatalf("GET %d times for 8 concurrent misses on a failing Ops, want 1", n)
	}
}

// Live run: unlabelling a cluster also stops it matching the AddonInstall,
// whose ClusterAddon is garbage collected with the AddonConfig it owns. The
// offboard must not recreate the AddonConfig when that lands mid-offboard.
func TestOffboardDoesNotRecreateCollectedAddonConfig(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.r.LegacyAddonConfig = true
	h.reconcile("ns1", "c1")
	// The fake client sets no UIDs; the real server gives every object one.
	orig := h.addonConfig("ns1", "c1")
	if err := h.client.Delete(context.Background(), orig); err != nil {
		t.Fatal(err)
	}
	orig.SetUID("original")
	orig.SetResourceVersion("")
	if err := h.client.Create(context.Background(), orig); err != nil {
		t.Fatal(err)
	}
	unlabel(h, "ns1", "c1")
	var collected atomic.Bool
	h.r.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if collected.CompareAndSwap(false, true) {
				ac := newAddonConfig()
				ac.SetNamespace("ns1")
				ac.SetName("c1-platform-observability")
				if err := c.Delete(ctx, ac); err != nil {
					t.Fatal(err)
				}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	h.reconcile("ns1", "c1")
	if !collected.Load() {
		t.Fatal("offboard made no apply")
	}
	ac := newAddonConfig()
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "c1-platform-observability"}, ac)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("AddonConfig recreated after it was collected: %v", err)
	}
	if fins := h.cluster("ns1", "c1").GetFinalizers(); len(fins) != 0 {
		t.Fatalf("finalizers %v, want none", fins)
	}
	if n := h.ops.count("DELETE c1"); n != 1 {
		t.Fatalf("DELETE %d times, want 1", n)
	}
}
