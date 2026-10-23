package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpsServer answers the four calls the onboarder makes, the way VCF
// Operations does, and counts them.
type fakeOpsServer struct {
	mu       sync.Mutex
	calls    map[string]int
	secrets  map[string]int // cluster -> how many secrets issued
	clusters map[string]bool
	// expireNext makes the next configurations call answer 401 once.
	expireNext bool
	// failDelete makes every DELETE answer 500, even for a listed cluster.
	failDelete bool
}

func newFakeOps(t *testing.T) (*fakeOpsServer, *opsClient) {
	f := &fakeOpsServer{calls: map[string]int{}, secrets: map[string]int{}, clusters: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, newOpsClient(srv.URL, "platform-observability-onboard", "pw", srv.Client(), time.Minute)
}

func (f *fakeOpsServer) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeOpsServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/suite-api/api/auth/token/acquire" {
		f.calls["login"]++
		_ = json.NewEncoder(w).Encode(map[string]string{"token": fmt.Sprintf("tok-%d", f.calls["login"])})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "OpsToken tok-") ||
		r.Header.Get("X-vRealizeOps-API-use-unsupported") != "true" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.expireNext {
		f.expireNext = false
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == configurationsPath:
		c := r.URL.Query().Get("clusterName")
		f.calls["POST "+c]++
		f.secrets[c]++
		f.clusters[c] = true
		id := "vcf-obs-data-platform/da99/svc-vodap-app0-" + c
		_ = json.NewEncoder(w).Encode(map[string]any{
			"values-aggregator.yaml": "...",
			"commands": []string{
				"# 4. Upsert VCF services runtime Platform OAuth TLS CA\necho ...",
				fmt.Sprintf("# 5. Upsert VCF services runtime Platform OAuth Client Credentials\nkubectl create secret generic otel-gateway-oauth-creds --from-literal=client-id=%q --from-literal=client-secret=\"secret-%s-%d\" -n vcf-monitoring", id, c, f.secrets[c]),
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == configurationsPath:
		f.calls["GET"]++
		var out []map[string]string
		for c := range f.clusters {
			out = append(out, map[string]string{
				"clusterName": c, "saClientId": "id-" + c,
				"vmspInstanceFqdn": "shared01.example", "vmspPlatformFqdn": "vsp01.example",
				"vmspInstanceCertificate": "INSTANCE-CA", "vmspPlatformCertificate": "PLATFORM-CA",
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, configurationsPath+"/"):
		c := strings.TrimPrefix(r.URL.Path, configurationsPath+"/")
		f.calls["DELETE "+c]++
		// As the real Ops does for a cluster it doesn't list.
		if !f.clusters[c] || f.failDelete {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"Internal Server error, cause unknown."}`))
			return
		}
		delete(f.clusters, c)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestOpsOnboardParsesCredentials(t *testing.T) {
	f, ops := newFakeOps(t)
	creds, err := ops.Onboard(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if creds.ClientID != "vcf-obs-data-platform/da99/svc-vodap-app0-c1" || creds.ClientSecret != "secret-c1-1" {
		t.Fatalf("credentials: %+v", creds)
	}
	if f.count("login") != 1 {
		t.Fatalf("logins: %d", f.count("login"))
	}
}

func TestOpsEndpointsAreCachedAcrossClusters(t *testing.T) {
	f, ops := newFakeOps(t)
	ctx := context.Background()
	for _, c := range []string{"a", "b", "c"} {
		if _, err := ops.Onboard(ctx, c); err != nil {
			t.Fatal(err)
		}
		ep, err := ops.Endpoints(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ep != (Endpoints{"shared01.example", "vsp01.example", "INSTANCE-CA", "PLATFORM-CA"}) {
			t.Fatalf("endpoints: %+v", ep)
		}
	}
	if f.count("GET") != 1 {
		t.Fatalf("GET configurations %d times for 3 clusters, want 1", f.count("GET"))
	}
}

func TestOpsLogsInAgainOnExpiredToken(t *testing.T) {
	f, ops := newFakeOps(t)
	ctx := context.Background()
	if _, err := ops.Onboard(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.expireNext = true
	f.mu.Unlock()
	if err := ops.Offboard(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if f.count("login") != 2 || f.count("DELETE c1") != 1 {
		t.Fatalf("logins %d, deletes %d", f.count("login"), f.count("DELETE c1"))
	}
}

func TestParseOnboardWithoutSecretFails(t *testing.T) {
	if _, err := parseOnboard([]byte(`{"commands":["helm repo add x y"]}`)); err == nil {
		t.Fatal("want an error when step 5 is missing")
	}
}

// Ops answers 500 to a DELETE for a cluster it doesn't list. That is
// offboarded, not an error, or the finalizer never comes off.
func TestOpsOffboardOfAnUnlistedClusterSucceeds(t *testing.T) {
	f, ops := newFakeOps(t)
	if _, err := ops.Onboard(context.Background(), "other"); err != nil {
		t.Fatal(err)
	}
	if err := ops.Offboard(context.Background(), "gone"); err != nil {
		t.Fatalf("offboard of an unlisted cluster: %v", err)
	}
	if f.count("DELETE gone") != 1 || f.count("GET") != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
}

// A failed DELETE for a cluster Ops still lists stays an error.
func TestOpsOffboardFailureForAListedClusterIsAnError(t *testing.T) {
	f, ops := newFakeOps(t)
	if _, err := ops.Onboard(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failDelete = true
	f.mu.Unlock()
	if err := ops.Offboard(context.Background(), "c1"); err == nil {
		t.Fatal("want an error while Ops still lists the cluster")
	}
}
