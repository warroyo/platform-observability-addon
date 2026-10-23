package main

// Renders chart/addon-config-definition.yaml the way the addon controller
// does (Go text/template, sprig, Helm's toYaml), against the Secret the
// reconciler really writes, then feeds the result to helm template. The
// controller reports a template error only per cluster after install, in a
// chart version that can't be re-published, so this is the fast loop.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"sigs.k8s.io/yaml"
)

type acdFile struct {
	Spec struct {
		TemplateOutputResources []struct {
			Template            string         `json:"template"`
			TargetClusterOutput map[string]any `json:"targetClusterOutput"`
		} `json:"templateOutputResources"`
	} `json:"spec"`
}

// valuesTemplate returns the template that renders the guest values Secret.
func valuesTemplate(t *testing.T) *template.Template {
	t.Helper()
	raw, err := os.ReadFile("../chart/addon-config-definition.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var acd acdFile
	if err := yaml.Unmarshal(raw, &acd); err != nil {
		t.Fatal(err)
	}
	funcs := sprig.TxtFuncMap()
	funcs["toYaml"] = func(v any) string {
		b, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(b), "\n")
	}
	for _, o := range acd.Spec.TemplateOutputResources {
		if o.TargetClusterOutput != nil && o.TargetClusterOutput["referenceType"] == "ValuesRef" {
			tpl, err := template.New("values").Funcs(funcs).Option("missingkey=error").Parse(o.Template)
			if err != nil {
				t.Fatal(err)
			}
			return tpl
		}
	}
	t.Fatal("no ValuesRef output in the ACD")
	return nil
}

// onboardedSecret is the Secret the reconciler writes for c1, as a map, the
// way the controller hands an input to the template.
func onboardedSecret(t *testing.T) map[string]any {
	t.Helper()
	h := newHarness(t, cluster("ns1", "c1", true, nil))
	h.reconcile("ns1", "c1")
	s := h.secret("ns1", "c1")
	if s == nil {
		t.Fatal("no Secret")
	}
	return s.Object
}

// render returns the helm values the ACD renders for cluster c1.
func render(t *testing.T, values map[string]any, deps map[string]any) map[string]any {
	t.Helper()
	ctx := map[string]any{
		"Values":       values,
		"Dependencies": deps,
		"Cluster":      map[string]any{"name": "c1", "namespace": "ns1"},
		"Addon":        map[string]any{"name": "platform-observability"},
	}
	var out bytes.Buffer
	if err := valuesTemplate(t).Execute(&out, ctx); err != nil {
		t.Fatalf("render: %v", err)
	}
	var secret struct {
		StringData map[string]string `json:"stringData"`
	}
	if err := yaml.Unmarshal(out.Bytes(), &secret); err != nil {
		t.Fatalf("output is not a Secret body: %v\n%s", err, out.String())
	}
	var helmValues map[string]any
	if err := yaml.Unmarshal([]byte(secret.StringData["values.yaml"]), &helmValues); err != nil {
		t.Fatalf("values.yaml: %v\n%s", err, out.String())
	}
	return helmValues
}

func get(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func TestACDRendersTheSecret(t *testing.T) {
	values := map[string]any{"helmValues": map[string]any{
		"aggregator": map[string]any{"replicaCount": 2},
		// Set in an AddonConfig: the Secret wins.
		"vcfOps": map[string]any{"clientSecret": "stale"},
	}}
	// A second input, so .Dependencies is populated whatever the Secret.
	deps := map[string]any{"opsCredentials": onboardedSecret(t), "other": map[string]any{}}
	v := render(t, values, deps)

	if get(v, "vcfOps", "clientSecret") != "secret-c1-1" || get(v, "vcfOps", "instanceCA") != "INSTANCE-CA" ||
		get(v, "vcfOps", "clientId") == "" || get(v, "vcfOps", "platformFqdn") != "vsp01.example" {
		t.Fatalf("vcfOps: %v", get(v, "vcfOps"))
	}
	if get(v, "clusterName") != "c1" {
		t.Fatalf("clusterName %v", get(v, "clusterName"))
	}
	if get(v, "aggregator", "podAnnotations", secretHashAnnotation) != secretHash("secret-c1-1") {
		t.Fatalf("hash: %v", get(v, "aggregator"))
	}
	if n, _ := get(v, "aggregator", "replicaCount").(float64); n != 2 {
		t.Fatalf("the AddonConfig's own values lost: %v", get(v, "aggregator"))
	}
	if _, set := v["enabled"]; set {
		t.Fatal("enabled set with credentials present")
	}
	helmTemplate(t, v, true)
}

func TestACDWithoutCredentialsInstallsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		values map[string]any
		deps   map[string]any
	}{
		"no AddonConfig and no inputs": {nil, nil},
		"empty inputs":                 {map[string]any{}, map[string]any{}},
		"another input but no Secret":  {map[string]any{"helmValues": map[string]any{}}, map[string]any{"other": map[string]any{}}},
		"vcfOps without a secret":      {map[string]any{"helmValues": map[string]any{"vcfOps": map[string]any{"clientId": "x"}}}, nil},
		"helmValues present but empty": {map[string]any{"helmValues": nil}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			v := render(t, tc.values, tc.deps)
			if v["enabled"] != false {
				t.Fatalf("enabled %v, want false", v["enabled"])
			}
			helmTemplate(t, v, false)
		})
	}
}

// Credentials in an AddonConfig are not a way in: without the Secret the
// chart installs nothing, and the AddonConfig's secret is not passed on.
func TestACDIgnoresAddonConfigCredentials(t *testing.T) {
	legacy := map[string]any{
		"vcfOps": map[string]any{
			"instanceFqdn": "shared01.example", "platformFqdn": "vsp01.example",
			"instanceCA": "INSTANCE-CA", "platformCA": "PLATFORM-CA",
			"clientId": "id-c1", "clientSecret": "kept-secret",
		},
	}
	v := render(t, map[string]any{"helmValues": legacy}, nil)
	if v["enabled"] != false || get(v, "vcfOps", "clientSecret") != nil {
		t.Fatalf("values: %v", v)
	}
	helmTemplate(t, v, false)
}

// helmTemplate renders the chart with these values and checks whether the
// collectors are in it.
func helmTemplate(t *testing.T, values map[string]any, wantCollectors bool) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Log("helm not installed; skipping helm template")
		return
	}
	b, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	f := t.TempDir() + "/values.yaml"
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(context.Background(), "helm", "template", "x", "../chart/platform-observability", "-f", f).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	has := strings.Contains(string(out), "name: otel-gateway-oauth-creds") && strings.Contains(string(out), "kind: DaemonSet")
	if has != wantCollectors {
		t.Fatalf("collectors rendered: %v, want %v\n%s", has, wantCollectors, out)
	}
}
