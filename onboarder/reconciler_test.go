package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type harness struct {
	t      *testing.T
	ops    *fakeOpsServer
	client client.Client
	rec    *events.FakeRecorder
	r      *Reconciler
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(clusterGVK, meta.RESTScopeNamespace)
	mapper.Add(addonConfigGVK, meta.RESTScopeNamespace)
	mapper.Add(secretGVK, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithRESTMapper(mapper).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithReturnManagedFields().
		WithIndex(newCluster(), "metadata.name", func(o client.Object) []string { return []string{o.GetName()} }).
		WithObjects(objs...).
		Build()
	f, ops := newFakeOps(t)
	rec := events.NewFakeRecorder(50)
	return &harness{t: t, ops: f, client: c, rec: rec, r: &Reconciler{
		Client: c, APIReader: c, Ops: ops, Recorder: rec,
		AddonName: "platform-observability", FieldManager: "platform-observability-onboarder",
	}}
}

func cluster(ns, name string, enabled bool, annotations map[string]string) *unstructured.Unstructured {
	u := newCluster()
	u.SetNamespace(ns)
	u.SetName(name)
	if enabled {
		u.SetLabels(map[string]string{"addons.kubernetes.vmware.com/platform-observability": "enabled"})
	}
	u.SetAnnotations(annotations)
	return u
}

func req(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func (h *harness) reconcile(ns, name string) {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		h.t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
}

func (h *harness) cluster(ns, name string) *unstructured.Unstructured {
	h.t.Helper()
	u := newCluster()
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u); err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) addonConfig(ns, cluster string) *unstructured.Unstructured {
	h.t.Helper()
	u := newAddonConfig()
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: cluster + "-platform-observability"}, u); err != nil {
		h.t.Fatal(err)
	}
	return u
}

// secret returns the cluster's credentials Secret, or nil if there is none.
func (h *harness) secret(ns, cluster string) *unstructured.Unstructured {
	h.t.Helper()
	u := newSecret()
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "platform-observability-" + cluster}, u)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

// secretValue returns one decoded key of the cluster's credentials Secret, or
// "" if there is no Secret.
func (h *harness) secretValue(ns, cluster, key string) string {
	h.t.Helper()
	u := h.secret(ns, cluster)
	if u == nil {
		return ""
	}
	s, _, _ := unstructured.NestedString(u.Object, "data", key)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func helmValue(u *unstructured.Unstructured, path ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, append([]string{"spec", "values", "helmValues"}, path...)...)
	return s
}

func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestOnboardsOnceAndThenLeavesTheSecretAlone(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")

	if got := h.secretValue("ns1", "c1", "clientSecret"); got != "secret-c1-1" {
		t.Fatalf("clientSecret %q", got)
	}
	s := h.secret("ns1", "c1")
	if h.secretValue("ns1", "c1", "instanceFqdn") != "shared01.example" || h.secretValue("ns1", "c1", "clientId") == "" ||
		s.GetAnnotations()[secretHashAnnotation] != secretHash("secret-c1-1") {
		t.Fatalf("secret: %v", s.Object)
	}
	if !controllerutil.ContainsFinalizer(h.cluster("ns1", "c1"), finalizer) {
		t.Fatal("no finalizer after onboarding")
	}
	ac := newAddonConfig()
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "c1-platform-observability"}, ac); !apierrors.IsNotFound(err) {
		t.Fatalf("AddonConfig written without --legacy-addon-config: %v", err)
	}
	rv := s.GetResourceVersion()

	// Resyncs: no second POST, and no write.
	h.reconcile("ns1", "c1")
	h.reconcile("ns1", "c1")
	if n := h.ops.count("POST c1"); n != 1 {
		t.Fatalf("POST %d times, want 1", n)
	}
	if got := h.secret("ns1", "c1").GetResourceVersion(); got != rv {
		t.Fatalf("Secret written on resync: resourceVersion %s -> %s", rv, got)
	}
	if ev := h.events(); len(ev) != 1 || !strings.Contains(ev[0], "Onboarded") {
		t.Fatalf("events: %v", ev)
	}
}

func TestAdoptsAnOnboardShAddonConfigWithoutRotating(t *testing.T) {
	existing := newAddonConfig()
	existing.SetNamespace("ns1")
	existing.SetName("c1-platform-observability")
	existing.Object["spec"] = map[string]any{"values": map[string]any{"helmValues": map[string]any{
		"clusterName": "c1",
		"vcfOps": map[string]any{
			"instanceFqdn": "shared01.example", "platformFqdn": "vsp01.example",
			"instanceCA": "INSTANCE-CA", "platformCA": "PLATFORM-CA",
			"clientId": "id-c1", "clientSecret": "kept-secret",
		},
	}}}
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	if err := h.client.Apply(context.Background(), client.ApplyConfigurationFromUnstructured(existing),
		client.FieldOwner("kubectl"), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}
	h.reconcile("ns1", "c1")

	if n := h.ops.count("POST c1"); n != 0 {
		t.Fatalf("POST %d times while adopting, want 0", n)
	}
	if h.secretValue("ns1", "c1", "clientSecret") != "kept-secret" || h.secretValue("ns1", "c1", "platformCA") != "PLATFORM-CA" {
		t.Fatalf("secret: %v", h.secret("ns1", "c1").Object)
	}
	ac := h.addonConfig("ns1", "c1")
	if hasCredentials(ac) {
		t.Fatalf("credentials left in the AddonConfig: %v", ac.Object["spec"])
	}
	// Moved, not issued: resyncs write nothing more.
	rv := h.secret("ns1", "c1").GetResourceVersion()
	acRV := ac.GetResourceVersion()
	h.reconcile("ns1", "c1")
	if h.secret("ns1", "c1").GetResourceVersion() != rv || h.addonConfig("ns1", "c1").GetResourceVersion() != acRV {
		t.Fatal("written again on resync")
	}
}

func TestLegacyAddonConfigMirrorsTheSecret(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.r.LegacyAddonConfig = true
	h.reconcile("ns1", "c1")

	ac := h.addonConfig("ns1", "c1")
	if helmValue(ac, "vcfOps", "clientSecret") != "secret-c1-1" || helmValue(ac, "clusterName") != "c1" ||
		helmValue(ac, "aggregator", "podAnnotations", secretHashAnnotation) != secretHash("secret-c1-1") {
		t.Fatalf("values: %v", ac.Object["spec"])
	}
	if h.secretValue("ns1", "c1", "clientSecret") != "secret-c1-1" {
		t.Fatal("no Secret in legacy mode")
	}
	rv := ac.GetResourceVersion()
	h.reconcile("ns1", "c1")
	if h.addonConfig("ns1", "c1").GetResourceVersion() != rv {
		t.Fatal("AddonConfig written on resync")
	}

	// Turning legacy mode off removes the credentials from the AddonConfig
	// and keeps the Secret.
	h.r.LegacyAddonConfig = false
	h.reconcile("ns1", "c1")
	if hasCredentials(h.addonConfig("ns1", "c1")) {
		t.Fatal("credentials left in the AddonConfig")
	}
	if h.secretValue("ns1", "c1", "clientSecret") != "secret-c1-1" || h.ops.count("POST c1") != 1 {
		t.Fatal("Secret changed or secret re-issued")
	}
}

func TestRotateAnnotationIssuesOneNewSecret(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")

	c := h.cluster("ns1", "c1")
	c.SetAnnotations(map[string]string{rotateAnnotation: "now"})
	if err := h.client.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	h.reconcile("ns1", "c1")
	h.reconcile("ns1", "c1")

	if n := h.ops.count("POST c1"); n != 2 {
		t.Fatalf("POST %d times, want 2", n)
	}
	if got := h.secretValue("ns1", "c1", "clientSecret"); got != "secret-c1-2" {
		t.Fatalf("clientSecret %q", got)
	}
	if _, ok := h.cluster("ns1", "c1").GetAnnotations()[rotateAnnotation]; ok {
		t.Fatal("rotate annotation still set")
	}
}

func TestOffboardKeepsOtherOwnersKeys(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.r.LegacyAddonConfig = true
	h.reconcile("ns1", "c1")

	// Argo CD owns a key in the same AddonConfig.
	argo := newAddonConfig()
	argo.SetNamespace("ns1")
	argo.SetName("c1-platform-observability")
	argo.Object["spec"] = map[string]any{"values": map[string]any{"helmValues": map[string]any{
		"aggregator": map[string]any{"replicaCount": int64(2)},
	}}}
	if err := h.client.Apply(context.Background(), client.ApplyConfigurationFromUnstructured(argo),
		client.FieldOwner("argocd-controller")); err != nil {
		t.Fatal(err)
	}

	c := h.cluster("ns1", "c1")
	c.SetLabels(nil)
	if err := h.client.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	h.reconcile("ns1", "c1")

	if n := h.ops.count("DELETE c1"); n != 1 {
		t.Fatalf("DELETE %d times, want 1", n)
	}
	if controllerutil.ContainsFinalizer(h.cluster("ns1", "c1"), finalizer) {
		t.Fatal("finalizer left after offboarding")
	}
	ac := h.addonConfig("ns1", "c1")
	if _, found, _ := unstructured.NestedMap(ac.Object, "spec", "values", "helmValues", "vcfOps"); found {
		t.Fatalf("vcfOps left after offboarding: %v", ac.Object["spec"])
	}
	if n, _, _ := unstructured.NestedInt64(ac.Object, "spec", "values", "helmValues", "aggregator", "replicaCount"); n != 2 {
		t.Fatalf("Argo CD's key lost: %v", ac.Object["spec"])
	}
	if h.secret("ns1", "c1") != nil {
		t.Fatal("credentials Secret left after offboarding")
	}
}

func TestDuplicateNameIsNotOnboarded(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil), cluster("ns2", "c1", true, nil))
	h.reconcile("ns1", "c1")
	h.events()
	h.reconcile("ns2", "c1")

	if n := h.ops.count("POST c1"); n != 1 {
		t.Fatalf("POST %d times, want 1: the duplicate rotated the original's secret", n)
	}
	if controllerutil.ContainsFinalizer(h.cluster("ns2", "c1"), finalizer) {
		t.Fatal("duplicate got the finalizer, so offboarding it would offboard the original")
	}
	if ev := h.events(); len(ev) != 1 || !strings.Contains(ev[0], "DuplicateClusterName") {
		t.Fatalf("events: %v", ev)
	}

	// Removing the duplicate's label must not offboard the original.
	c := h.cluster("ns2", "c1")
	c.SetLabels(nil)
	if err := h.client.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	h.reconcile("ns2", "c1")
	if n := h.ops.count("DELETE c1"); n != 0 {
		t.Fatalf("DELETE %d times for a never-onboarded duplicate", n)
	}
}

func TestSkipOffboardAnnotation(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")

	c := h.cluster("ns1", "c1")
	c.SetLabels(nil)
	c.SetAnnotations(map[string]string{skipOffboardAnnotation: "ops is gone"})
	if err := h.client.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	h.reconcile("ns1", "c1")
	if n := h.ops.count("DELETE c1"); n != 0 {
		t.Fatalf("DELETE %d times with skip-offboard", n)
	}
	if controllerutil.ContainsFinalizer(h.cluster("ns1", "c1"), finalizer) {
		t.Fatal("finalizer left with skip-offboard")
	}
}

func TestUnlabelledClusterIsIgnored(t *testing.T) {
	h := newHarness(t, cluster("ns1", "c1", false, nil))
	h.reconcile("ns1", "c1")
	if h.ops.count("POST c1")+h.ops.count("DELETE c1") != 0 {
		t.Fatal("Ops called for an unlabelled cluster")
	}
}
