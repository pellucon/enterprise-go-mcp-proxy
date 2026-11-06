// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"github.com/nats-io/nats.go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Writer is the admin-tool-side half of a section's config map: given an
// already-validated new value, it persists it to the section's
// "<section>-override" ConfigMap (via the k8s API) and pings every Watcher
// via NATS. Used only by go-proxy's own admin-tool handlers (admin_tools.go)
// — never by a backend server.
//
// Outside k8s, Update/Export fall back to an in-process value guarded by
// the same mutex, so the version-mismatch/optimistic-concurrency contract
// still holds standalone (and in hermetic tests) — just without any
// cross-replica propagation, matching this package's no-op-outside-k8s
// convention.
type Writer[T any] struct {
	section string
	nc      *nats.Conn // nil: NATS integration disabled, ping is a no-op

	localMu      sync.Mutex
	localPresent bool
	localVersion int64
	localConfig  T
}

// NewWriter creates a Writer for section. nc may be nil (NATS not
// configured), in which case Update still persists to k8s (if available)
// but never pings any Watcher — every Watcher for that section then only
// ever sees the value it loaded at construction, exactly as if configsync
// weren't wired in at all.
//
// initial seeds the local (outside-k8s) fallback at version 1, mirroring
// what Reconcile does in k8s by seeding the section's override ConfigMap
// from proxy-server.cfg the first time any replica starts — without this, a fresh standalone process's first export_<section> call,
// before any update_<section> has ever run, would report version 0 and an
// empty Config instead of the rules the process actually started with. Pass
// nil for a section with no meaningful "value the process already has"
// (there is none here — every section is always parsed from
// proxy-server.cfg, so callers always have one to pass).
func NewWriter[T any](section string, nc *nats.Conn, initial *T) *Writer[T] {
	w := &Writer[T]{section: section, nc: nc}
	if initial != nil {
		w.localPresent = true
		w.localVersion = 1
		w.localConfig = *initial
	}
	return w
}

// cfgHashKey is the override ConfigMap data key holding its baseline: a
// hash of the cfg section the live config last agreed with (see Reconcile).
// A key of its own, beside configMapDataKey, so the export/update envelope
// is unchanged and an admin update, which rewrites only configMapDataKey,
// carries the baseline over untouched.
const cfgHashKey = "cfg-hash"

// DriftError is Reconcile's refusal: the deployed cfg's section was changed
// since the live config last agreed with it, and differs from the live
// config.
type DriftError struct {
	Section string
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("configsync: %s: the deployed cfg changes this section, which differs from the live config", e.Section)
}

// Reconcile checks this section's deployed cfg value (NewWriter's initial)
// against its live "<section>-override" ConfigMap at startup. Once a
// section is seeded, the override is authoritative and the cfg is never
// applied over it, so a cfg edit that differs from the live config would
// otherwise be silently ignored - a revocation made in the cfg would never
// take effect. The ConfigMap's baseline (cfgHashKey) records the cfg
// section the live config last agreed with, which tells the two ways they
// can differ apart:
//
//   - no override yet: it's created from the cfg (version 1), with the cfg
//     as its baseline - the one-time bootstrap;
//   - the cfg is unchanged since the baseline: the live config moved on
//     through admin updates, which is how a change is made. Allowed, with a
//     warning that the cfg is behind and should be regenerated;
//   - the cfg changed and now matches the live config (a cfg regenerated
//     from the live config): allowed, and it becomes the new baseline;
//   - the cfg changed and differs from the live config: a *DriftError.
//
// parse is the section's own parser. Both values are normalized through it
// before comparing (and hashing), so defaults it fills in, or field order,
// never count as a difference. An override without a baseline adopts the
// current cfg as its baseline. Safe to call from every replica at once. A
// no-op outside k8s, where there's no override.
func (w *Writer[T]) Reconcile(ctx context.Context, parse func([]byte) (*T, error)) error {
	w.localMu.Lock()
	var initial *T
	if w.localPresent {
		config := w.localConfig
		initial = &config
	}
	w.localMu.Unlock()
	clientset, err := getK8sClientset()
	if err != nil {
		return nil
	}
	namespace, err := podNamespace()
	if err != nil {
		return err
	}
	attemptCtx, cancel := attemptContext(ctx)
	defer cancel()
	return reconcileK8s(attemptCtx, clientset, namespace, w.section, initial, parse)
}

func reconcileK8s[T any](ctx context.Context, clientset kubernetes.Interface, namespace, section string, initial *T, parse func([]byte) (*T, error)) error {
	name := overrideConfigMapName(section)
	configMaps := clientset.CoreV1().ConfigMaps(namespace)

	cfgCanonical := ""
	if initial != nil {
		raw, err := json.Marshal(initial)
		if err != nil {
			return fmt.Errorf("configsync: %s: failed to marshal the cfg's value: %w", section, err)
		}
		if cfgCanonical, err = canonical(raw, parse); err != nil {
			return fmt.Errorf("configsync: %s: the cfg's value doesn't re-parse: %w", section, err)
		}
	}
	cfgHash := hashOf(cfgCanonical)

	existing, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if initial == nil {
			return nil // nothing in the cfg to seed
		}
		raw, err := json.Marshal(VersionedConfig[T]{Version: 1, Config: *initial})
		if err != nil {
			return fmt.Errorf("configsync: failed to marshal seed config: %w", err)
		}
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       map[string]string{configMapDataKey: string(raw), cfgHashKey: cfgHash},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return nil // another replica won the race -- its seed is equally valid
		}
		if err != nil {
			return fmt.Errorf("configsync: failed to seed override ConfigMap %s/%s: %w", namespace, name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("configsync: failed to read override ConfigMap %s/%s: %w", namespace, name, err)
	}

	var live rawVersionedConfig
	if err := json.Unmarshal([]byte(existing.Data[configMapDataKey]), &live); err != nil {
		return fmt.Errorf("configsync: failed to parse override ConfigMap %s/%s: %w", namespace, name, err)
	}
	liveCanonical, err := canonical(live.Config, parse)
	if err != nil {
		return fmt.Errorf("configsync: %s: override ConfigMap %s/%s failed validation: %w", section, namespace, name, err)
	}

	baseline := existing.Data[cfgHashKey]
	if baseline == "" {
		// Written before baselines existed: take the deployed cfg as it.
		baseline = cfgHash
		writeBaseline(ctx, configMaps, existing, section, cfgHash)
	}
	switch {
	case cfgHash == baseline:
		if cfgCanonical != liveCanonical {
			logging.Warnf("[configsync] %s: the live config (version %d) has moved on from the deployed cfg through admin updates; the live config is in effect. Regenerate the cfg (_admin.export_full_config) so it matches.", section, live.Version)
		}
		return nil
	case cfgCanonical == liveCanonical:
		writeBaseline(ctx, configMaps, existing, section, cfgHash)
		return nil
	default:
		return &DriftError{Section: section}
	}
}

// writeBaseline records cfgHash as existing's baseline. A failure is only
// logged: startup goes on, and the next start compares against the old
// baseline again.
func writeBaseline(ctx context.Context, configMaps corev1client.ConfigMapInterface, existing *corev1.ConfigMap, section, cfgHash string) {
	updated := existing.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string]string{}
	}
	updated.Data[cfgHashKey] = cfgHash
	if _, err := configMaps.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		logging.Errorf("[configsync] %s: failed to record the cfg baseline: %v", section, err)
	}
}

// canonical is raw normalized by parse and re-encoded, so two values that
// parse the same compare equal.
func canonical[T any](raw []byte, parse func([]byte) (*T, error)) (string, error) {
	parsed, err := parse(raw)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func hashOf(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// Export returns the section's current version+config: in k8s, a fresh GET
// of the override ConfigMap (absent entirely => version 0, zero Config,
// the state before a section is ever written); outside k8s, the local
// fallback value.
func (w *Writer[T]) Export(ctx context.Context) (VersionedConfig[T], error) {
	clientset, err := getK8sClientset()
	if err != nil {
		return w.exportLocal(), nil
	}
	namespace, err := podNamespace()
	if err != nil {
		return VersionedConfig[T]{}, err
	}
	attemptCtx, cancel := attemptContext(ctx)
	defer cancel()
	return exportK8s[T](attemptCtx, clientset, namespace, w.section)
}

func exportK8s[T any](ctx context.Context, clientset kubernetes.Interface, namespace, section string) (VersionedConfig[T], error) {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, overrideConfigMapName(section), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return VersionedConfig[T]{}, nil // never written: implicit version 0
	}
	if err != nil {
		return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to read override ConfigMap %s/%s: %w", namespace, overrideConfigMapName(section), err)
	}
	raw, ok := cm.Data[configMapDataKey]
	if !ok {
		return VersionedConfig[T]{}, nil
	}
	var vc VersionedConfig[T]
	if err := json.Unmarshal([]byte(raw), &vc); err != nil {
		return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to parse override ConfigMap %s/%s: %w", namespace, overrideConfigMapName(section), err)
	}
	return vc, nil
}

func (w *Writer[T]) exportLocal() VersionedConfig[T] {
	w.localMu.Lock()
	defer w.localMu.Unlock()
	if !w.localPresent {
		return VersionedConfig[T]{}
	}
	return VersionedConfig[T]{Version: w.localVersion, Config: w.localConfig}
}

// Update writes {Version: current+1, Config: in.Config} and returns it, but
// only if in.Version matches the section's actual current version — read
// fresh (never from any cached/in-memory copy) as part of this same call,
// since another replica or another admin call may have updated it since
// this replica last saw a NATS ping. A mismatch is rejected with a
// descriptive error and no write is attempted.
//
// In k8s, the write uses the ConfigMap object's resourceVersion from that
// same fresh GET, so a concurrent writer racing between the GET and this
// Update's own write fails the k8s API's own optimistic-concurrency check
// outright — surfaced to the caller as an error, not silently retried.
// Outside k8s, the equivalent check-then-swap happens against the local
// fallback value under w.localMu, which is equally race-free.
func (w *Writer[T]) Update(ctx context.Context, in VersionedConfig[T]) (VersionedConfig[T], error) {
	clientset, err := getK8sClientset()
	if err != nil {
		out, err := w.updateLocal(in)
		if err != nil {
			return VersionedConfig[T]{}, err
		}
		// Pinging here too (not just the k8s branch below) matters for a
		// real, supported deployment shape: NATS configured without k8s
		// (e.g. plain docker-compose) still wants live reload across
		// whatever processes share that NATS server, even though there's
		// no ConfigMap for them to actually read back — see Watcher's own
		// "no path" no-op convention for why a Watcher with nothing to
		// read simply ignores the ping.
		w.ping()
		return out, nil
	}
	namespace, err := podNamespace()
	if err != nil {
		return VersionedConfig[T]{}, err
	}
	attemptCtx, cancel := attemptContext(ctx)
	defer cancel()
	out, err := updateK8s(attemptCtx, clientset, namespace, w.section, in)
	if err != nil {
		return VersionedConfig[T]{}, err
	}
	w.ping()
	return out, nil
}

func updateK8s[T any](ctx context.Context, clientset kubernetes.Interface, namespace, section string, in VersionedConfig[T]) (VersionedConfig[T], error) {
	name := overrideConfigMapName(section)
	configMaps := clientset.CoreV1().ConfigMaps(namespace)

	existing, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if in.Version != 0 {
			return VersionedConfig[T]{}, fmt.Errorf("configsync: version mismatch: config map %q is at version 0, update targeted version %d — export again and retry", section, in.Version)
		}
		out := VersionedConfig[T]{Version: 1, Config: in.Config}
		raw, merr := json.Marshal(out)
		if merr != nil {
			return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to marshal config: %w", merr)
		}
		if _, err := configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       map[string]string{configMapDataKey: string(raw)},
		}, metav1.CreateOptions{}); err != nil {
			return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to create override ConfigMap %s/%s: %w", namespace, name, err)
		}
		return out, nil
	}
	if err != nil {
		return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to read override ConfigMap %s/%s: %w", namespace, name, err)
	}

	var current VersionedConfig[T]
	if raw, ok := existing.Data[configMapDataKey]; ok {
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to parse override ConfigMap %s/%s: %w", namespace, name, err)
		}
	}
	if in.Version != current.Version {
		return VersionedConfig[T]{}, fmt.Errorf("configsync: version mismatch: config map %q is at version %d, update targeted version %d — export again and retry", section, current.Version, in.Version)
	}

	out := VersionedConfig[T]{Version: current.Version + 1, Config: in.Config}
	raw, err := json.Marshal(out)
	if err != nil {
		return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to marshal config: %w", err)
	}
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	existing.Data[configMapDataKey] = string(raw)
	if _, err := configMaps.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return VersionedConfig[T]{}, fmt.Errorf("configsync: failed to update override ConfigMap %s/%s (possibly a concurrent writer — export again and retry): %w", namespace, name, err)
	}
	return out, nil
}

func (w *Writer[T]) updateLocal(in VersionedConfig[T]) (VersionedConfig[T], error) {
	w.localMu.Lock()
	defer w.localMu.Unlock()

	if in.Version != w.localVersion {
		return VersionedConfig[T]{}, fmt.Errorf("configsync: version mismatch: config map %q is at version %d, update targeted version %d — export again and retry", w.section, w.localVersion, in.Version)
	}
	w.localVersion++
	w.localPresent = true
	w.localConfig = in.Config
	return VersionedConfig[T]{Version: w.localVersion, Config: w.localConfig}, nil
}

// ping publishes a bare, payload-less message on this section's config
// subject. A no-op when NATS isn't configured (w.nc == nil) or the publish
// itself fails — a missed ping only delays convergence until the next
// admin update, it never corrupts state, so it's not treated as a fatal
// error for the Update call that triggered it.
func (w *Writer[T]) ping() {
	if w.nc == nil {
		return
	}
	_ = w.nc.Publish(ConfigSubject(w.section), nil)
}
