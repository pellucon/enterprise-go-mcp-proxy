// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Annotation keys read off a Service that already matched the discovery
// label selector (see defaultK8sLabelSelector). All are optional: a Service
// with none of them still resolves to a working ServerAttributes using the
// Service's own name/first port/root path. No annotation assigns a
// credentials Secret: that comes only from the discovery config's
// k8sCredentials, or any Service could ask for any backend's credentials.
const (
	annotationServerName = "mcp.io/server-name"
	annotationPort       = "mcp.io/port"
	annotationPath       = "mcp.io/path"
)

// k8sListTimeout bounds one Services List call, so a hung API server
// can't stall ServerMonitor forever.
const k8sListTimeout = 10 * time.Second

var (
	k8sClientset     *kubernetes.Clientset
	k8sClientsetOnce sync.Once
	k8sClientsetErr  error
)

// getK8sClientset builds the in-cluster clientset once and reuses it across
// every poll tick, since discoverK8sServers is called on ServerMonitor's
// configured frequency, not just once at startup.
func getK8sClientset() (*kubernetes.Clientset, error) {
	k8sClientsetOnce.Do(func() {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			k8sClientsetErr = fmt.Errorf("k8s discovery: failed to load in-cluster config (are you running inside a k8s pod?): %w", err)
			return
		}
		clientset, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			k8sClientsetErr = fmt.Errorf("k8s discovery: failed to build clientset: %w", err)
			return
		}
		k8sClientset = clientset
	})
	return k8sClientset, k8sClientsetErr
}

// discoverK8sServers lists the Services matching cfg.K8sLabelSelector in
// each of cfg.K8sNamespaces and turns them into ServerAttributes pointed at
// each Service's cluster-DNS name, so kube-proxy load-balances across
// whatever pods currently back it (see CLAUDE.md's "Multi-pod load
// balancing" discussion). This is the DiscoveryFunc for ModeK8s;
// ServerMonitor itself needs no changes to drive it, since ServerMonitor is
// generic over any func() (ServerMap, error).
//
// Any namespace failing to list fails the whole call: a partial result
// would make that namespace's servers look removed. ServerMonitor keeps its
// last-seen set on an error.
func discoverK8sServers(cfg *MCPDiscoveryConfig, mustPin MustPin) (ServerMap, error) {
	clientset, err := getK8sClientset()
	if err != nil {
		return nil, err
	}

	var services []corev1.Service
	for _, namespace := range cfg.K8sNamespaces {
		ctx, cancel := context.WithTimeout(context.Background(), k8sListTimeout)
		list, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: cfg.K8sLabelSelector,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("k8s discovery: failed to list services in namespace %q with selector %q: %w", namespace, cfg.K8sLabelSelector, err)
		}
		services = append(services, list.Items...)
	}
	serverMap, problems := buildServerMap(services, cfg, mustPin)
	k8sProblems.report(problems)
	return serverMap, nil
}

// buildServerMap turns already-listed Services into a ServerMap, skipping
// (and logging) any Service that can't be trusted as a backend as-is:
// an ExternalName Service, an invalid server name, a pinned name claimed by
// a Service other than the pinned one, a mustPin name that isn't pinned at
// all, or an invalid port/path annotation.
// When several Services claim the same server name, the winner is chosen
// deterministically - the earliest namespace in cfg.K8sNamespaces, then a
// Service whose own name is the server name, then the lowest Service name -
// so the result never depends on list order.
//
// Problems are returned rather than logged, since this runs on every
// discovery pass; discoverK8sServers reports them through k8sProblems,
// which logs each one only once while it persists.
func buildServerMap(services []corev1.Service, cfg *MCPDiscoveryConfig, mustPin MustPin) (ServerMap, problemList) {
	var problems problemList
	nsRank := make(map[string]int, len(cfg.K8sNamespaces))
	for i, ns := range cfg.K8sNamespaces {
		nsRank[ns] = i
	}
	rank := func(ns string) int {
		if r, ok := nsRank[ns]; ok {
			return r
		}
		return len(nsRank)
	}

	type candidate struct {
		svc   *corev1.Service
		attrs *ServerAttributes
	}
	candidates := make(map[string][]candidate)

	for i := range services {
		svc := &services[i]
		ref := svc.Namespace + "/" + svc.Name

		if svc.Spec.Type == corev1.ServiceTypeExternalName {
			problems.errorf("k8s discovery: skipping Service %s: ExternalName Services can point anywhere and aren't supported as backends", ref)
			continue
		}

		name := serverNameFor(svc)
		if err := ValidateServerName(name); err != nil {
			problems.errorf("k8s discovery: skipping Service %s: %v", ref, err)
			continue
		}
		pinned, isPinned := cfg.K8sPinnedServers[name]
		if isPinned && pinned != ref {
			problems.errorf("k8s discovery: skipping Service %s: server name %q is pinned to %s", ref, name, pinned)
			continue
		}
		if why, ok := mustPin[name]; ok && !isPinned {
			problems.errorf("k8s discovery: skipping Service %s: server name %q must be pinned in k8sPinnedServers (%s)", ref, name, why)
			continue
		}
		serverURL, err := serverUrlFor(svc)
		if err != nil {
			problems.errorf("k8s discovery: skipping Service %s: %v", ref, err)
			continue
		}

		candidates[name] = append(candidates[name], candidate{svc: svc, attrs: &ServerAttributes{
			ServerName:        name,
			ServerUrl:         serverURL,
			CredentialsSecret: cfg.K8sCredentials[ref],
		}})
	}

	serverMap := make(ServerMap, len(candidates))
	for name, cs := range candidates {
		if len(cs) > 1 {
			sort.Slice(cs, func(i, j int) bool {
				a, b := cs[i].svc, cs[j].svc
				if ra, rb := rank(a.Namespace), rank(b.Namespace); ra != rb {
					return ra < rb
				}
				if ea, eb := a.Name == name, b.Name == name; ea != eb {
					return ea
				}
				return a.Name < b.Name
			})
			refs := make([]string, len(cs))
			for i, c := range cs {
				refs[i] = c.svc.Namespace + "/" + c.svc.Name
			}
			problems.errorf("k8s discovery: server name %q is claimed by several Services %v; using %s", name, refs, refs[0])
		}
		serverMap[name] = cs[0].attrs
	}
	return serverMap, problems
}

func serverNameFor(svc *corev1.Service) string {
	if name := svc.Annotations[annotationServerName]; name != "" {
		return name
	}
	return svc.Name
}

// serverUrlFor builds the Service's in-cluster URL from its parts, so no
// annotation can change the host it points at.
func serverUrlFor(svc *corev1.Service) (string, error) {
	port, err := portFor(svc)
	if err != nil {
		return "", err
	}
	path := svc.Annotations[annotationPath]
	if err := validatePath(path); err != nil {
		return "", err
	}
	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(svc.Name+"."+svc.Namespace+".svc.cluster.local", strconv.Itoa(port)),
		Path:   path,
	}
	return u.String(), nil
}

// validatePath accepts an empty path or a plain absolute path. Anything
// else (no leading "/", a "//" that a URL parser could read as a new
// authority, a query, fragment, userinfo "@", backslash or control
// character) is rejected rather than escaped, since it's never a legitimate
// MCP endpoint path.
func validatePath(path string) error {
	if path == "" {
		return nil
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return fmt.Errorf("invalid %s annotation %q: must be an absolute path starting with a single /", annotationPath, path)
	}
	if strings.ContainsAny(path, "?#@\\") || strings.ContainsFunc(path, unicode.IsControl) {
		return fmt.Errorf("invalid %s annotation %q: must not contain ?, #, @, \\ or control characters", annotationPath, path)
	}
	return nil
}

// portFor returns the mcp.io/port annotation if set (which must be a valid
// port number), otherwise the Service's first port.
func portFor(svc *corev1.Service) (int, error) {
	if raw, ok := svc.Annotations[annotationPort]; ok {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			return 0, fmt.Errorf("invalid %s annotation %q: must be a port number between 1 and 65535", annotationPort, raw)
		}
		return p, nil
	}
	if len(svc.Spec.Ports) > 0 && svc.Spec.Ports[0].Port > 0 {
		return int(svc.Spec.Ports[0].Port), nil
	}
	return 0, fmt.Errorf("Service has no ports and no %s annotation", annotationPort)
}
