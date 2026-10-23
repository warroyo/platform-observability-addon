// platform-observability-onboarder: a Supervisor Service controller that
// onboards VKS clusters labelled addons.kubernetes.vmware.com/<addon>=enabled
// to VCF Operations Platform Observability. See reconciler.go for what it
// writes, and the README for how it is installed.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		addonName     = flag.String("addon-name", "platform-observability", "the addon's name: the cluster label is addons.kubernetes.vmware.com/<addon-name> and the AddonConfig <cluster>-<addon-name>")
		fieldManager  = flag.String("field-manager", "platform-observability-onboarder", "the server-side apply field manager for the credentials Secrets and the AddonConfig keys this controller owns")
		legacyConfig  = flag.Bool("legacy-addon-config", false, "also write the credentials into each cluster's AddonConfig, for clusters still on chart 0.1.0; without it they are removed from there")
		syncPeriod    = flag.Duration("sync-period", 10*time.Minute, "how often every cluster is reconciled again; a resync makes no Ops call")
		maxConcurrent = flag.Int("max-concurrent", 4, "reconciles at a time; bounds the POSTs to Ops when many clusters are labelled at once")
		endpointsTTL  = flag.Duration("endpoints-ttl", 10*time.Minute, "how long one GET configurations serves the endpoints for every cluster")
		leaderElect   = flag.Bool("leader-elect", true, "use leader election, so only one replica talks to Ops")
		probeAddr     = flag.String("health-probe-bind-address", ":8081", "health probe address")
	)
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	opsURL, opsUser, opsPassword := os.Getenv("OPS_URL"), env("OPS_USERNAME", "platform-observability-onboard"), os.Getenv("OPS_PASSWORD")
	if opsURL == "" || opsPassword == "" {
		setupLog.Error(fmt.Errorf("OPS_URL and OPS_PASSWORD are required"), "configuration")
		os.Exit(1)
	}
	httpClient, err := opsHTTPClient(os.Getenv("OPS_CA_FILE"), env("OPS_INSECURE", "false") == "true")
	if err != nil {
		setupLog.Error(err, "Ops TLS")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Cache: cache.Options{
			SyncPeriod: syncPeriod,
			// Cluster objects are large; the managed fields are most of that,
			// and nothing here reads them from the cache.
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "platform-observability-onboarder",
	})
	if err != nil {
		setupLog.Error(err, "manager")
		os.Exit(1)
	}

	r := &Reconciler{
		Client:       mgr.GetClient(),
		APIReader:    mgr.GetAPIReader(),
		Ops:          newOpsClient(opsURL, opsUser, opsPassword, httpClient, *endpointsTTL),
		Recorder:     mgr.GetEventRecorder("platform-observability-onboarder"),
		AddonName:    *addonName,
		FieldManager: *fieldManager,

		LegacyAddonConfig: *legacyConfig,
	}

	// Clusters: the labelled ones, and any that still carry our finalizer (the
	// label was removed, or the cluster is being deleted) so they get
	// offboarded. All Clusters are cached; the predicate only drops events.
	relevant := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[r.enabledLabel()] == "enabled" || controllerutil.ContainsFinalizer(o, finalizer)
	})
	suffix := "-" + *addonName
	// AddonConfigs, metadata only: a deleted or changed AddonConfig brings its
	// cluster back at once instead of at the next resync. Secrets are not
	// watched: the Supervisor refuses a cluster-wide Secret watch to a
	// third-party service account, so a deleted credentials Secret is only
	// noticed at the next resync.
	addonConfigToCluster := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []ctrl.Request {
		if !strings.HasSuffix(o.GetName(), suffix) {
			return nil
		}
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: strings.TrimSuffix(o.GetName(), suffix)}}}
	})
	addonConfigMeta := &metav1.PartialObjectMetadata{}
	addonConfigMeta.SetGroupVersionKind(addonConfigGVK)

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("platform-observability-onboarder").
		For(newCluster(), builder.WithPredicates(relevant)).
		Watches(addonConfigMeta, addonConfigToCluster, builder.OnlyMetadata).
		WithOptions(controller.Options{MaxConcurrentReconciles: *maxConcurrent}).
		Complete(r); err != nil {
		setupLog.Error(err, "controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "readyz")
		os.Exit(1)
	}

	setupLog.Info("starting", "addon", *addonName, "ops", opsURL, "opsUser", opsUser, "legacyAddonConfig", *legacyConfig)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager stopped")
		os.Exit(1)
	}
}

func opsHTTPClient(caFile string, insecure bool) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // opt-in, for labs with self-signed Ops certificates
	} else if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", caFile)
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: http.ProxyFromEnvironment}}, nil
}
