// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseConfigFile_K8sMode(t *testing.T) {
	config, err := ParseConfigFile("mcp_discovery_k8s_test.json")
	if err != nil {
		t.Fatalf("Failed to parse config file: %v", err)
	}
	if config.Mode != ModeK8s {
		t.Errorf("Expected ModeK8s, got %v", config.Mode)
	}
	if config.FrequencySeconds != 2 {
		t.Errorf("Expected FrequencySeconds to be 2, got %v", config.FrequencySeconds)
	}
	if want := []string{"mcp-proxy", "gardening-demo"}; !reflect.DeepEqual(config.K8sNamespaces, want) {
		t.Errorf("Expected K8sNamespaces %v, got %v", want, config.K8sNamespaces)
	}
	if config.K8sLabelSelector != defaultK8sLabelSelector {
		t.Errorf("Expected K8sLabelSelector to default to %v, got %v", defaultK8sLabelSelector, config.K8sLabelSelector)
	}
	if got := config.K8sCredentials["gardening-demo/garden-online-store"]; got != "garden-store-credentials" {
		t.Errorf("Expected k8sCredentials entry, got %q", got)
	}
	if got := config.K8sPinnedServers["rate-limiter"]; got != "mcp-proxy/rate-limiter" {
		t.Errorf("Expected k8sPinnedServers entry, got %q", got)
	}
}

func TestParseConfig_K8sModeRejects(t *testing.T) {
	cases := map[string]string{
		"missing k8sNamespaces":   `{"discoveryMode": "k8s", "listedTools": ["*"]}`,
		"empty k8sNamespaces":     `{"discoveryMode": "k8s", "listedTools": ["*"], "k8sNamespaces": []}`,
		"blank namespace":         `{"discoveryMode": "k8s", "listedTools": ["*"], "k8sNamespaces": [" "]}`,
		"duplicate namespace":     `{"discoveryMode": "k8s", "listedTools": ["*"], "k8sNamespaces": ["a", "a"]}`,
		"credentials bad key":     `{"discoveryMode": "k8s", "listedTools": ["*"], "k8sNamespaces": ["a"], "k8sCredentials": {"svc": "s"}}`,
		"credentials empty value": `{"discoveryMode": "k8s", "listedTools": ["*"], "k8sNamespaces": ["a"], "k8sCredentials": {"a/svc": ""}}`,
		"pinned bad ref":          `{"discoveryMode": "k8s", "listedTools": ["*"], "k8sNamespaces": ["a"], "k8sPinnedServers": {"x": "a/b/c"}}`,
		"pinned bad name":         `{"discoveryMode": "k8s", "listedTools": ["*"], "k8sNamespaces": ["a"], "k8sPinnedServers": {"x.y": "a/b"}}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(cfg)); err == nil {
				t.Fatalf("expected an error, got nil")
			}
		})
	}
}

func TestMarshalJSON_K8sModeRoundTrips(t *testing.T) {
	config, err := ParseConfigFile("mcp_discovery_k8s_test.json")
	if err != nil {
		t.Fatalf("ParseConfigFile: %v", err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	again, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig of marshaled config: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(config, again) {
		t.Errorf("round trip changed the config:\nbefore %+v\nafter  %+v", config, again)
	}
}

func TestParseConfigFile_UnknownModeMentionsK8s(t *testing.T) {
	tmp := t.TempDir() + "/bad_discovery.json"
	if err := os.WriteFile(tmp, []byte(`{"discoveryMode": "bogus", "listedTools": ["*"]}`), 0o644); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}
	_, err := ParseConfigFile(tmp)
	if err == nil {
		t.Fatal("Expected an error for an unknown discovery mode, got nil")
	}
	if !strings.Contains(err.Error(), "'k8s'") {
		t.Errorf("Expected error to mention the 'k8s' mode, got: %v", err)
	}
}

func TestServerNameFor(t *testing.T) {
	svcNoAnnotation := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "customer-service"}}
	if got := serverNameFor(svcNoAnnotation); got != "customer-service" {
		t.Errorf("Expected fallback to Service name, got %v", got)
	}

	svcWithAnnotation := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:        "svc-cs-prod-v2",
		Annotations: map[string]string{annotationServerName: "customer-service"},
	}}
	if got := serverNameFor(svcWithAnnotation); got != "customer-service" {
		t.Errorf("Expected annotation override, got %v", got)
	}
}

func TestServerUrlFor(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "customer-service", Namespace: "mcp-proxy"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}
	want := "http://customer-service.mcp-proxy.svc.cluster.local:8080"
	if got, err := serverUrlFor(svc); err != nil || got != want {
		t.Errorf("Expected %v, got %v (err %v)", want, got, err)
	}

	svcWithPathAndPortOverride := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "customer-service",
			Namespace: "mcp-proxy",
			Annotations: map[string]string{
				annotationPort: "9090",
				annotationPath: "/mcp",
			},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}
	want = "http://customer-service.mcp-proxy.svc.cluster.local:9090/mcp"
	if got, err := serverUrlFor(svcWithPathAndPortOverride); err != nil || got != want {
		t.Errorf("Expected %v, got %v (err %v)", want, got, err)
	}
}

// TestServerUrlFor_RejectsBadPaths covers paths that are never a real
// endpoint path, including ones that, spliced into the URL as plain text,
// would change the host it points at (e.g. "@" turns the cluster name into
// userinfo).
func TestServerUrlFor_RejectsBadPaths(t *testing.T) {
	for _, path := range []string{
		"@evil.example.com/mcp",
		"/mcp@evil.example.com",
		"mcp",
		"//evil.example.com/mcp",
		"/mcp?x=1",
		"/mcp#frag",
		`/mcp\evil`,
		"/mcp\n",
	} {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "customer-service",
				Namespace:   "mcp-proxy",
				Annotations: map[string]string{annotationPath: path},
			},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
		}
		if got, err := serverUrlFor(svc); err == nil {
			t.Errorf("path %q: expected an error, got URL %q", path, got)
		}
	}
}

func TestPortFor(t *testing.T) {
	svcFirstPort := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}, {Port: 8081}}}}
	if got, err := portFor(svcFirstPort); err != nil || got != 8080 {
		t.Errorf("Expected first port 8080, got %v (err %v)", got, err)
	}

	svcAnnotationOverride := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{annotationPort: "9090"}},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}
	if got, err := portFor(svcAnnotationOverride); err != nil || got != 9090 {
		t.Errorf("Expected annotation override 9090, got %v (err %v)", got, err)
	}
}

func TestPortFor_Rejects(t *testing.T) {
	if _, err := portFor(&corev1.Service{}); err == nil {
		t.Error("expected an error for a Service with no ports")
	}
	for _, raw := range []string{"abc", "0", "-1", "65536", "4294967376", ""} {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{annotationPort: raw}},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
		}
		if got, err := portFor(svc); err == nil {
			t.Errorf("port annotation %q: expected an error, got %v", raw, got)
		}
	}
}

func testService(namespace, name string, annotations map[string]string) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: annotations},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}
}

func TestBuildServerMap(t *testing.T) {
	cfg := &MCPDiscoveryConfig{
		K8sNamespaces:    []string{"mcp-proxy", "gardening-demo"},
		K8sCredentials:   map[string]string{"gardening-demo/garden-online-store": "garden-store-credentials"},
		K8sPinnedServers: map[string]string{"rate-limiter": "mcp-proxy/rate-limiter"},
	}

	tests := []struct {
		name     string
		services []corev1.Service
		want     ServerMap
	}{
		{
			name: "plain services",
			services: []corev1.Service{
				testService("mcp-proxy", "event-aggregator", nil),
				testService("gardening-demo", "garden-online-store", map[string]string{annotationPath: "/mcp"}),
			},
			want: ServerMap{
				"event-aggregator":    {ServerName: "event-aggregator", ServerUrl: "http://event-aggregator.mcp-proxy.svc.cluster.local:8080"},
				"garden-online-store": {ServerName: "garden-online-store", ServerUrl: "http://garden-online-store.gardening-demo.svc.cluster.local:8080/mcp", CredentialsSecret: "garden-store-credentials"},
			},
		},
		{
			name: "credentials follow the Service, not a claimed server name",
			services: []corev1.Service{
				testService("gardening-demo", "impostor", map[string]string{annotationServerName: "garden-online-store"}),
			},
			want: ServerMap{
				"garden-online-store": {ServerName: "garden-online-store", ServerUrl: "http://impostor.gardening-demo.svc.cluster.local:8080"},
			},
		},
		{
			name: "earlier namespace wins a name, whatever the list order",
			services: []corev1.Service{
				testService("gardening-demo", "fake-guardrail", map[string]string{annotationServerName: "declarative-guardrail"}),
				testService("mcp-proxy", "declarative-guardrail", nil),
			},
			want: ServerMap{
				"declarative-guardrail": {ServerName: "declarative-guardrail", ServerUrl: "http://declarative-guardrail.mcp-proxy.svc.cluster.local:8080"},
			},
		},
		{
			name: "within a namespace, the Service named like the server wins",
			services: []corev1.Service{
				testService("gardening-demo", "aaa", map[string]string{annotationServerName: "garden-staff-admin"}),
				testService("gardening-demo", "garden-staff-admin", nil),
			},
			want: ServerMap{
				"garden-staff-admin": {ServerName: "garden-staff-admin", ServerUrl: "http://garden-staff-admin.gardening-demo.svc.cluster.local:8080"},
			},
		},
		{
			name: "otherwise the lowest Service name wins",
			services: []corev1.Service{
				testService("gardening-demo", "zzz", map[string]string{annotationServerName: "shared"}),
				testService("gardening-demo", "bbb", map[string]string{annotationServerName: "shared"}),
			},
			want: ServerMap{
				"shared": {ServerName: "shared", ServerUrl: "http://bbb.gardening-demo.svc.cluster.local:8080"},
			},
		},
		{
			name: "pinned name is only accepted from the pinned Service",
			services: []corev1.Service{
				testService("mcp-proxy", "fake-limiter", map[string]string{annotationServerName: "rate-limiter"}),
				testService("gardening-demo", "rate-limiter", nil),
			},
			want: ServerMap{},
		},
		{
			name: "pinned Service is accepted",
			services: []corev1.Service{
				testService("mcp-proxy", "rate-limiter", nil),
			},
			want: ServerMap{
				"rate-limiter": {ServerName: "rate-limiter", ServerUrl: "http://rate-limiter.mcp-proxy.svc.cluster.local:8080"},
			},
		},
		{
			name: "invalid names, paths, ports and ExternalName Services are skipped",
			services: []corev1.Service{
				testService("gardening-demo", "dotted", map[string]string{annotationServerName: "garden.online"}),
				testService("gardening-demo", "colon", map[string]string{annotationServerName: "garden:online"}),
				testService("gardening-demo", "redirect", map[string]string{annotationPath: "@evil.example.com/mcp"}),
				testService("gardening-demo", "badport", map[string]string{annotationPort: "99999"}),
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "gardening-demo", Name: "external"},
					Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "evil.example.com", Ports: []corev1.ServicePort{{Port: 8080}}},
				},
				testService("gardening-demo", "good", nil),
			},
			want: ServerMap{
				"good": {ServerName: "good", ServerUrl: "http://good.gardening-demo.svc.cluster.local:8080"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := buildServerMap(tt.services, cfg, nil); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildServerMap:\ngot  %v\nwant %v", describe(got), describe(tt.want))
			}
		})
	}
}

func describe(m ServerMap) map[string]ServerAttributes {
	out := make(map[string]ServerAttributes, len(m))
	for k, v := range m {
		out[k] = *v
	}
	return out
}

func TestBuildServerMap_ReturnsProblemsInsteadOfLogging(t *testing.T) {
	cfg := &MCPDiscoveryConfig{K8sNamespaces: []string{"gardening-demo"}}
	_, problems := buildServerMap([]corev1.Service{
		testService("gardening-demo", "good", nil),
		testService("gardening-demo", "dotted", map[string]string{annotationServerName: "garden.online"}),
		testService("gardening-demo", "badport", map[string]string{annotationPort: "0"}),
	}, cfg, nil)
	if len(problems) != 2 {
		t.Fatalf("expected 2 problems (invalid name, invalid port), got %v", problems)
	}
}

// TestBuildServerMapMustPin checks that a name go-proxy trusts for a
// security decision is never accepted from an unpinned Service, while a
// pinned one still is.
func TestBuildServerMapMustPin(t *testing.T) {
	cfg := &MCPDiscoveryConfig{
		K8sNamespaces:    []string{"mcp-proxy"},
		K8sPinnedServers: map[string]string{"rate-limiter": "mcp-proxy/rate-limiter"},
	}
	mustPin := MustPin{"rate-limiter": "rate limiting", "declarative-guardrail": "guardrails"}
	got, problems := buildServerMap([]corev1.Service{
		testService("mcp-proxy", "rate-limiter", nil),
		testService("mcp-proxy", "declarative-guardrail", nil),
		testService("mcp-proxy", "ordinary", nil),
	}, cfg, mustPin)

	if _, ok := got["rate-limiter"]; !ok {
		t.Error("the pinned rate-limiter wasn't discovered")
	}
	if _, ok := got["declarative-guardrail"]; ok {
		t.Error("an unpinned declarative-guardrail was discovered")
	}
	if _, ok := got["ordinary"]; !ok {
		t.Error("a name nothing trusts must not need a pin")
	}
	if len(problems) != 1 || !strings.Contains(problems[0], `"declarative-guardrail" must be pinned`) {
		t.Errorf("problems = %v; want one about declarative-guardrail", problems)
	}
}

func TestUnpinnedNames(t *testing.T) {
	mustPin := MustPin{"rate-limiter": "rate limiting", "garden-online-store": "internalAuth.relay source"}
	k8s := &MCPDiscoveryConfig{Mode: ModeK8s, K8sPinnedServers: map[string]string{"rate-limiter": "mcp-proxy/rate-limiter"}}
	err := UnpinnedNames(k8s, mustPin)
	if err == nil || !strings.Contains(err.Error(), `"garden-online-store" (internalAuth.relay source)`) || strings.Contains(err.Error(), `"rate-limiter"`) {
		t.Errorf("UnpinnedNames = %v; want only garden-online-store reported", err)
	}
	if err := UnpinnedNames(&MCPDiscoveryConfig{Mode: ModeStatic}, mustPin); err != nil {
		t.Errorf("static mode needs no pins, got %v", err)
	}
}
