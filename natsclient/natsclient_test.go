// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package natsclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// components are the NATS users in k8s/nats/nats.conf.tmpl.
var components = []string{"go-proxy", "event-aggregator", "declarative-guardrail", "rate-limiter", "ai-powered-tools"}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeTLS writes a CA and a server certificate for 127.0.0.1/localhost
// into dir, returning the CA's path.
func writeTLS(t *testing.T, dir string) string {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-nats-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "nats"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(serverKey)
	writePEM(t, filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER)
	writePEM(t, filepath.Join(dir, "tls.crt"), "CERTIFICATE", serverDER)
	writePEM(t, filepath.Join(dir, "tls.key"), "EC PRIVATE KEY", keyDER)
	return filepath.Join(dir, "ca.crt")
}

// startConfiguredServer renders the real k8s/nats/nats.conf.tmpl the way
// deploy.sh does (password "pw-<user>" for each user), points its TLS paths
// at a test certificate, and starts NATS with it on a free port.
func startConfiguredServer(t *testing.T) (url, caPath string) {
	t.Helper()
	dir := t.TempDir()
	caPath = writeTLS(t, dir)
	tmpl, err := os.ReadFile("../k8s/nats/nats.conf.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	conf := string(tmpl)
	for _, user := range components {
		conf = strings.ReplaceAll(conf, "@@password:"+user+"@@", "pw-"+user)
	}
	if strings.Contains(conf, `"@@password:`) {
		t.Fatal("template has a placeholder for a user this test doesn't know")
	}
	conf = strings.ReplaceAll(conf, "/etc/nats/tls/", dir+"/")
	confPath := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := natsserver.ProcessConfigFile(confPath)
	if err != nil {
		t.Fatalf("NATS rejects the rendered config: %v", err)
	}
	opts.Host, opts.Port, opts.NoLog, opts.NoSigs = "127.0.0.1", -1, true, true
	if opts.HTTPPort != 8222 {
		t.Errorf("the template's monitoring port is %d; the NATS Deployment's probes use 8222", opts.HTTPPort)
	}
	opts.HTTPPort = 0 // don't open the monitoring port on the test machine
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(4 * time.Second) {
		t.Fatal("NATS not ready")
	}
	t.Cleanup(ns.Shutdown)
	return ns.ClientURL(), caPath
}

// credentialsDir writes a credentials directory the way deploy.sh's
// nats-client-<user> Secret is mounted.
func credentialsDir(t *testing.T, user, password, caPath string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "user"), []byte(user), 0o600)
	os.WriteFile(filepath.Join(dir, "password"), []byte(password), 0o600)
	if caPath != "" {
		ca, _ := os.ReadFile(caPath)
		os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600)
	}
	return dir
}

// permissionErrors collects the asynchronous permission violations NATS
// reports for a connection.
type permissionErrors struct {
	mu   sync.Mutex
	errs []string
}

func (p *permissionErrors) handler(_ *nats.Conn, _ *nats.Subscription, err error) {
	p.mu.Lock()
	p.errs = append(p.errs, err.Error())
	p.mu.Unlock()
}

func (p *permissionErrors) has(subject string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.errs {
		if strings.Contains(e, "Permissions Violation") && strings.Contains(e, subject) {
			return true
		}
	}
	return false
}

func connectAs(t *testing.T, url, user, caPath string) (*nats.Conn, *permissionErrors) {
	t.Helper()
	opts, err := OptionsFrom(credentialsDir(t, user, "pw-"+user, caPath))
	if err != nil {
		t.Fatal(err)
	}
	perr := &permissionErrors{}
	nc, err := nats.Connect(url, append(opts, nats.ErrorHandler(perr.handler))...)
	if err != nil {
		t.Fatalf("connecting as %s: %v", user, err)
	}
	t.Cleanup(nc.Close)
	return nc, perr
}

// settle flushes nc and gives NATS a moment to report asynchronous
// permission violations.
func settle(t *testing.T, nc *nats.Conn) {
	t.Helper()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
}

// TestNATSConfigRefusesUnauthenticatedClients checks the deployed config's
// authentication: no anonymous, wrong-password, or plain-text clients.
func TestNATSConfigRefusesUnauthenticatedClients(t *testing.T) {
	url, caPath := startConfiguredServer(t)

	if nc, err := nats.Connect(url, nats.RootCAs(caPath)); err == nil {
		nc.Close()
		t.Error("an anonymous client was accepted")
	}
	opts, _ := OptionsFrom(credentialsDir(t, "go-proxy", "wrong", caPath))
	if nc, err := nats.Connect(url, opts...); err == nil {
		nc.Close()
		t.Error("a wrong password was accepted")
	}
	opts, _ = OptionsFrom(credentialsDir(t, "go-proxy", "pw-go-proxy", "")) // no CA: no TLS
	if nc, err := nats.Connect(url, opts...); err == nil {
		nc.Close()
		t.Error("a client without TLS was accepted")
	}
}

// TestNATSConfigPermissions checks that each user can do exactly what its
// component needs: only go-proxy publishes events, only event-aggregator
// reads them, and the others only hear their own config pings.
func TestNATSConfigPermissions(t *testing.T) {
	url, caPath := startConfiguredServer(t)

	aggregator, aggErrs := connectAs(t, url, "event-aggregator", caPath)
	received := make(chan string, 10)
	if _, err := aggregator.Subscribe("events.>", func(m *nats.Msg) { received <- m.Subject }); err != nil {
		t.Fatal(err)
	}
	settle(t, aggregator)

	proxy, proxyErrs := connectAs(t, url, "go-proxy", caPath)
	if _, err := proxy.Subscribe("_system.config.access-rules", func(*nats.Msg) {}); err != nil {
		t.Fatal(err)
	}
	proxy.Publish("events.alert.tool-call-blocked", []byte(`{}`))
	proxy.Publish("_system.config.access-rules", nil)
	settle(t, proxy)
	select {
	case subject := <-received:
		if subject != "events.alert.tool-call-blocked" {
			t.Errorf("event-aggregator received %q", subject)
		}
	case <-time.After(2 * time.Second):
		t.Error("event-aggregator didn't receive go-proxy's event")
	}
	if proxyErrs.has("events.") || proxyErrs.has("_system.config") {
		t.Errorf("go-proxy was refused something it needs: %v", proxyErrs.errs)
	}

	// event-aggregator can't forge events.
	aggregator.Publish("events.alert.tool-call-blocked", []byte(`{"forged":true}`))
	settle(t, aggregator)
	if !aggErrs.has("events.alert.tool-call-blocked") {
		t.Error("event-aggregator was allowed to publish an event")
	}

	// The other components can't read events, nor another section's pings,
	// but do hear their own.
	for user, own := range map[string]string{
		"declarative-guardrail": "_system.config.declarative-guardrails",
		"rate-limiter":          "_system.config.rate-limiting",
		"ai-powered-tools":      "_system.config.log-level",
	} {
		nc, errs := connectAs(t, url, user, caPath)
		nc.Subscribe("events.>", func(*nats.Msg) {})
		nc.Subscribe("_system.config.access-rules", func(*nats.Msg) {})
		if _, err := nc.Subscribe(own, func(*nats.Msg) {}); err != nil {
			t.Fatal(err)
		}
		nc.Publish(own, nil)
		settle(t, nc)
		for _, subject := range []string{"events.>", "_system.config.access-rules", own} {
			denied := errs.has(subject)
			wantDenied := subject != own
			if subject == own {
				// Subscribing to its own section is allowed; publishing
				// to it isn't (only go-proxy sends config pings).
				denied = errs.has("Publish to \"" + own)
				wantDenied = true
			}
			if denied != wantDenied {
				t.Errorf("%s on %s: denied=%v, want %v (errors: %v)", user, subject, denied, wantDenied, errs.errs)
			}
		}
	}
}

func TestOptionsFrom(t *testing.T) {
	if opts, err := OptionsFrom(""); err != nil || opts != nil {
		t.Errorf("no directory: got %v, %v; want no options", opts, err)
	}
	if opts, err := OptionsFrom(t.TempDir()); err != nil || opts != nil {
		t.Errorf("an empty directory: got %v, %v; want no options (anonymous)", opts, err)
	}
	userOnly := t.TempDir()
	os.WriteFile(filepath.Join(userOnly, "user"), []byte("u"), 0o600)
	if _, err := OptionsFrom(userOnly); err == nil {
		t.Error("a user without a password was accepted")
	}
	if opts, err := OptionsFrom(credentialsDir(t, "u", "p", "")); err != nil || len(opts) != 1 {
		t.Errorf("credentials without a CA: got %d options, %v; want just the credentials", len(opts), err)
	}
}
