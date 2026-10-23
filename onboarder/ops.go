package main

// The VCF Operations side: the onboarding dialog's API under suite-api, with
// an Ops API token. See the README's "The VCF Operations API" for what each
// call does. The account needs only extensions.management.usage
// (scripts/ops-account.sh).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const configurationsPath = "/suite-api/internal/extension/vodap/app0/configurations"

// Credentials is what a POST configurations issues for one cluster. A new
// secret revokes the previous one at once.
type Credentials struct {
	ClientID     string
	ClientSecret string
}

// Endpoints are the VCF services runtime endpoints and CA chains. They are the
// same for every cluster on one Ops instance.
type Endpoints struct {
	InstanceFqdn string
	PlatformFqdn string
	InstanceCA   string
	PlatformCA   string
}

// OpsAPI is what the reconciler needs from VCF Operations.
type OpsAPI interface {
	// Onboard creates the cluster's OAuth client if needed and issues it a new
	// secret.
	Onboard(ctx context.Context, cluster string) (Credentials, error)
	// Endpoints returns the instance-wide endpoints, from a shared cache.
	Endpoints(ctx context.Context) (Endpoints, error)
	// Offboard removes the cluster from Ops and deletes its OAuth client.
	// A cluster Ops does not know is not an error.
	Offboard(ctx context.Context, cluster string) error
}

type opsClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
	// How long one GET configurations serves Endpoints. The GET lists every
	// onboarded cluster, so it is fetched once per TTL, not once per cluster.
	endpointsTTL time.Duration

	// Held while one GET refreshes the endpoints, so concurrent misses wait for
	// it instead of each sending its own.
	refreshMu  sync.Mutex
	mu         sync.Mutex
	token      string
	endpoints  *Endpoints
	endpointAt time.Time
	// A failed refresh is served to everyone for failureBackoff, so a failing
	// Ops gets one GET per window, not one per waiting reconcile.
	lastErr   error
	lastErrAt time.Time
}

func newOpsClient(baseURL, username, password string, httpClient *http.Client, endpointsTTL time.Duration) *opsClient {
	return &opsClient{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		username:     username,
		password:     password,
		http:         httpClient,
		endpointsTTL: endpointsTTL,
	}
}

var errUnauthorized = errors.New("unauthorized")

// errNoEndpoints: Ops lists no onboarded cluster yet, so it has nowhere to
// read the endpoints from. The first POST creates one.
var errNoEndpoints = errors.New("ops GET configurations: no entry with both endpoints and CA chains")

func (o *opsClient) acquireToken(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": o.username, "password": o.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/suite-api/api/auth/token/acquire", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ops login as %s: HTTP %d", o.username, resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("ops login: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("ops login: empty token")
	}
	return out.Token, nil
}

// do sends one configurations call, logging in first if needed and once more
// if the token has expired.
func (o *opsClient) do(ctx context.Context, method, path string) (int, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		o.mu.Lock()
		token := o.token
		o.mu.Unlock()
		if token == "" {
			t, err := o.acquireToken(ctx)
			if err != nil {
				return 0, nil, err
			}
			o.mu.Lock()
			o.token = t
			o.mu.Unlock()
			token = t
		}
		code, body, err := o.send(ctx, method, path, token)
		if errors.Is(err, errUnauthorized) {
			o.mu.Lock()
			if o.token == token {
				o.token = ""
			}
			o.mu.Unlock()
			continue
		}
		return code, body, err
	}
	return 0, nil, fmt.Errorf("ops %s %s: unauthorized after a new login", method, path)
}

func (o *opsClient) send(ctx context.Context, method, path, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "OpsToken "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-vRealizeOps-API-use-unsupported", "true")
	if method == http.MethodPost {
		req.ContentLength = 0
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, nil, errUnauthorized
	}
	return resp.StatusCode, body, nil
}

// The secret is only in the dialog's step 5, a kubectl command:
//
//	kubectl create secret generic otel-gateway-oauth-creds --from-literal=client-id="..." --from-literal=client-secret="..."
var (
	clientIDRe     = regexp.MustCompile(`client-id="([^"]+)"`)
	clientSecretRe = regexp.MustCompile(`client-secret="([^"]+)"`)
)

func (o *opsClient) Onboard(ctx context.Context, cluster string) (Credentials, error) {
	code, body, err := o.do(ctx, http.MethodPost, configurationsPath+"?clusterName="+url.QueryEscape(cluster))
	if err != nil {
		return Credentials{}, err
	}
	if code != http.StatusOK {
		return Credentials{}, fmt.Errorf("ops POST configurations for %s: HTTP %d", cluster, code)
	}
	return parseOnboard(body)
}

func parseOnboard(body []byte) (Credentials, error) {
	var out struct {
		Commands []string `json:"commands"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Credentials{}, fmt.Errorf("ops POST configurations: %w", err)
	}
	for _, c := range out.Commands {
		if !strings.Contains(c, "otel-gateway-oauth-creds") {
			continue
		}
		id, secret := clientIDRe.FindStringSubmatch(c), clientSecretRe.FindStringSubmatch(c)
		if id == nil || secret == nil {
			break
		}
		return Credentials{ClientID: id[1], ClientSecret: secret[1]}, nil
	}
	return Credentials{}, errors.New("ops POST configurations: no otel-gateway-oauth-creds command with a client ID and secret")
}

const failureBackoff = 5 * time.Second

func (o *opsClient) cachedEndpoints() (Endpoints, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.endpoints != nil && time.Since(o.endpointAt) < o.endpointsTTL {
		return *o.endpoints, true, nil
	}
	if o.lastErr != nil && time.Since(o.lastErrAt) < failureBackoff {
		return Endpoints{}, true, o.lastErr
	}
	return Endpoints{}, false, nil
}

// remember caches a failed refresh, except errNoEndpoints: right after the
// first POST on an empty Ops the next GET must go out at once.
func (o *opsClient) remember(err error) error {
	if !errors.Is(err, errNoEndpoints) {
		o.mu.Lock()
		o.lastErr, o.lastErrAt = err, time.Now()
		o.mu.Unlock()
	}
	return err
}

func (o *opsClient) Endpoints(ctx context.Context) (Endpoints, error) {
	if ep, ok, err := o.cachedEndpoints(); ok {
		return ep, err
	}
	o.refreshMu.Lock()
	defer o.refreshMu.Unlock()
	if ep, ok, err := o.cachedEndpoints(); ok {
		return ep, err
	}

	code, body, err := o.do(ctx, http.MethodGet, configurationsPath)
	if err != nil {
		return Endpoints{}, o.remember(err)
	}
	if code != http.StatusOK {
		return Endpoints{}, o.remember(fmt.Errorf("ops GET configurations: HTTP %d", code))
	}
	ep, err := parseEndpoints(body)
	if err != nil {
		return Endpoints{}, o.remember(err)
	}
	o.mu.Lock()
	o.endpoints, o.endpointAt, o.lastErr = &ep, time.Now(), nil
	o.mu.Unlock()
	return ep, nil
}

// parseEndpoints takes the endpoints from the first complete entry. The GET
// ignores clusterName and lists every cluster; the endpoints are the same in
// each.
func parseEndpoints(body []byte) (Endpoints, error) {
	var entries []struct {
		InstanceFqdn string `json:"vmspInstanceFqdn"`
		PlatformFqdn string `json:"vmspPlatformFqdn"`
		InstanceCA   string `json:"vmspInstanceCertificate"`
		PlatformCA   string `json:"vmspPlatformCertificate"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return Endpoints{}, fmt.Errorf("ops GET configurations: %w", err)
	}
	for _, e := range entries {
		if e.InstanceFqdn != "" && e.PlatformFqdn != "" && e.InstanceCA != "" && e.PlatformCA != "" {
			return Endpoints(e), nil
		}
	}
	return Endpoints{}, errNoEndpoints
}

// Offboard deletes the cluster in Ops. Ops answers 500 ("Downstream service
// returned error") to a DELETE for a cluster it doesn't list, including one
// an earlier DELETE removed while also answering with an error. So a failed
// DELETE is checked against a fresh GET: a cluster not listed is offboarded.
func (o *opsClient) Offboard(ctx context.Context, cluster string) error {
	code, _, err := o.do(ctx, http.MethodDelete, configurationsPath+"/"+url.PathEscape(cluster))
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	deleteErr := fmt.Errorf("ops DELETE configurations/%s: HTTP %d", cluster, code)
	listed, err := o.listed(ctx, cluster)
	if err != nil {
		return fmt.Errorf("%w; and checking whether Ops still lists it: %w", deleteErr, err)
	}
	if !listed {
		return nil
	}
	return deleteErr
}

// listed reports whether a live GET configurations lists the cluster.
func (o *opsClient) listed(ctx context.Context, cluster string) (bool, error) {
	code, body, err := o.do(ctx, http.MethodGet, configurationsPath)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("ops GET configurations: HTTP %d", code)
	}
	var entries []struct {
		ClusterName string `json:"clusterName"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return false, fmt.Errorf("ops GET configurations: %w", err)
	}
	for _, e := range entries {
		if e.ClusterName == cluster {
			return true, nil
		}
	}
	return false, nil
}
