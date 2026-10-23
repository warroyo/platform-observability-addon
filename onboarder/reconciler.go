package main

// The reconciler onboards labelled VKS clusters to VCF Operations Platform
// Observability and keeps the cluster's OAuth client in the Secret
// platform-observability-<cluster>, in the cluster's namespace. The addon's
// AddonConfigDefinition (chart 0.2.0 and later) reads that Secret as a
// template input, so the credentials never sit in an AddonConfig.
//
// Chart 0.1.0 read them from the AddonConfig instead. For clusters still on
// it, --legacy-addon-config also writes these AddonConfig keys, through
// server-side apply so Argo CD (or anyone) can own the rest:
//
//	spec.values.helmValues.clusterName
//	spec.values.helmValues.vcfOps.*
//	spec.values.helmValues.aggregator.podAnnotations["platform-observability/secret-hash"]
//
// Without it, the reconciler adopts credentials it finds there into the
// Secret, then removes them from the AddonConfig.
//
// A POST to Ops issues a new secret and revokes the old one at once, and the
// collectors only pick a new secret up on helm-controller's next interval (up
// to 10 minutes). So the reconciler POSTs only when neither the Secret nor the
// AddonConfig has a secret yet, or the Cluster asks for a rotation, and it
// decides that from live reads, never from a cache: a stale cache would POST
// again right after a successful onboarding and cut the cluster off. Secrets
// are never cached at all: the Supervisor refuses a cluster-wide Secret watch
// to a third-party service account.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// Set on a Cluster once the reconciler has onboarded it, and removed after
	// offboarding. Only a cluster carrying it is ever offboarded, so a
	// duplicate name that was never onboarded can't offboard the original.
	finalizer = "platform-observability/offboard"
	// Any value on a Cluster asks for a new secret. Removed when done.
	rotateAnnotation = "platform-observability/rotate"
	// Any value on a Cluster skips the Ops call when offboarding, so a cluster
	// can still be deleted when VCF Operations is gone for good.
	skipOffboardAnnotation = "platform-observability/skip-offboard"
	// On the credentials Secret: the rotate request its secret answers,
	// written with the secret, so a rotation is never repeated when clearing
	// the Cluster's annotation fails.
	rotatedForAnnotation = "platform-observability/rotated-for"
	// On the credentials Secret, and from there on the aggregator pods, so a
	// new secret rolls them. Also the legacy AddonConfig's pod annotation.
	secretHashAnnotation = "platform-observability/secret-hash"
	// The addon system garbage-collects the AddonConfig with the ClusterAddon.
	ownedForDeletionAnnotation = "clusteraddon.addons.kubernetes.vmware.com/owned-for-deletion"
	managedByLabel             = "app.kubernetes.io/managed-by"
)

var (
	clusterGVK     = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	addonConfigGVK = schema.GroupVersionKind{Group: "addons.kubernetes.vmware.com", Version: "v1alpha1", Kind: "AddonConfig"}
	secretGVK      = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
)

// The credentials Secret's keys. The AddonConfigDefinition in
// chart/acd/platform-observability.yaml reads these names.
const (
	keyClientID     = "clientId"
	keyClientSecret = "clientSecret"
	keyInstanceFqdn = "instanceFqdn"
	keyPlatformFqdn = "platformFqdn"
	keyInstanceCA   = "instanceCA"
	keyPlatformCA   = "platformCA"
)

type Reconciler struct {
	// Cached client: Cluster reads for event handling, and all writes.
	Client client.Client
	// Live reads, for every decision that leads to a POST, and every Secret
	// read.
	APIReader    client.Reader
	Ops          OpsAPI
	Recorder     events.EventRecorder
	AddonName    string
	FieldManager string
	// Also write the credentials into the AddonConfig, for clusters still on
	// chart 0.1.0. Off: remove them from it.
	LegacyAddonConfig bool

	// Ops keys a cluster on its bare name, so two clusters with the same name
	// in different namespaces share one OAuth client. Reconciles of the same
	// name are serialized so the duplicate check can't race.
	nameLocks sync.Map
	// Credentials issued by a POST whose Secret write has not succeeded yet,
	// by cluster: a retry writes these instead of issuing another secret.
	pending sync.Map
}

func (r *Reconciler) enabledLabel() string { return "addons.kubernetes.vmware.com/" + r.AddonName }

func (r *Reconciler) addonConfigName(cluster string) string { return cluster + "-" + r.AddonName }

func (r *Reconciler) secretName(cluster string) string { return r.AddonName + "-" + cluster }

func (r *Reconciler) lockName(name string) func() {
	m, _ := r.nameLocks.LoadOrStore(name, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func newCluster() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(clusterGVK)
	return u
}

func newAddonConfig() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(addonConfigGVK)
	return u
}

func newSecret() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(secretGVK)
	return u
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	unlock := r.lockName(req.Name)
	defer unlock()

	cluster := newCluster()
	if err := r.APIReader.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	enabled := cluster.GetLabels()[r.enabledLabel()] == "enabled"
	if cluster.GetDeletionTimestamp() != nil || !enabled {
		return ctrl.Result{}, r.offboard(ctx, cluster)
	}
	return ctrl.Result{}, r.onboard(ctx, cluster)
}

// pendingIssue is a secret a POST issued whose Secret write has not been
// confirmed. Keyed by Cluster UID, so a cluster deleted and re-created with the
// same name never inherits one.
type pendingIssue struct {
	values opsValues
	// The rotate request it answers; rotation false for a first onboarding.
	request  string
	rotation bool
}

// onboard handles a labelled cluster, in a fixed order. Every step that
// could lead to a POST reads live state first.
func (r *Reconciler) onboard(ctx context.Context, cluster *unstructured.Unstructured) error {
	rotateValue, rotate := cluster.GetAnnotations()[rotateAnnotation]

	cur, err := r.currentValues(ctx, cluster)
	if err != nil {
		return err
	}

	// 1. A secret already issued wins over everything: Ops has revoked
	// whatever the Secret had before it.
	if v, ok := r.pending.Load(cluster.GetUID()); ok {
		return r.settlePending(ctx, cluster, cur, v.(pendingIssue))
	}

	// 2. A rotation whose secret is written but whose request was not cleared.
	if rotate && cur.hasMarker && cur.rotatedFor == rotateValue {
		return r.clearRotate(ctx, cluster, rotateValue)
	}
	// 3. A finished rotation's marker goes, so the same value can ask again.
	if !rotate && cur.hasMarker && cur.values.ClientSecret != "" {
		return r.writeValues(ctx, cluster, cur, cur.values, nil)
	}

	// 4. Only the cluster holding the finalizer owns the name in Ops. Check
	// before anything that would hand this cluster the finalizer.
	if !controllerutil.ContainsFinalizer(cluster, finalizer) {
		if dup, err := r.onboardedElsewhere(ctx, cluster); err != nil {
			return err
		} else if dup != "" {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "DuplicateClusterName", "Onboard",
				"cluster %s in namespace %s holds this name in VCF Operations, which keys clusters by name alone; not onboarding", cluster.GetName(), dup)
			return nil
		}
	}

	// 5. Already onboarded, by us, by an earlier version into the AddonConfig,
	// or by scripts/onboard.sh: keep the same values. No POST, so nothing
	// rotates. A resync over hundreds of clusters writes nothing once they
	// are ours.
	if cur.values.ClientSecret != "" && !rotate {
		if cur.values.ClientID == "" {
			r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "IncompleteCredentials", "Onboard",
				"the credentials have a clientSecret but no clientId; annotate the cluster %s to issue a new secret", rotateAnnotation)
			return nil
		}
		if err := r.ensureFinalizer(ctx, cluster); err != nil {
			return err
		}
		v := cur.values
		if !cur.secretOwned && (v.InstanceFqdn == "" || v.PlatformFqdn == "" || v.InstanceCA == "" || v.PlatformCA == "") {
			ep, err := r.Ops.Endpoints(ctx)
			if err != nil {
				return fmt.Errorf("endpoints for %s: %w", cluster.GetName(), err)
			}
			v.Endpoints = ep
		}
		return r.writeValues(ctx, cluster, cur, v, nil)
	}

	// 6. Issue a secret. The finalizer goes on first, so a cluster deleted
	// mid-onboarding is still offboarded.
	if err := r.ensureFinalizer(ctx, cluster); err != nil {
		return err
	}
	// Endpoints first: if Ops can't serve them, fail before issuing a secret.
	// The exception is an Ops with no cluster onboarded yet, which lists
	// endpoints only after the first POST.
	ep, epErr := r.Ops.Endpoints(ctx)
	if epErr != nil && !errors.Is(epErr, errNoEndpoints) {
		return fmt.Errorf("endpoints for %s: %w", cluster.GetName(), epErr)
	}
	creds, err := r.Ops.Onboard(ctx, cluster.GetName())
	if err != nil {
		return fmt.Errorf("onboard %s: %w", cluster.GetName(), err)
	}
	issued := pendingIssue{values: opsValues{Credentials: creds, Endpoints: ep}, request: rotateValue, rotation: rotate}
	// From here a failure must not lead to another POST: the secret is kept and
	// written by the next attempt. Only a crash before the write loses it.
	r.pending.Store(cluster.GetUID(), issued)
	if epErr != nil {
		if issued.values.Endpoints, err = r.Ops.Endpoints(ctx); err != nil {
			return fmt.Errorf("endpoints for %s: %w", cluster.GetName(), err)
		}
		r.pending.Store(cluster.GetUID(), issued)
	}
	return r.settlePending(ctx, cluster, cur, issued)
}

// settlePending writes an issued secret unless a live read shows it is already
// written (a write that succeeded but reported an error), then forgets it and
// finishes the rotation request it answers.
func (r *Reconciler) settlePending(ctx context.Context, cluster *unstructured.Unstructured, cur current, p pendingIssue) error {
	if p.values.InstanceFqdn == "" {
		ep, err := r.Ops.Endpoints(ctx)
		if err != nil {
			return fmt.Errorf("endpoints for %s: %w", cluster.GetName(), err)
		}
		p.values.Endpoints = ep
		r.pending.Store(cluster.GetUID(), p)
	}
	var marker *string
	if p.rotation {
		marker = &p.request
	}
	if !cur.secretExists || cur.values.ClientSecret != p.values.ClientSecret {
		cur.secretOwned = false
	}
	if err := r.writeValues(ctx, cluster, cur, p.values, marker); err != nil {
		return err
	}
	r.pending.Delete(cluster.GetUID())

	reason, verb := "Onboarded", "onboarded to"
	if p.rotation {
		reason, verb = "SecretRotated", "issued a new secret by"
		if value, ok := cluster.GetAnnotations()[rotateAnnotation]; ok && value == p.request {
			if err := r.clearRotate(ctx, cluster, value); err != nil {
				return err
			}
		}
	}
	log.FromContext(ctx).Info(reason, "cluster", client.ObjectKeyFromObject(cluster).String())
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, reason, "Onboard",
		"%s VCF Operations Platform Observability as client %s", verb, p.values.ClientID)
	return nil
}

func (r *Reconciler) offboard(ctx context.Context, cluster *unstructured.Unstructured) error {
	if !controllerutil.ContainsFinalizer(cluster, finalizer) {
		return nil
	}
	r.pending.Delete(cluster.GetUID())
	// Credentials out of the Secret and the AddonConfig before the client
	// goes in Ops: if the cluster is labelled again in between, it finds no
	// secret and onboards again, instead of adopting one Ops has already
	// deleted.
	if err := r.deleteSecret(ctx, cluster); err != nil {
		return err
	}
	if err := r.removeAddonConfigValues(ctx, cluster); err != nil {
		return err
	}
	if _, skip := cluster.GetAnnotations()[skipOffboardAnnotation]; !skip {
		if err := r.Ops.Offboard(ctx, cluster.GetName()); err != nil {
			return fmt.Errorf("offboard %s: %w", cluster.GetName(), err)
		}
	}

	orig := cluster.DeepCopy()
	controllerutil.RemoveFinalizer(cluster, finalizer)
	if err := r.Client.Patch(ctx, cluster, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return client.IgnoreNotFound(err)
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "Offboarded", "Offboard",
		"removed from VCF Operations Platform Observability")
	return nil
}

func (r *Reconciler) deleteSecret(ctx context.Context, cluster *unstructured.Unstructured) error {
	s := newSecret()
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: cluster.GetNamespace(), Name: r.secretName(cluster.GetName())}, s); err != nil {
		return client.IgnoreNotFound(err)
	}
	uid := s.GetUID()
	if err := r.Client.Delete(ctx, s, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete Secret %s/%s: %w", s.GetNamespace(), s.GetName(), err)
	}
	return nil
}

// removeAddonConfigValues drops the credentials from the AddonConfig. An apply
// with none of our fields releases what only we own; values another manager
// also owns (an adopted onboard.sh AddonConfig) are then removed by path,
// leaving every other key alone. It writes nothing when there is nothing of
// ours or of the credentials there.
func (r *Reconciler) removeAddonConfigValues(ctx context.Context, cluster *unstructured.Unstructured) error {
	key := types.NamespacedName{Namespace: cluster.GetNamespace(), Name: r.addonConfigName(cluster.GetName())}
	ac := newAddonConfig()
	if err := r.APIReader.Get(ctx, key, ac); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !r.appliedBy(ac) && !hasCredentials(ac) {
		return nil
	}
	empty := newAddonConfig()
	empty.SetNamespace(key.Namespace)
	empty.SetName(key.Name)
	// The AddonConfig is often garbage collected right now, with its
	// ClusterAddon, when the cluster stops matching the AddonInstall. An apply
	// creates a missing object (resourceVersion does not stop it), so one that
	// lands after the collection either fails or makes an empty AddonConfig,
	// told apart from the one read by its UID, and deleted again.
	uid := ac.GetUID()
	applyErr := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(empty),
		client.FieldOwner(r.FieldManager), client.ForceOwnership)
	if err := r.APIReader.Get(ctx, key, ac); err != nil {
		return client.IgnoreNotFound(err)
	}
	if created := ac.GetUID(); created != uid {
		err := r.Client.Delete(ctx, ac, client.Preconditions{UID: &created})
		return client.IgnoreNotFound(err)
	}
	if applyErr != nil {
		return fmt.Errorf("release AddonConfig %s: %w", key, applyErr)
	}
	var ops []string
	if _, found, _ := unstructured.NestedFieldNoCopy(ac.Object, "spec", "values", "helmValues", "vcfOps"); found {
		ops = append(ops, `{"op":"remove","path":"/spec/values/helmValues/vcfOps"}`)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(ac.Object, "spec", "values", "helmValues", "aggregator", "podAnnotations", secretHashAnnotation); found {
		ops = append(ops, `{"op":"remove","path":"/spec/values/helmValues/aggregator/podAnnotations/platform-observability~1secret-hash"}`)
	}
	if len(ops) == 0 {
		return nil
	}
	patch := client.RawPatch(types.JSONPatchType, []byte("["+strings.Join(ops, ",")+"]"))
	if err := r.Client.Patch(ctx, ac, patch); err != nil {
		return fmt.Errorf("remove credentials from AddonConfig %s: %w", key, err)
	}
	return nil
}

func hasCredentials(ac *unstructured.Unstructured) bool {
	_, ops, _ := unstructured.NestedFieldNoCopy(ac.Object, "spec", "values", "helmValues", "vcfOps")
	_, hash, _ := unstructured.NestedFieldNoCopy(ac.Object, "spec", "values", "helmValues", "aggregator", "podAnnotations", secretHashAnnotation)
	return ops || hash
}

// appliedBy: our field manager has applied to this object.
func (r *Reconciler) appliedBy(u *unstructured.Unstructured) bool {
	for _, mf := range u.GetManagedFields() {
		if mf.Manager == r.FieldManager && mf.Operation == "Apply" {
			return true
		}
	}
	return false
}

// opsValues is one cluster's credentials and the shared endpoints.
type opsValues struct {
	Credentials
	Endpoints
}

func (v opsValues) complete() bool {
	return v.ClientID != "" && v.ClientSecret != "" &&
		v.InstanceFqdn != "" && v.PlatformFqdn != "" && v.InstanceCA != "" && v.PlatformCA != ""
}

// current is what the cluster's Secret and AddonConfig hold now.
type current struct {
	// The credentials and endpoints, from the Secret if it exists, else from
	// the AddonConfig; possibly none.
	values       opsValues
	secretExists bool
	// The Secret already holds exactly what writeSecret would write, under
	// our field manager.
	secretOwned bool
	// The rotate request whose new secret the Secret carries, while the
	// rotation is finishing.
	rotatedFor string
	hasMarker  bool
	// The AddonConfig: whether it holds credentials (or an earlier version's
	// rotation marker), and whether it holds exactly what writeAddonConfig would write.
	addonConfigTouched bool
	addonConfigOwned   bool
}

// currentValues reads the cluster's Secret and AddonConfig live.
func (r *Reconciler) currentValues(ctx context.Context, cluster *unstructured.Unstructured) (current, error) {
	var cur current

	s := newSecret()
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: cluster.GetNamespace(), Name: r.secretName(cluster.GetName())}, s)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return current{}, err
	default:
		data, _, _ := unstructured.NestedStringMap(s.Object, "data")
		get := func(k string) string {
			b, _ := base64.StdEncoding.DecodeString(data[k])
			return string(b)
		}
		cur.secretExists = true
		cur.values = opsValues{
			Credentials: Credentials{ClientID: get(keyClientID), ClientSecret: get(keyClientSecret)},
			Endpoints: Endpoints{
				InstanceFqdn: get(keyInstanceFqdn), PlatformFqdn: get(keyPlatformFqdn),
				InstanceCA: get(keyInstanceCA), PlatformCA: get(keyPlatformCA),
			},
		}
		cur.rotatedFor, cur.hasMarker = s.GetAnnotations()[rotatedForAnnotation]
		cur.secretOwned = cur.values.complete() && r.appliedBy(s) &&
			s.GetAnnotations()[secretHashAnnotation] == secretHash(cur.values.ClientSecret)
	}

	ac := newAddonConfig()
	err = r.APIReader.Get(ctx, types.NamespacedName{Namespace: cluster.GetNamespace(), Name: r.addonConfigName(cluster.GetName())}, ac)
	if apierrors.IsNotFound(err) {
		return cur, nil
	}
	if err != nil {
		return current{}, err
	}
	get := func(field string) string {
		s, _, _ := unstructured.NestedString(ac.Object, "spec", "values", "helmValues", "vcfOps", field)
		return s
	}
	legacy := opsValues{
		Credentials: Credentials{ClientID: get("clientId"), ClientSecret: get("clientSecret")},
		Endpoints: Endpoints{
			InstanceFqdn: get("instanceFqdn"), PlatformFqdn: get("platformFqdn"),
			InstanceCA: get("instanceCA"), PlatformCA: get("platformCA"),
		},
	}
	if !cur.secretExists {
		cur.values = legacy
	}
	_, legacyMarker := ac.GetAnnotations()[rotatedForAnnotation]
	cur.addonConfigTouched = hasCredentials(ac) || legacyMarker
	cur.addonConfigOwned = r.addonConfigAsWanted(ac, cluster.GetName(), legacy) && legacy == cur.values
	return cur, nil
}

// addonConfigAsWanted: our field manager has applied to this AddonConfig, and
// the fields we write already hold what we would write.
func (r *Reconciler) addonConfigAsWanted(ac *unstructured.Unstructured, cluster string, v opsValues) bool {
	if !r.appliedBy(ac) || !v.complete() {
		return false
	}
	_, hasMarker := ac.GetAnnotations()[rotatedForAnnotation]
	name, _, _ := unstructured.NestedString(ac.Object, "spec", "values", "helmValues", "clusterName")
	hash, _, _ := unstructured.NestedString(ac.Object, "spec", "values", "helmValues", "aggregator", "podAnnotations", secretHashAnnotation)
	return name == cluster && hash == secretHash(v.ClientSecret) && !hasMarker &&
		ac.GetAnnotations()[ownedForDeletionAnnotation] == "true"
}

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:16]
}

// writeValues makes the Secret hold v, then brings the AddonConfig in line:
// the same values with --legacy-addon-config, none without. Each write is
// skipped when the live read shows it would change nothing, so a resync writes
// nothing.
// rotatedFor, when set, records which rotate request this secret answers, in
// the same write as the secret. nil drops a marker an earlier write set.
func (r *Reconciler) writeValues(ctx context.Context, cluster *unstructured.Unstructured, cur current, v opsValues, rotatedFor *string) error {
	if !cur.secretOwned || cur.hasMarker || rotatedFor != nil {
		if err := r.writeSecret(ctx, cluster, v, rotatedFor); err != nil {
			return err
		}
		cur.addonConfigOwned = false
	}
	switch {
	case r.LegacyAddonConfig && !cur.addonConfigOwned:
		return r.writeAddonConfig(ctx, cluster, v)
	case !r.LegacyAddonConfig && cur.addonConfigTouched:
		return r.removeAddonConfigValues(ctx, cluster)
	}
	return nil
}

// writeSecret server-side applies the whole credentials Secret, with force.
func (r *Reconciler) writeSecret(ctx context.Context, cluster *unstructured.Unstructured, v opsValues, rotatedFor *string) error {
	s := newSecret()
	s.SetNamespace(cluster.GetNamespace())
	s.SetName(r.secretName(cluster.GetName()))
	s.SetLabels(map[string]string{managedByLabel: r.FieldManager})
	annotations := map[string]string{secretHashAnnotation: secretHash(v.ClientSecret)}
	if rotatedFor != nil {
		annotations[rotatedForAnnotation] = *rotatedFor
	}
	s.SetAnnotations(annotations)
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	s.Object["type"] = string(corev1.SecretTypeOpaque)
	s.Object["data"] = map[string]any{
		keyClientID:     enc(v.ClientID),
		keyClientSecret: enc(v.ClientSecret),
		keyInstanceFqdn: enc(v.InstanceFqdn),
		keyPlatformFqdn: enc(v.PlatformFqdn),
		keyInstanceCA:   enc(v.InstanceCA),
		keyPlatformCA:   enc(v.PlatformCA),
	}
	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(s),
		client.FieldOwner(r.FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply Secret %s/%s: %w", s.GetNamespace(), s.GetName(), err)
	}
	return nil
}

// writeAddonConfig server-side applies our keys, and only ours, with force:
// the reconciler always takes its own keys back. For chart 0.1.0 only.
func (r *Reconciler) writeAddonConfig(ctx context.Context, cluster *unstructured.Unstructured, v opsValues) error {
	ac := newAddonConfig()
	ac.SetNamespace(cluster.GetNamespace())
	ac.SetName(r.addonConfigName(cluster.GetName()))
	ac.SetAnnotations(map[string]string{ownedForDeletionAnnotation: "true"})
	ac.Object["spec"] = map[string]any{
		"values": map[string]any{
			"helmValues": map[string]any{
				"clusterName": cluster.GetName(),
				"vcfOps": map[string]any{
					"instanceFqdn": v.InstanceFqdn,
					"platformFqdn": v.PlatformFqdn,
					"instanceCA":   v.InstanceCA,
					"platformCA":   v.PlatformCA,
					"clientId":     v.ClientID,
					"clientSecret": v.ClientSecret,
				},
				"aggregator": map[string]any{
					"podAnnotations": map[string]any{secretHashAnnotation: secretHash(v.ClientSecret)},
				},
			},
		},
	}
	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(ac),
		client.FieldOwner(r.FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply AddonConfig %s/%s: %w", ac.GetNamespace(), ac.GetName(), err)
	}
	return nil
}

// onboardedElsewhere returns the namespace of another Cluster with the same
// name that holds it in Ops (carries the finalizer), or "". That includes a
// cluster whose label is gone but whose offboarding has not finished: its
// DELETE would revoke a new cluster's secret. Live list: it runs only for a
// cluster that has no finalizer yet.
func (r *Reconciler) onboardedElsewhere(ctx context.Context, cluster *unstructured.Unstructured) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterGVK.GroupVersion().WithKind("ClusterList"))
	// Only clusters with this name: a field selector, so onboarding hundreds
	// of clusters doesn't read every Cluster once per cluster.
	if err := r.APIReader.List(ctx, list, client.MatchingFields{"metadata.name": cluster.GetName()}); err != nil {
		return "", err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.GetName() == cluster.GetName() && other.GetNamespace() != cluster.GetNamespace() &&
			controllerutil.ContainsFinalizer(other, finalizer) {
			return other.GetNamespace(), nil
		}
	}
	return "", nil
}

func (r *Reconciler) ensureFinalizer(ctx context.Context, cluster *unstructured.Unstructured) error {
	if controllerutil.ContainsFinalizer(cluster, finalizer) {
		return nil
	}
	orig := cluster.DeepCopy()
	controllerutil.AddFinalizer(cluster, finalizer)
	return r.Client.Patch(ctx, cluster, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// clearRotate removes the rotate annotation only if it still holds the request
// just answered: a newer request set meanwhile survives, and is handled next.
func (r *Reconciler) clearRotate(ctx context.Context, cluster *unstructured.Unstructured, answered string) error {
	value, _ := json.Marshal(answered)
	path := "/metadata/annotations/" + strings.ReplaceAll(rotateAnnotation, "/", "~1")
	patch := client.RawPatch(types.JSONPatchType, []byte(fmt.Sprintf(
		`[{"op":"test","path":%q,"value":%s},{"op":"remove","path":%q}]`, path, value, path)))
	err := r.Client.Patch(ctx, cluster, patch)
	if err == nil {
		return nil
	}
	// The test failed if the request changed or went away: not an error.
	now := newCluster()
	if getErr := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cluster), now); getErr != nil {
		return client.IgnoreNotFound(getErr)
	}
	if v, ok := now.GetAnnotations()[rotateAnnotation]; !ok || v != answered {
		return nil
	}
	return err
}
