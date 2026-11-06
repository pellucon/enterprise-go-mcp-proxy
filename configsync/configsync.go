// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package configsync provides a reusable ConfigMap-backed sync mechanism
// for any section of go-proxy's combined config: a ConfigMap (one per
// section, named "<section>-override") that go-proxy owns as sole writer,
// with change notification via a bare, payload-less NATS ping — never a
// live k8s API read on the hot path. Every reader (go-proxy's own runtime
// engines, and backend MCP servers) reads the current value the same way:
// from a locally mounted volume file. See CLAUDE.md's "Unify
// configuration into a single config-map mechanism" notes.
//
// Two independent pieces: Writer (proxy admin-tool handlers only, talks to
// the k8s API) and Watcher[T] (every reader, talks to a local file plus an
// optional NATS subscription). Both are silent no-ops when the k8s
// clientset/NATS connection they need isn't available, matching this
// repo's nil-pointer/absent-section-means-no-op convention throughout.
package configsync

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/natsclient"

	"github.com/nats-io/nats.go"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// VersionedConfig wraps a section's config with an optimistic-concurrency
// version. Version 0 is the implicit state of a section that has never
// been written to its override ConfigMap; the first real write produces
// version 1. _admin.export_<section> returns this whole envelope, and
// _admin.update_<section> takes it back unmodified except for edits to
// Config, so no separate "-version" flag/field is needed anywhere.
type VersionedConfig[T any] struct {
	Version int64 `json:"version"`
	Config  T     `json:"config"`
}

// ConfigSubject is the NATS subject a section's change ping is published/
// subscribed on. Deliberately not routed through the events package
// (events.Event/Dictionary/events.Client.Publish): the eventDictionary
// section is itself one of the things configsync watches, so using
// events.Client here would create a bootstrapping paradox. A configsync
// ping carries no payload — a subscriber always reacts by re-reading its
// own mounted file, never by trusting anything in the NATS message itself.
func ConfigSubject(section string) string {
	return "_system.config." + section
}

// overrideConfigMapName is the k8s ConfigMap name a section's Writer/
// Watcher-seeding logic reads/writes: "<section>-override".
func overrideConfigMapName(section string) string {
	return section + "-override"
}

// configMapDataKey is the single data key every override ConfigMap stores
// its VersionedConfig[T] JSON blob under.
const configMapDataKey = "config.json"

// k8sAttemptTimeout bounds a single k8s API call, matching
// dynamic_proxy's pollAttemptTimeout.
const k8sAttemptTimeout = 10 * time.Second

// k8s clientset/pod-namespace singletons, shared by every section's
// Writer/Watcher.
var (
	k8sClientset     *kubernetes.Clientset
	k8sClientsetOnce sync.Once
	k8sClientsetErr  error
)

// K8sClientset returns the shared in-cluster kubernetes clientset this
// package uses internally, or an error if not running in-cluster (the
// standard no-op-outside-k8s signal every configsync caller already
// checks for). Exported so a caller that needs k8s API access for a
// resource configsync doesn't itself manage — main.go's Secret-backed
// internal-signing-key bootstrap, see CLAUDE.md's "Internal identity
// assertions" section — can reuse the same singleton instead of dialing
// in-cluster config a second time.
func K8sClientset() (*kubernetes.Clientset, error) {
	return getK8sClientset()
}

// PodNamespace returns this pod's own namespace (read from the
// service-account volume every pod gets), the same way Writer[T]/Watcher[T]
// resolve which namespace to read/write ConfigMaps in. See K8sClientset's
// doc comment for why this is exported.
func PodNamespace() (string, error) {
	return podNamespace()
}

func getK8sClientset() (*kubernetes.Clientset, error) {
	k8sClientsetOnce.Do(func() {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			k8sClientsetErr = fmt.Errorf("configsync: failed to load in-cluster config (are you running inside a k8s pod?): %w", err)
			return
		}
		clientset, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			k8sClientsetErr = fmt.Errorf("configsync: failed to build clientset: %w", err)
			return
		}
		k8sClientset = clientset
	})
	return k8sClientset, k8sClientsetErr
}

var (
	podNamespaceOnce  sync.Once
	podNamespaceValue string
	podNamespaceErr   error
	podNamespaceFile  = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

func podNamespace() (string, error) {
	podNamespaceOnce.Do(func() {
		raw, err := os.ReadFile(podNamespaceFile)
		if err != nil {
			podNamespaceErr = fmt.Errorf("configsync: failed to read pod namespace from %s: %w", podNamespaceFile, err)
			return
		}
		podNamespaceValue = string(raw)
	})
	return podNamespaceValue, podNamespaceErr
}

func attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, k8sAttemptTimeout)
}

// Connect dials natsURL for configsync's own use (change-notification pings
// and subscriptions), independent of any events.Client connection a caller
// may separately hold — the two packages don't share a connection, mirror
// the separation between their concerns (see this package's doc comment).
// Empty natsURL means "not configured": returns (nil, nil), letting every
// Writer/Watcher constructed with the result silently skip the NATS half of
// its work, per this package's no-op-outside-k8s-or-NATS convention.
//
// RetryOnFailedConnect mirrors events.Client.NewClient's identical
// rationale: a pod can start before NATS has finished rolling out, and
// without it the very first Connect attempt fails outright with no retry.
func Connect(natsURL string) (*nats.Conn, error) {
	if natsURL == "" {
		return nil, nil
	}
	authOpts, err := natsclient.Options()
	if err != nil {
		return nil, err
	}
	conn, err := nats.Connect(natsURL, append([]nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logging.Errorf("configsync: disconnected from NATS at %s: %v", natsURL, err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logging.Debugf("configsync: (re)connected to NATS at %s", nc.ConnectedUrl())
		}),
	}, authOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("configsync: failed to connect to NATS at %s: %w", natsURL, err)
	}
	return conn, nil
}
