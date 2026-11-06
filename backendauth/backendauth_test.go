// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package backendauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSecrets is an in-memory SecretReader whose contents tests can change.
// Every Secret allows host 127.0.0.1 (httptest's), unless hosts overrides
// its AllowedHostsAnnotation.
type fakeSecrets struct {
	mu    sync.Mutex
	data  map[string]map[string][]byte
	hosts map[string]string
	err   error
}

func (f *fakeSecrets) read(_ context.Context, name string) (*Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.data[name]
	if !ok {
		return nil, fmt.Errorf("secret %q not found", name)
	}
	hosts, ok := f.hosts[name]
	if !ok {
		hosts = "127.0.0.1"
	}
	annotations := map[string]string{}
	if hosts != "" {
		annotations[AllowedHostsAnnotation] = hosts
	}
	return &Secret{Data: d, Annotations: annotations}, nil
}

func (f *fakeSecrets) setHosts(name, hosts string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hosts == nil {
		f.hosts = map[string]string{}
	}
	f.hosts[name] = hosts
}

func (f *fakeSecrets) set(name string, kv ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := map[string][]byte{}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i]] = []byte(kv[i+1])
	}
	f.data[name] = d
}

// echoBackend records the headers of the last request it received.
func echoBackend(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var last http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header { mu.Lock(); defer mu.Unlock(); return last }
}

func withStore(t *testing.T, s *Store) {
	t.Helper()
	old := Default
	Default = s
	t.Cleanup(func() { Default = old })
}

func get(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func TestClientWithoutSecretIsUnchanged(t *testing.T) {
	base := &http.Client{}
	if got := Client(base, ""); got != base {
		t.Fatal("Client with no secret should return the base client unchanged")
	}
}

func TestBearerToken(t *testing.T) {
	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "token", "s3cret")
	withStore(t, NewStore(secrets.read, time.Minute))
	srv, last := echoBackend(t)

	if err := get(t, Client(&http.Client{}, "crm"), srv.URL); err != nil {
		t.Fatal(err)
	}
	if got := last().Get("Authorization"); got != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer s3cret")
	}
}

func TestCustomHeader(t *testing.T) {
	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "header", "X-API-Key", "value", "k-123")
	withStore(t, NewStore(secrets.read, time.Minute))
	srv, last := echoBackend(t)

	if err := get(t, Client(&http.Client{}, "crm"), srv.URL); err != nil {
		t.Fatal(err)
	}
	if got := last().Get("X-API-Key"); got != "k-123" {
		t.Errorf("X-API-Key = %q, want %q", got, "k-123")
	}
	if got := last().Get("Authorization"); got != "" {
		t.Errorf("unexpected Authorization header %q", got)
	}
}

func TestOAuthClientCredentials(t *testing.T) {
	var tokenRequests atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if r.PostForm.Get("grant_type") != "client_credentials" || id != "gw" || secret != "pw" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		if r.PostForm.Get("scope") != "crm.read crm.write" || r.PostForm.Get("audience") != "api://crm" {
			http.Error(w, `{"error":"invalid_scope"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-1","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "tokenUrl", tokenSrv.URL, "clientId", "gw", "clientSecret", "pw", "scopes", "crm.read crm.write", "audience", "api://crm")
	withStore(t, NewStore(secrets.read, 0)) // re-read the secret every time; the token must still be reused
	srv, last := echoBackend(t)

	c := Client(&http.Client{}, "crm")
	for i := 0; i < 3; i++ {
		if err := get(t, c, srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if got := last().Get("Authorization"); got != "Bearer at-1" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer at-1")
	}
	if n := tokenRequests.Load(); n != 1 {
		t.Errorf("token endpoint called %d times for 3 requests, want 1 (token should be cached)", n)
	}
}

func TestOAuthTokenFailureFailsClosed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
	}))
	defer tokenSrv.Close()

	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "tokenUrl", tokenSrv.URL, "clientId", "gw", "clientSecret", "wrong")
	withStore(t, NewStore(secrets.read, time.Minute))
	srv, last := echoBackend(t)

	if err := get(t, Client(&http.Client{}, "crm"), srv.URL); err == nil {
		t.Fatal("request succeeded without a token; want failure")
	}
	if last() != nil {
		t.Error("the backend was called without credentials")
	}
}

func TestRotationIsPickedUp(t *testing.T) {
	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "token", "old")
	withStore(t, NewStore(secrets.read, 0))
	srv, last := echoBackend(t)
	c := Client(&http.Client{}, "crm")

	if err := get(t, c, srv.URL); err != nil {
		t.Fatal(err)
	}
	secrets.set("crm", "token", "new")
	if err := get(t, c, srv.URL); err != nil {
		t.Fatal(err)
	}
	if got := last().Get("Authorization"); got != "Bearer new" {
		t.Errorf("after rotation Authorization = %q, want %q", got, "Bearer new")
	}
}

func TestMissingSecretFailsClosed(t *testing.T) {
	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	withStore(t, NewStore(secrets.read, time.Minute))
	srv, last := echoBackend(t)

	err := get(t, Client(&http.Client{}, "nope"), srv.URL)
	if err == nil || !strings.Contains(err.Error(), `secret "nope"`) {
		t.Fatalf("err = %v, want a failure naming the secret", err)
	}
	if last() != nil {
		t.Error("the backend was called without credentials")
	}
}

func TestTransientReadErrorKeepsLastGoodCredential(t *testing.T) {
	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "token", "good")
	withStore(t, NewStore(secrets.read, 0))
	srv, last := echoBackend(t)
	c := Client(&http.Client{}, "crm")

	if err := get(t, c, srv.URL); err != nil {
		t.Fatal(err)
	}
	secrets.mu.Lock()
	secrets.err = errors.New("API server unavailable")
	secrets.mu.Unlock()
	if err := get(t, c, srv.URL); err != nil {
		t.Fatalf("a transient read error cut off the backend: %v", err)
	}
	if got := last().Get("Authorization"); got != "Bearer good" {
		t.Errorf("Authorization = %q, want the last good %q", got, "Bearer good")
	}
}

func TestInvalidSecrets(t *testing.T) {
	cases := map[string][]string{
		"empty":                  {},
		"two styles":             {"token", "t", "header", "X-Key", "value", "v"},
		"header without value":   {"header", "X-Key"},
		"incomplete oauth":       {"tokenUrl", "https://idp.example/token", "clientId", "gw"},
		"tokenUrl not a URL":     {"tokenUrl", "idp.example/token", "clientId", "gw", "clientSecret", "pw"},
		"reserved relay header":  {"header", "X-Go-Proxy-Assertion", "value", "forged"},
		"reserved, any spelling": {"header", "x-go-proxy-callback-url", "value", "http://evil"},
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			d := map[string][]byte{}
			for i := 0; i+1 < len(kv); i += 2 {
				d[kv[i]] = []byte(kv[i+1])
			}
			if _, err := parseCredential(d); err == nil {
				t.Errorf("parseCredential(%v) succeeded; want an error", kv)
			}
		})
	}
}

// TestCredentialOnlySentToAllowedHosts checks that a credential goes only
// to the hosts its Secret's AllowedHostsAnnotation lists: a request to any
// other host fails instead of carrying it there.
func TestCredentialOnlySentToAllowedHosts(t *testing.T) {
	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "token", "s3cret")
	withStore(t, NewStore(secrets.read, 0))
	var withCredential atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withCredential.Add(1)
		}
	}))
	t.Cleanup(srv.Close)
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")

	cases := []struct {
		hosts   string
		allowed bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.1:" + port, true},
		{"crm.example.com, 127.0.0.1:" + port, true},
		{"127.0.0.1:1", false},
		{"crm.example.com", false},
		{"", false}, // no annotation at all
	}
	for _, tc := range cases {
		secrets.setHosts("crm", tc.hosts)
		withCredential.Store(0)
		err := get(t, Client(&http.Client{}, "crm"), srv.URL)
		switch {
		case tc.allowed && (err != nil || withCredential.Load() != 1):
			t.Errorf("hosts %q: want the credential sent, got err=%v", tc.hosts, err)
		case !tc.allowed && err == nil:
			t.Errorf("hosts %q: request to %s succeeded; it should have been refused", tc.hosts, srv.URL)
		case !tc.allowed && withCredential.Load() != 0:
			t.Errorf("hosts %q: the credential reached the backend", tc.hosts)
		}
	}
}

// TestCredentialNotSentAcrossRedirect checks that a backend can't redirect
// a request, credential and all, to a host the Secret doesn't allow.
func TestCredentialNotSentAcrossRedirect(t *testing.T) {
	secrets := &fakeSecrets{data: map[string]map[string][]byte{}}
	secrets.set("crm", "token", "s3cret")
	withStore(t, NewStore(secrets.read, time.Minute))

	var reached atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			reached.Store(true)
		}
	}))
	t.Cleanup(other.Close)
	// "localhost" isn't in the allowed hosts (only 127.0.0.1 is), so the
	// redirect target counts as another host.
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherURL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	if err := get(t, Client(&http.Client{}, "crm"), redirector.URL); err == nil {
		t.Error("a redirect to a host the secret doesn't allow succeeded")
	}
	if reached.Load() {
		t.Error("the credential was sent to the redirect target")
	}
}

func TestInvalidAllowedHosts(t *testing.T) {
	for _, hosts := range []string{"http://crm.example.com", "crm.example.com/path", "user@crm.example.com", ":8080"} {
		if _, err := parseAllowedHosts(hosts); err == nil {
			t.Errorf("parseAllowedHosts(%q) = nil error, want one", hosts)
		}
	}
}

// TestSlowRefreshDoesNotBlockOtherCallers is the regression test for the
// Store holding its lock across the Secret read: while one caller's
// refresh hangs, the others go on with the last good credential.
func TestSlowRefreshDoesNotBlockOtherCallers(t *testing.T) {
	release := make(chan struct{})
	reads := 0
	var mu sync.Mutex
	read := func(_ context.Context, name string) (*Secret, error) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n > 1 {
			<-release // every refresh after the first hangs
		}
		return &Secret{Data: map[string][]byte{"token": []byte("s3cret")}, Annotations: map[string]string{AllowedHostsAnnotation: "127.0.0.1"}}, nil
	}
	s := NewStore(read, 0) // every call wants a refresh
	defer close(release)

	if _, err := s.credential(context.Background(), "crm"); err != nil {
		t.Fatal(err)
	}
	go s.credential(context.Background(), "crm") // starts a refresh that hangs
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := s.credential(context.Background(), "crm")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("credential = %v; want the last good one", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a caller was blocked behind another's hanging refresh")
	}
}
