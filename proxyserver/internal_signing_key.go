// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"crypto/rsa"
	"fmt"

	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// internalSigningKeySecretName is the k8s Secret go-proxy itself creates —
// unlike every other Secret in this repo (jwt-public-key, formerly
// go-proxy-internal-signing-key/internal-auth-public-key), which
// scripts/deploy.sh provisioned from a local file, this one is written by
// go-proxy at runtime and never touches disk outside the cluster. Kept
// under its pre-existing name so no separate migration/rename is needed
// for anyone who already has it deployed.
const internalSigningKeySecretName = "go-proxy-internal-signing-key"

// internalSigningKeySecretDataKey is the Secret's one data key, holding a
// PKCS8 PEM-encoded RSA private key (see auth.EncodePrivateKeyPEM).
const internalSigningKeySecretDataKey = "private.pem"

// bootstrapInternalSigningKey resolves the RSA private key this proxy
// instance signs internal identity assertions with (see CLAUDE.md's
// "Internal identity assertions" section) — called once from doRunOn
// whenever the config's "internalAuth" section is present.
//
// Outside k8s (a standalone binary, or any hermetic test —
// configsync.K8sClientset returning an error is the same no-op-outside-k8s
// signal every other configsync caller already checks for), this process
// just generates and keeps its own key: there's only one such process, so
// there's nothing to coordinate.
//
// Inside k8s, every replica must sign with the SAME key: a backend
// verifies a relayed call's assertion against one JWKS endpoint
// (/.well-known/jwks-internal.json) that's only ever reachable through
// whichever pod happens to serve that particular request, so a replica
// that signed with a key no other replica's JWKS endpoint would ever
// serve would produce assertions no backend could reliably verify. The
// first replica to start creates internalSigningKeySecretName holding a
// freshly generated key; every other replica (including this one, on a
// later restart) reads that same Secret back. A create race is resolved
// by falling back to a Get on AlreadyExists — the same
// create-or-lose-the-race idiom configsync's own updateK8s already relies
// on for its ConfigMaps, just simpler here since there's no
// optimistic-concurrency version to reconcile, only ever a single winner.
//
// A fresh keypair is generated only when this Secret doesn't already
// exist — i.e. on a deploy that creates the namespace/Secret from
// scratch, or after it's deliberately deleted. A rolling restart of
// existing pods reuses the same key on purpose: rotating it mid-rollout
// would invalidate assertions already minted by pods still finishing
// in-flight requests, and relayed calls presenting an assertion signed by
// a pod that has since restarted would start failing verification against
// pods now serving a different key.
func bootstrapInternalSigningKey(ctx context.Context) (*rsa.PrivateKey, error) {
	clientset, err := configsync.K8sClientset()
	if err != nil {
		return auth.GenerateKey()
	}
	namespace, err := configsync.PodNamespace()
	if err != nil {
		return nil, err
	}
	secrets := clientset.CoreV1().Secrets(namespace)

	if secret, err := secrets.Get(ctx, internalSigningKeySecretName, metav1.GetOptions{}); err == nil {
		return auth.ParsePrivateKeyPEM(secret.Data[internalSigningKeySecretDataKey])
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to read %s secret: %w", internalSigningKeySecretName, err)
	}

	key, err := auth.GenerateKey()
	if err != nil {
		return nil, err
	}
	keyPEM, err := auth.EncodePrivateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: internalSigningKeySecretName, Namespace: namespace},
		Data:       map[string][]byte{internalSigningKeySecretDataKey: keyPEM},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		secret, getErr := secrets.Get(ctx, internalSigningKeySecretName, metav1.GetOptions{})
		if getErr != nil {
			return nil, fmt.Errorf("failed to read %s secret after losing the create race: %w", internalSigningKeySecretName, getErr)
		}
		return auth.ParsePrivateKeyPEM(secret.Data[internalSigningKeySecretDataKey])
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create %s secret: %w", internalSigningKeySecretName, err)
	}
	return key, nil
}
