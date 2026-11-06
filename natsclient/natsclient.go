// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package natsclient holds the connection options every NATS client in this
// repo uses (events.NewClient, configsync.Connect): the component's own
// credentials, and the CA to verify the NATS server's TLS certificate with.
//
// In Kubernetes, NATS refuses anonymous connections and gives each
// component a user that may publish and subscribe only to the subjects it
// needs (see k8s/nats/nats.conf.tmpl); scripts/deploy.sh puts each
// component's credentials in its own Secret, mounted at
// $NATS_CREDENTIALS_DIR. Without that variable - a standalone run, or a
// test's embedded server - the client connects anonymously and without
// TLS, which an authenticated server refuses (fail closed).
package natsclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"github.com/nats-io/nats.go"
)

// CredentialsDirEnv names the directory holding the component's NATS
// credentials: files "user" and "password", and "ca.crt", the CA the NATS
// server's TLS certificate is checked against.
const CredentialsDirEnv = "NATS_CREDENTIALS_DIR"

// Options returns the credentials and TLS options for connecting to NATS,
// from the directory $NATS_CREDENTIALS_DIR names; none if it's unset.
func Options() ([]nats.Option, error) {
	return OptionsFrom(os.Getenv(CredentialsDirEnv))
}

// OptionsFrom is Options for an explicit credentials directory ("" means
// none). A directory with no "user" file (e.g. an optional Secret that
// wasn't created, which Kubernetes mounts as an empty directory) also means
// none: the client connects anonymously, which an authenticated server
// refuses - the server, not this client, is what enforces authentication.
// With "user", "password" is required too; ca.crt, if present, makes the
// connection require TLS, verified against that CA only.
func OptionsFrom(dir string) ([]nats.Option, error) {
	if dir == "" {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "user")); os.IsNotExist(err) {
		logging.Warnf("natsclient: no NATS credentials in %s; connecting anonymously", dir)
		return nil, nil
	}
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("natsclient: reading %s from %s: %w", name, dir, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("natsclient: %s in %s is empty", name, dir)
		}
		return v, nil
	}
	user, err := read("user")
	if err != nil {
		return nil, err
	}
	password, err := read("password")
	if err != nil {
		return nil, err
	}
	opts := []nats.Option{nats.UserInfo(user, password)}

	caPath := filepath.Join(dir, "ca.crt")
	if _, err := os.Stat(caPath); err == nil {
		opts = append(opts, nats.RootCAs(caPath))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("natsclient: %w", err)
	}
	return opts, nil
}
