// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package backendauth adds go-proxy's own credentials to its requests to a
// backend MCP server that requires them: a static API key or token sent in a
// header, or an OAuth 2.0 access token obtained with the client-credentials
// grant (go-proxy logging in as an application).
//
// These are go-proxy's credentials for that server, the same for every user -
// never a client's own token, which go-proxy never forwards (see
// docs/architecture/auth.md). Per-user identity still travels separately, in
// the signed X-Go-Proxy-Assertion header.
//
// A server is assigned a Kubernetes Secret only by go-proxy's own config:
// the discovery section's k8sCredentials, or a static server entry's
// credentialsSecret - never by the backend itself. The Secret lives in
// go-proxy's own namespace, and must say which hosts its credential may be
// sent to, in its mcp.io/allowed-hosts annotation (see AllowedHostsAnnotation):
// a request to any other host fails, so neither a config change pointing a
// server's URL elsewhere nor a backend's redirect can send the credential
// to a host it wasn't issued for. Its data holds exactly one of:
//
//   - token: sent as "Authorization: Bearer <token>"
//   - header + value: sent as "<header>: <value>" (e.g. X-API-Key)
//   - tokenUrl + clientId + clientSecret (+ optional scopes, space-separated,
//     and audience): OAuth 2.0 client credentials; the resulting access
//     token is sent as "Authorization: Bearer <access token>"
//
// The Secret is re-read at most every refreshInterval, so a rotated
// credential takes effect without a restart; OAuth tokens are cached until
// shortly before they expire. Everything fails closed: if a named Secret
// can't be read or parsed, or a token can't be obtained, the request fails
// instead of going out without credentials.
package backendauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AllowedHostsAnnotation is the Secret annotation listing the hosts its
// credential may be sent to: comma- or space-separated "host" or
// "host:port" entries (case-insensitive). An entry without a port matches
// any port. A Secret without it is refused.
const AllowedHostsAnnotation = "mcp.io/allowed-hosts"

// Secret is what a SecretReader returns: a Secret's data and annotations.
type Secret struct {
	Data        map[string][]byte
	Annotations map[string]string
}

// SecretReader returns the named Secret.
type SecretReader func(ctx context.Context, name string) (*Secret, error)

// Store resolves Secret names to credentials, caching them.
type Store struct {
	read            SecretReader
	refreshInterval time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	fetched     time.Time
	fingerprint [sha256.Size]byte
	cred        *credential
	// refreshing is set while a caller re-reads the Secret (see credential).
	refreshing bool
}

// NewStore returns a Store that reads Secrets with read and re-reads each one
// at most every refreshInterval (0 re-reads on every request).
func NewStore(read SecretReader, refreshInterval time.Duration) *Store {
	return &Store{read: read, refreshInterval: refreshInterval, entries: make(map[string]*entry)}
}

// Default is the Store go-proxy's backend connections use: it reads Secrets
// from go-proxy's own namespace through the Kubernetes API, re-reading each
// every minute. Tests replace it.
var Default = NewStore(readKubernetesSecret, time.Minute)

// tokenRequestTimeout bounds a single OAuth token request.
const tokenRequestTimeout = 15 * time.Second

// Client returns base unchanged when secretName is empty; otherwise a client
// whose requests carry the credentials from secretName (resolved through
// Default), on top of whatever base's transport already does.
func Client(base *http.Client, secretName string) *http.Client {
	if secretName == "" {
		return base
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &http.Client{
		Transport: &transport{base: rt, store: Default, secretName: secretName},
		Timeout:   base.Timeout,
	}
}

type transport struct {
	base       http.RoundTripper
	store      *Store
	secretName string
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	cred, err := t.store.credential(req.Context(), t.secretName)
	if err == nil && !cred.allows(req.URL) {
		err = fmt.Errorf("host %q isn't in the secret's %s annotation", req.URL.Host, AllowedHostsAnnotation)
	}
	if err != nil {
		// A RoundTripper must close the request body, even on error.
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("backend credentials from secret %q: %w", t.secretName, err)
	}
	name, value, err := cred.header(req.Context())
	if err != nil {
		return nil, fmt.Errorf("backend credentials from secret %q: %w", t.secretName, err)
	}
	out := req.Clone(req.Context())
	out.Header.Set(name, value)
	return t.base.RoundTrip(out)
}

// credential returns secretName's parsed credential, re-reading the Secret
// once its cached copy is older than refreshInterval. If a re-read fails
// but an earlier read succeeded, the last good credential is kept (and the
// failure logged), so a transient API error doesn't cut off a backend.
func (s *Store) credential(ctx context.Context, secretName string) (*credential, error) {
	// The lock guards only the map, never the Kubernetes API read below:
	// held across it, one slow read would hold up every credentialed
	// request to every backend. While one caller refreshes an entry, the
	// others go on with its last good credential.
	s.mu.Lock()
	e := s.entries[secretName]
	if e != nil && (time.Since(e.fetched) < s.refreshInterval || e.refreshing) {
		cred := e.cred
		s.mu.Unlock()
		return cred, nil
	}
	if e != nil {
		e.refreshing = true
	}
	s.mu.Unlock()

	secret, err := s.read(ctx, secretName)

	s.mu.Lock()
	defer s.mu.Unlock()
	if e != nil {
		e.refreshing = false
	}
	if err != nil {
		if e != nil {
			logging.Errorf("backendauth: re-reading secret %q failed, keeping the last good credentials: %v", secretName, err)
			e.fetched = time.Now()
			return e.cred, nil
		}
		return nil, err
	}

	fp := fingerprint(secret)
	if e != nil && fp == e.fingerprint {
		e.fetched = time.Now()
		return e.cred, nil
	}

	cred, err := parseCredential(secret.Data)
	if err != nil {
		return nil, err
	}
	cred.allowedHosts, err = parseAllowedHosts(secret.Annotations[AllowedHostsAnnotation])
	if err != nil {
		return nil, err
	}
	s.entries[secretName] = &entry{fetched: time.Now(), fingerprint: fp, cred: cred}
	logging.Infof("backendauth: loaded %s credentials from secret %q", cred.kind, secretName)
	return cred, nil
}

// credential is one Secret's parsed contents: either a fixed header, or an
// OAuth token source whose token goes in the Authorization header.
type credential struct {
	kind        string // "bearer token", "header", "OAuth client-credentials"; for logs only
	headerName  string
	headerValue string
	tokens      oauth2.TokenSource
	// allowedHosts is the Secret's AllowedHostsAnnotation, parsed.
	allowedHosts []allowedHost
}

type allowedHost struct {
	host string // lowercased hostname
	port string // "" matches any port
}

func parseAllowedHosts(raw string) ([]allowedHost, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	if len(fields) == 0 {
		return nil, fmt.Errorf("the secret has no %s annotation saying which hosts its credential may be sent to", AllowedHostsAnnotation)
	}
	hosts := make([]allowedHost, 0, len(fields))
	for _, f := range fields {
		u, err := url.Parse("//" + f)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" {
			return nil, fmt.Errorf("invalid %s entry %q: want host or host:port", AllowedHostsAnnotation, f)
		}
		hosts = append(hosts, allowedHost{host: strings.ToLower(u.Hostname()), port: u.Port()})
	}
	return hosts, nil
}

// allows reports whether the credential may be sent to u's host.
func (c *credential) allows(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	for _, h := range c.allowedHosts {
		if h.host == host && (h.port == "" || h.port == port) {
			return true
		}
	}
	return false
}

func (c *credential) header(ctx context.Context) (string, string, error) {
	if c.tokens == nil {
		return c.headerName, c.headerValue, nil
	}
	tok, err := c.tokens.Token()
	if err != nil {
		return "", "", fmt.Errorf("obtaining an OAuth access token: %w", err)
	}
	return "Authorization", "Bearer " + tok.AccessToken, nil
}

func parseCredential(data map[string][]byte) (*credential, error) {
	get := func(key string) string { return strings.TrimSpace(string(data[key])) }

	token := get("token")
	header, value := get("header"), get("value")
	tokenURL, clientID, clientSecret := get("tokenUrl"), get("clientId"), get("clientSecret")

	styles := 0
	for _, used := range []bool{token != "", header != "" || value != "", tokenURL != "" || clientID != "" || clientSecret != ""} {
		if used {
			styles++
		}
	}
	switch {
	case styles == 0:
		return nil, errors.New("the secret must contain token, header+value, or tokenUrl+clientId+clientSecret")
	case styles > 1:
		return nil, errors.New("the secret must contain only one of: token, header+value, tokenUrl+clientId+clientSecret")
	}

	switch {
	case token != "":
		return &credential{kind: "bearer token", headerName: "Authorization", headerValue: "Bearer " + token}, nil

	case header != "" || value != "":
		if header == "" || value == "" {
			return nil, errors.New("header and value must be given together")
		}
		if strings.HasPrefix(http.CanonicalHeaderKey(header), "X-Go-Proxy-") {
			return nil, fmt.Errorf("header %q is reserved for go-proxy's own relay headers", header)
		}
		return &credential{kind: "header", headerName: header, headerValue: value}, nil

	default:
		if tokenURL == "" || clientID == "" || clientSecret == "" {
			return nil, errors.New("OAuth client credentials need tokenUrl, clientId and clientSecret")
		}
		if u, err := url.Parse(tokenURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("tokenUrl %q is not an http(s) URL", tokenURL)
		}
		cfg := clientcredentials.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			TokenURL:     tokenURL,
			Scopes:       strings.Fields(get("scopes")),
		}
		if audience := get("audience"); audience != "" {
			cfg.EndpointParams = url.Values{"audience": {audience}}
		}
		// The token source outlives any one request, so it gets its own
		// context; tokenRequestTimeout bounds each token request instead.
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: tokenRequestTimeout})
		return &credential{kind: "OAuth client-credentials", tokens: cfg.TokenSource(ctx)}, nil
	}
}

// fingerprint lets a re-read with unchanged data and allowed hosts keep
// the existing credential, and with it any cached OAuth token.
func fingerprint(secret *Secret) [sha256.Size]byte {
	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d:%s=%d:", len(k), k, len(secret.Data[k]))
		h.Write(secret.Data[k])
	}
	hosts := secret.Annotations[AllowedHostsAnnotation]
	fmt.Fprintf(h, "hosts=%d:%s", len(hosts), hosts)
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// readKubernetesSecret reads name from go-proxy's own namespace. Outside
// Kubernetes there are no Secrets to read, so a server configured with
// credentials can't be reached - failing closed.
func readKubernetesSecret(ctx context.Context, name string) (*Secret, error) {
	clientset, err := configsync.K8sClientset()
	if err != nil {
		return nil, fmt.Errorf("credentials secrets can only be read in Kubernetes: %w", err)
	}
	namespace, err := configsync.PodNamespace()
	if err != nil {
		return nil, err
	}
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsForbidden(err) {
		// go-proxy may read only the credentials Secrets deploy.sh granted
		// it (apply_credentials_rbac), from the discovery config as it was
		// when deploy.sh last ran.
		return nil, fmt.Errorf("go-proxy isn't allowed to read Secret %q: re-run scripts/deploy.sh --deploy-proxy, which grants it every credentials Secret the discovery config names: %w", name, err)
	}
	if err != nil {
		return nil, err
	}
	return &Secret{Data: secret.Data, Annotations: secret.Annotations}, nil
}
