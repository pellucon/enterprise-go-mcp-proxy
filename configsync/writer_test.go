// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package configsync

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const testNamespace = "mcp-proxy"

type testConfig struct {
	Rules []string `json:"rules"`
}

func TestUpdateK8s_CreatesWhenAbsent(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	out, err := updateK8s(context.Background(), clientset, testNamespace, "widgets", VersionedConfig[testConfig]{Version: 0, Config: testConfig{Rules: []string{"a"}}})
	if err != nil {
		t.Fatalf("updateK8s: %v", err)
	}
	if out.Version != 1 {
		t.Errorf("expected first write to produce version 1, got %d", out.Version)
	}

	cm, err := clientset.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), "widgets-override", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap to have been created: %v", err)
	}
	var stored VersionedConfig[testConfig]
	if err := json.Unmarshal([]byte(cm.Data[configMapDataKey]), &stored); err != nil {
		t.Fatalf("failed to parse stored config: %v", err)
	}
	if stored.Version != 1 || len(stored.Config.Rules) != 1 {
		t.Errorf("unexpected stored config: %+v", stored)
	}
}

func TestUpdateK8s_RejectsWrongInitialVersion(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	_, err := updateK8s(context.Background(), clientset, testNamespace, "widgets", VersionedConfig[testConfig]{Version: 5, Config: testConfig{}})
	if err == nil {
		t.Fatal("expected an error when a first-ever write doesn't target version 0")
	}
}

func TestUpdateK8s_UpdatesAndIncrementsVersion(t *testing.T) {
	raw, _ := json.Marshal(VersionedConfig[testConfig]{Version: 1, Config: testConfig{Rules: []string{"a"}}})
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets-override", Namespace: testNamespace},
		Data:       map[string]string{configMapDataKey: string(raw)},
	})

	out, err := updateK8s(context.Background(), clientset, testNamespace, "widgets", VersionedConfig[testConfig]{Version: 1, Config: testConfig{Rules: []string{"a", "b"}}})
	if err != nil {
		t.Fatalf("updateK8s: %v", err)
	}
	if out.Version != 2 || len(out.Config.Rules) != 2 {
		t.Errorf("unexpected output: %+v", out)
	}
}

func TestUpdateK8s_RejectsStaleVersion(t *testing.T) {
	raw, _ := json.Marshal(VersionedConfig[testConfig]{Version: 3, Config: testConfig{Rules: []string{"a"}}})
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets-override", Namespace: testNamespace},
		Data:       map[string]string{configMapDataKey: string(raw)},
	})

	_, err := updateK8s(context.Background(), clientset, testNamespace, "widgets", VersionedConfig[testConfig]{Version: 1, Config: testConfig{Rules: []string{"stale"}}})
	if err == nil {
		t.Fatal("expected a version-mismatch error")
	}

	cm, _ := clientset.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), "widgets-override", metav1.GetOptions{})
	var stored VersionedConfig[testConfig]
	json.Unmarshal([]byte(cm.Data[configMapDataKey]), &stored)
	if stored.Version != 3 {
		t.Errorf("expected the rejected update to leave the config map untouched, got version %d", stored.Version)
	}
}

func TestExportK8s_NotFoundIsVersionZero(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	out, err := exportK8s[testConfig](context.Background(), clientset, testNamespace, "widgets")
	if err != nil {
		t.Fatalf("exportK8s: %v", err)
	}
	if out.Version != 0 {
		t.Errorf("expected version 0 for a never-written section, got %d", out.Version)
	}
}

func TestWriter_UpdateLocal_VersioningAndMismatch(t *testing.T) {
	// No in-cluster config in this test process, so Writer.Update/Export
	// fall back to the local path exercised here.
	w := NewWriter[testConfig]("widgets", nil, nil)

	exported, err := w.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if exported.Version != 0 {
		t.Errorf("expected initial version 0, got %d", exported.Version)
	}

	out, err := w.Update(context.Background(), VersionedConfig[testConfig]{Version: 0, Config: testConfig{Rules: []string{"a"}}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Version != 1 {
		t.Errorf("expected version 1 after first update, got %d", out.Version)
	}

	if _, err := w.Update(context.Background(), VersionedConfig[testConfig]{Version: 0, Config: testConfig{Rules: []string{"stale"}}}); err == nil {
		t.Fatal("expected a stale version to be rejected")
	}

	exported, err = w.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if exported.Version != 1 || len(exported.Config.Rules) != 1 {
		t.Errorf("expected the rejected update to leave state untouched, got %+v", exported)
	}
}

// parseNormalizingTestConfig is testConfig's parser, normalizing like a
// real section parser does: an absent rules list becomes empty.
func parseNormalizingTestConfig(raw []byte) (*testConfig, error) {
	var c testConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Rules == nil {
		c.Rules = []string{}
	}
	return &c, nil
}

// overrideWithBaseline is a widgets-override ConfigMap holding live at
// version, with baseline as its recorded cfg (nil: no baseline recorded).
func overrideWithBaseline(t *testing.T, version int64, live testConfig, baseline *testConfig) *corev1.ConfigMap {
	t.Helper()
	raw, _ := json.Marshal(VersionedConfig[testConfig]{Version: version, Config: live})
	data := map[string]string{configMapDataKey: string(raw)}
	if baseline != nil {
		b, _ := json.Marshal(baseline)
		c, err := canonical(b, parseNormalizingTestConfig)
		if err != nil {
			t.Fatal(err)
		}
		data[cfgHashKey] = hashOf(c)
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "widgets-override", Namespace: testNamespace}, Data: data}
}

func storedOverride(t *testing.T, clientset *fake.Clientset) (VersionedConfig[testConfig], string) {
	t.Helper()
	cm, err := clientset.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), "widgets-override", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the override: %v", err)
	}
	var stored VersionedConfig[testConfig]
	if err := json.Unmarshal([]byte(cm.Data[configMapDataKey]), &stored); err != nil {
		t.Fatalf("failed to parse stored config: %v", err)
	}
	return stored, cm.Data[cfgHashKey]
}

func reconcile(clientset *fake.Clientset, cfg *testConfig) error {
	return reconcileK8s(context.Background(), clientset, testNamespace, "widgets", cfg, parseNormalizingTestConfig)
}

func TestReconcileK8s_SeedsWhenAbsent(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if err := reconcile(clientset, &testConfig{Rules: []string{"seeded"}}); err != nil {
		t.Fatalf("reconcileK8s: %v", err)
	}
	stored, baseline := storedOverride(t, clientset)
	if stored.Version != 1 || len(stored.Config.Rules) != 1 || stored.Config.Rules[0] != "seeded" {
		t.Errorf("unexpected stored config: %+v", stored)
	}
	if baseline == "" {
		t.Error("the seed recorded no baseline")
	}
}

func TestReconcileK8s_NoSeedWithoutCfgValue(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if err := reconcile(clientset, nil); err != nil {
		t.Fatalf("reconcileK8s: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), "widgets-override", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a section absent from the cfg was seeded (err=%v)", err)
	}
}

func TestReconcileK8s_TreatsCreateRaceAsSuccess(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("configmaps"), "widgets-override")
	})
	if err := reconcile(clientset, &testConfig{Rules: []string{"seeded"}}); err != nil {
		t.Fatalf("expected a Create race (AlreadyExists) to be treated as success, got: %v", err)
	}
}

// TestReconcileK8s_AdminUpdatesAllowed: the cfg is unchanged since the
// baseline and the live config moved on through admin updates - the normal
// way to make a change - so startup goes on, and the override is untouched.
func TestReconcileK8s_AdminUpdatesAllowed(t *testing.T) {
	cfg := testConfig{Rules: []string{"original"}}
	clientset := fake.NewSimpleClientset(overrideWithBaseline(t, 3, testConfig{Rules: []string{"admin-updated"}}, &cfg))
	if err := reconcile(clientset, &cfg); err != nil {
		t.Fatalf("reconcileK8s: %v", err)
	}
	stored, _ := storedOverride(t, clientset)
	if stored.Version != 3 || stored.Config.Rules[0] != "admin-updated" {
		t.Errorf("the live config was changed: %+v", stored)
	}
}

// TestReconcileK8s_RefusesChangedCfg is the regression test for a cfg edit
// that used to be silently ignored: the cfg changed since the baseline and
// differs from the live config.
func TestReconcileK8s_RefusesChangedCfg(t *testing.T) {
	baseline := testConfig{Rules: []string{"original"}}
	clientset := fake.NewSimpleClientset(overrideWithBaseline(t, 3, testConfig{Rules: []string{"admin-updated"}}, &baseline))
	err := reconcile(clientset, &testConfig{Rules: []string{"edited-in-cfg"}})
	var drift *DriftError
	if !errors.As(err, &drift) || drift.Section != "widgets" {
		t.Fatalf("reconcileK8s = %v; want a DriftError for widgets", err)
	}
}

// TestReconcileK8s_RefusesCfgThatDropsSection: a section removed from the
// cfg is a change too, unless it was never in it.
func TestReconcileK8s_RefusesCfgThatDropsSection(t *testing.T) {
	baseline := testConfig{Rules: []string{"original"}}
	clientset := fake.NewSimpleClientset(overrideWithBaseline(t, 1, baseline, &baseline))
	var drift *DriftError
	if err := reconcile(clientset, nil); !errors.As(err, &drift) {
		t.Fatalf("reconcileK8s = %v; want a DriftError", err)
	}
}

// TestReconcileK8s_RegeneratedCfgBecomesBaseline: a cfg regenerated from the
// live config starts, and becomes the baseline - so a later admin update
// followed by a restart is allowed rather than refused.
func TestReconcileK8s_RegeneratedCfgBecomesBaseline(t *testing.T) {
	live := testConfig{Rules: []string{"admin-updated"}}
	clientset := fake.NewSimpleClientset(overrideWithBaseline(t, 3, live, &testConfig{Rules: []string{"original"}}))
	if err := reconcile(clientset, &live); err != nil {
		t.Fatalf("reconcileK8s with a regenerated cfg: %v", err)
	}
	_, baseline := storedOverride(t, clientset)
	b, _ := json.Marshal(live)
	c, _ := canonical(b, parseNormalizingTestConfig)
	if baseline != hashOf(c) {
		t.Error("the regenerated cfg didn't become the baseline")
	}

	if _, err := updateK8s(context.Background(), clientset, testNamespace, "widgets", VersionedConfig[testConfig]{Version: 3, Config: testConfig{Rules: []string{"later"}}}); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(clientset, &live); err != nil {
		t.Errorf("a restart after a later admin update was refused: %v", err)
	}
}

// TestReconcileK8s_NormalizesBeforeComparing: values that differ only in
// what the parser fills in are equal.
func TestReconcileK8s_NormalizesBeforeComparing(t *testing.T) {
	clientset := fake.NewSimpleClientset(overrideWithBaseline(t, 2, testConfig{Rules: nil}, &testConfig{Rules: []string{"original"}}))
	if err := reconcile(clientset, &testConfig{Rules: []string{}}); err != nil {
		t.Errorf("a cfg equal to the live config after parsing was refused: %v", err)
	}
}

// TestReconcileK8s_AdoptsBaselineWhenMissing: an override written before
// baselines existed takes the deployed cfg as its baseline.
func TestReconcileK8s_AdoptsBaselineWhenMissing(t *testing.T) {
	cfg := testConfig{Rules: []string{"original"}}
	clientset := fake.NewSimpleClientset(overrideWithBaseline(t, 4, testConfig{Rules: []string{"admin-updated"}}, nil))
	if err := reconcile(clientset, &cfg); err != nil {
		t.Fatalf("reconcileK8s: %v", err)
	}
	if _, baseline := storedOverride(t, clientset); baseline == "" {
		t.Error("no baseline was recorded")
	}
	if err := reconcile(clientset, &testConfig{Rules: []string{"edited-in-cfg"}}); err == nil {
		t.Error("a cfg edit after adopting the baseline wasn't refused")
	}
}

// TestUpdateK8s_KeepsBaseline: an admin update rewrites only the config, so
// the baseline survives it.
func TestUpdateK8s_KeepsBaseline(t *testing.T) {
	cfg := testConfig{Rules: []string{"original"}}
	clientset := fake.NewSimpleClientset(overrideWithBaseline(t, 1, cfg, &cfg))
	_, before := storedOverride(t, clientset)
	if _, err := updateK8s(context.Background(), clientset, testNamespace, "widgets", VersionedConfig[testConfig]{Version: 1, Config: testConfig{Rules: []string{"new"}}}); err != nil {
		t.Fatal(err)
	}
	if _, after := storedOverride(t, clientset); after != before {
		t.Errorf("the baseline changed from %q to %q", before, after)
	}
}

func TestWriter_Reconcile_NoopOutsideK8s(t *testing.T) {
	// No in-cluster config in this test process, so Reconcile falls back
	// to its "outside k8s" no-op even though initial is present.
	w := NewWriter[testConfig]("widgets", nil, &testConfig{Rules: []string{"seeded"}})
	if err := w.Reconcile(context.Background(), parseNormalizingTestConfig); err != nil {
		t.Fatalf("expected Reconcile to no-op outside k8s, got: %v", err)
	}
}

func TestWriter_SeededInitialExportsAsVersion1(t *testing.T) {
	w := NewWriter[testConfig]("widgets", nil, &testConfig{Rules: []string{"seeded"}})

	exported, err := w.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if exported.Version != 1 || len(exported.Config.Rules) != 1 || exported.Config.Rules[0] != "seeded" {
		t.Errorf("expected the seeded initial value to export as version 1, got %+v", exported)
	}

	if _, err := w.Update(context.Background(), VersionedConfig[testConfig]{Version: 1, Config: testConfig{Rules: []string{"updated"}}}); err != nil {
		t.Fatalf("Update against the seeded version: %v", err)
	}
}
