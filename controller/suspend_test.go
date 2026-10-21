package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	synccommon "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/test"
)

func withSuspend(app *v1alpha1.Application, suspended bool, status *v1alpha1.ApplicationSuspendStatus) *v1alpha1.Application {
	if suspended {
		annotations := app.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[v1alpha1.AnnotationKeySuspend] = "true"
		app.SetAnnotations(annotations)
	}
	app.Status.Suspend = status
	return app
}

func suspendTestDeployment(replicas *int64, annotations map[string]string) *unstructured.Unstructured {
	spec := map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"app": "guestbook"}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": "guestbook"}},
			"spec": map[string]any{"containers": []any{map[string]any{
				"name":  "guestbook",
				"image": "quay.io/argoprojlabs/argocd-e2e-container:0.2",
			}}},
		},
	}
	if replicas != nil {
		spec["replicas"] = *replicas
	}
	metadata := map[string]any{"name": "guestbook", "namespace": test.FakeDestNamespace}
	if annotations != nil {
		metadata["annotations"] = toAnyMap(annotations)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   metadata,
		"spec":       spec,
	}}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func manifestOf(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	data, err := json.Marshal(obj)
	require.NoError(t, err)
	return string(data)
}

func compareSuspendedApp(t *testing.T, app *v1alpha1.Application, manifests []string, live []*unstructured.Unstructured, configMapData map[string]string) *comparisonResult {
	t.Helper()
	liveObjs := map[kube.ResourceKey]*unstructured.Unstructured{}
	for _, obj := range live {
		liveObjs[kube.GetResourceKey(obj)] = obj
	}
	ctrl := newFakeController(t.Context(), &fakeData{
		apps: []runtime.Object{app, &defaultProj},
		manifestResponse: &apiclient.ManifestResponse{
			Manifests: manifests,
			Namespace: test.FakeDestNamespace,
			Server:    test.FakeClusterURL,
			Revision:  "abc123",
		},
		managedLiveObjs: liveObjs,
		configMapData:   configMapData,
	}, nil)
	sources := []v1alpha1.ApplicationSource{app.Spec.GetSource()}
	compRes, err := ctrl.appStateManager.CompareAppState(t.Context(), app, &defaultProj, []string{""}, sources, false, false, nil, false)
	require.NoError(t, err)
	return compRes
}

func conditionsOfType(app *v1alpha1.Application, conditionType v1alpha1.ApplicationConditionType) []v1alpha1.ApplicationCondition {
	var result []v1alpha1.ApplicationCondition
	for _, condition := range app.Status.Conditions {
		if condition.Type == conditionType {
			result = append(result, condition)
		}
	}
	return result
}

func TestCompareAppStateSuspend(t *testing.T) {
	three, zero := int64(3), int64(0)

	t.Run("SuspendedFormIsSynced", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, &v1alpha1.ApplicationSuspendStatus{Phase: v1alpha1.ApplicationSuspendPhaseSuspended})
		compRes := compareSuspendedApp(t, app,
			[]string{manifestOf(t, suspendTestDeployment(&three, nil))},
			[]*unstructured.Unstructured{suspendTestDeployment(&zero, nil)}, nil)

		assert.Equal(t, v1alpha1.SyncStatusCodeSynced, compRes.syncStatus.Status)
		assert.Empty(t, compRes.suspendResources)
		assert.Equal(t, health.HealthStatusSuspended, compRes.healthStatus)
		assert.Equal(t, suspendedHealthMessage, compRes.healthMessage)
		assert.Empty(t, app.Status.Conditions)
	})

	t.Run("NotSuspendedIsOutOfSync", func(t *testing.T) {
		app := newFakeApp()
		compRes := compareSuspendedApp(t, app,
			[]string{manifestOf(t, suspendTestDeployment(&three, nil))},
			[]*unstructured.Unstructured{suspendTestDeployment(&zero, nil)}, nil)

		assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, compRes.syncStatus.Status)
		assert.Empty(t, compRes.suspendResources)
		assert.NotEqual(t, health.HealthStatusSuspended, compRes.healthStatus)
	})

	t.Run("RunningWorkloadNeedsSuspendSync", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, nil)
		compRes := compareSuspendedApp(t, app,
			[]string{manifestOf(t, suspendTestDeployment(&three, nil))},
			[]*unstructured.Unstructured{suspendTestDeployment(&three, nil)}, nil)

		assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, compRes.syncStatus.Status)
		assert.Equal(t, []v1alpha1.SyncOperationResource{{Group: "apps", Kind: "Deployment", Name: "guestbook", Namespace: test.FakeDestNamespace}}, compRes.suspendResources)
		assert.NotEqual(t, health.HealthStatusSuspended, compRes.healthStatus)
		replicas, _, err := unstructured.NestedInt64(compRes.managedResources[0].Target.Object, "spec", "replicas")
		require.NoError(t, err)
		assert.Equal(t, int64(0), replicas)
	})

	t.Run("ResumingWorkloadNeedsResumeSync", func(t *testing.T) {
		app := withSuspend(newFakeApp(), false, &v1alpha1.ApplicationSuspendStatus{Phase: v1alpha1.ApplicationSuspendPhaseResuming})
		compRes := compareSuspendedApp(t, app,
			[]string{manifestOf(t, suspendTestDeployment(&three, nil))},
			[]*unstructured.Unstructured{suspendTestDeployment(&zero, nil)}, nil)

		assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, compRes.syncStatus.Status)
		assert.Len(t, compRes.suspendResources, 1)
		replicas, _, err := unstructured.NestedInt64(compRes.managedResources[0].Target.Object, "spec", "replicas")
		require.NoError(t, err)
		assert.Equal(t, int64(3), replicas)
	})

	t.Run("HooksAreNotSuspended", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, nil)
		hook := suspendTestDeployment(&three, map[string]string{synccommon.AnnotationKeyHook: "PreSync"})
		compRes := compareSuspendedApp(t, app, []string{manifestOf(t, hook)}, nil, nil)

		require.Len(t, compRes.reconciliationResult.Hooks, 1)
		replicas, _, err := unstructured.NestedInt64(compRes.reconciliationResult.Hooks[0].Object, "spec", "replicas")
		require.NoError(t, err)
		assert.Equal(t, int64(3), replicas)
		assert.Empty(t, compRes.suspendResources)
	})

	t.Run("DaemonSetIsReported", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, nil)
		daemonSet := `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"agent","namespace":"` + test.FakeDestNamespace + `"}}`
		compareSuspendedApp(t, app, []string{daemonSet}, nil, nil)

		warnings := conditionsOfType(app, v1alpha1.ApplicationConditionSuspendWarning)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0].Message, test.FakeDestNamespace+"/apps/DaemonSet/agent")
	})

	t.Run("FailingScriptIsComparisonError", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, nil)
		compareSuspendedApp(t, app,
			[]string{manifestOf(t, suspendTestDeployment(&three, nil))},
			[]*unstructured.Unstructured{suspendTestDeployment(&three, nil)},
			map[string]string{"resource.customizations.suspend.apps_Deployment": `error("boom")`})

		errs := conditionsOfType(app, v1alpha1.ApplicationConditionComparisonError)
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0].Message, "Failed to suspend "+test.FakeDestNamespace+"/apps/Deployment/guestbook")
	})

	t.Run("IgnoredSuspendedFieldIsReported", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, &v1alpha1.ApplicationSuspendStatus{Phase: v1alpha1.ApplicationSuspendPhaseSuspended})
		app.Spec.IgnoreDifferences = []v1alpha1.ResourceIgnoreDifferences{{Group: "apps", Kind: "Deployment", JSONPointers: []string{"/spec/replicas"}}}
		compRes := compareSuspendedApp(t, app,
			[]string{manifestOf(t, suspendTestDeployment(&three, nil))},
			[]*unstructured.Unstructured{suspendTestDeployment(&three, nil)}, nil)

		assert.Equal(t, v1alpha1.SyncStatusCodeSynced, compRes.syncStatus.Status)
		warnings := conditionsOfType(app, v1alpha1.ApplicationConditionSuspendWarning)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0].Message, "ignored in the diff")
		assert.Contains(t, warnings[0].Message, test.FakeDestNamespace+"/apps/Deployment/guestbook")
	})

	t.Run("WarningIsClearedAfterResume", func(t *testing.T) {
		app := withSuspend(newFakeApp(), false, nil)
		now := metav1.Now()
		app.Status.Conditions = []v1alpha1.ApplicationCondition{{Type: v1alpha1.ApplicationConditionSuspendWarning, Message: "stale", LastTransitionTime: &now}}
		compareSuspendedApp(t, app, []string{manifestOf(t, suspendTestDeployment(&three, nil))}, nil, nil)

		assert.Empty(t, conditionsOfType(app, v1alpha1.ApplicationConditionSuspendWarning))
	})
}

func TestSuspendTargetObjectsSkipsOwnManifest(t *testing.T) {
	app := withSuspend(newFakeApp(), true, nil)
	own := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": app.Name, "namespace": app.Namespace},
	}}
	child := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "child", "namespace": app.Namespace},
	}}
	targets := []*unstructured.Unstructured{own, child}

	state := suspendTargetObjects(app, targets, nil, metav1.Now())

	assert.Empty(t, targets[0].GetAnnotations())
	assert.Equal(t, "true", targets[1].GetAnnotations()[v1alpha1.AnnotationKeySuspend])
	assert.Len(t, state.scripts, 1)
}

func TestSuspendStateDiffers(t *testing.T) {
	phase := func(p v1alpha1.ApplicationSuspendPhase) *v1alpha1.ApplicationSuspendStatus {
		return &v1alpha1.ApplicationSuspendStatus{Phase: p}
	}
	tests := []struct {
		name      string
		suspended bool
		status    *v1alpha1.ApplicationSuspendStatus
		want      bool
	}{
		{name: "neither", want: false},
		{name: "suspend requested", suspended: true, want: true},
		{name: "suspending", suspended: true, status: phase(v1alpha1.ApplicationSuspendPhaseSuspending), want: false},
		{name: "suspended", suspended: true, status: phase(v1alpha1.ApplicationSuspendPhaseSuspended), want: false},
		{name: "suspend requested while resuming", suspended: true, status: phase(v1alpha1.ApplicationSuspendPhaseResuming), want: true},
		{name: "resume requested", status: phase(v1alpha1.ApplicationSuspendPhaseSuspended), want: true},
		{name: "resume requested while suspending", status: phase(v1alpha1.ApplicationSuspendPhaseSuspending), want: true},
		{name: "resuming", status: phase(v1alpha1.ApplicationSuspendPhaseResuming), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := withSuspend(newFakeApp(), tt.suspended, tt.status)
			assert.Equal(t, tt.want, suspendStateDiffers(app))
		})
	}
}

func TestNeedRefreshAppStatusSuspendStateDiffers(t *testing.T) {
	app := newFakeApp()
	now := metav1.Now()
	app.Status.ReconciledAt = &now
	app.Status.Sync = v1alpha1.SyncStatus{
		Status: v1alpha1.SyncStatusCodeSynced,
		ComparedTo: v1alpha1.ComparedTo{
			Source:            app.Spec.GetSource(),
			Destination:       app.Spec.Destination,
			IgnoreDifferences: app.Spec.IgnoreDifferences,
		},
	}
	ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{}}, nil)

	needRefresh, _, _ := ctrl.needRefreshAppStatus(app, time.Hour, 2*time.Hour)
	assert.False(t, needRefresh)

	needRefresh, _, compareWith := ctrl.needRefreshAppStatus(withSuspend(app, true, nil), time.Hour, 2*time.Hour)
	assert.True(t, needRefresh)
	assert.Equal(t, CompareWithLatest, compareWith)
}

func TestSuspendSync(t *testing.T) {
	deployment := v1alpha1.SyncOperationResource{Group: "apps", Kind: "Deployment", Name: "guestbook", Namespace: test.FakeDestNamespace}
	outOfSync := func(resources ...v1alpha1.SyncOperationResource) *comparisonResult {
		return &comparisonResult{
			syncStatus:       &v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeOutOfSync, Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			suspendResources: resources,
		}
	}
	synced := &comparisonResult{syncStatus: &v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeSynced}}
	phase := func(p v1alpha1.ApplicationSuspendPhase) *v1alpha1.ApplicationSuspendStatus {
		return &v1alpha1.ApplicationSuspendStatus{Phase: p}
	}
	storedOperation := func(t *testing.T, ctrl *ApplicationController, app *v1alpha1.Application) *v1alpha1.Operation {
		t.Helper()
		stored, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
		require.NoError(t, err)
		return stored.Operation
	}

	t.Run("StartsSuspend", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, nil)
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		cond, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.Nil(t, cond)
		assert.True(t, initiated)
		assert.Equal(t, v1alpha1.ApplicationSuspendPhaseSuspending, app.Status.Suspend.Phase)
		op := storedOperation(t, ctrl, app)
		require.NotNil(t, op)
		assert.True(t, op.InitiatedBy.Automated)
		assert.True(t, isSuspendOperation(op))
		assert.Equal(t, suspendOperationReason, suspendReasonOf(op))
		assert.False(t, op.Sync.Prune)
		assert.Equal(t, []v1alpha1.SyncOperationResource{deployment}, op.Sync.Resources)
		assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", op.Sync.Revision)
	})

	t.Run("StartsSuspendWithoutSyncPolicy", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, nil)
		app.Spec.SyncPolicy = nil
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		_, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.True(t, initiated)
		assert.NotNil(t, storedOperation(t, ctrl, app))
	})

	t.Run("CompletesSuspend", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, phase(v1alpha1.ApplicationSuspendPhaseSuspending))
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		_, _, initiated := ctrl.suspendSync(t.Context(), app, synced, true)

		assert.False(t, initiated)
		assert.Equal(t, v1alpha1.ApplicationSuspendPhaseSuspended, app.Status.Suspend.Phase)
	})

	t.Run("StartsResume", func(t *testing.T) {
		app := withSuspend(newFakeApp(), false, phase(v1alpha1.ApplicationSuspendPhaseSuspended))
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		_, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.True(t, initiated)
		assert.Equal(t, v1alpha1.ApplicationSuspendPhaseResuming, app.Status.Suspend.Phase)
		assert.Equal(t, resumeOperationReason, suspendReasonOf(storedOperation(t, ctrl, app)))
	})

	t.Run("CompletesResume", func(t *testing.T) {
		app := withSuspend(newFakeApp(), false, phase(v1alpha1.ApplicationSuspendPhaseResuming))
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		ctrl.suspendSync(t.Context(), app, synced, true)

		assert.Nil(t, app.Status.Suspend)
	})

	t.Run("ResumeRequestedWhileSuspending", func(t *testing.T) {
		app := withSuspend(newFakeApp(), false, phase(v1alpha1.ApplicationSuspendPhaseSuspending))
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.Equal(t, v1alpha1.ApplicationSuspendPhaseResuming, app.Status.Suspend.Phase)
	})

	t.Run("DoesNothingWhenNotSuspended", func(t *testing.T) {
		app := newFakeApp()
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		cond, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.Nil(t, cond)
		assert.False(t, initiated)
		assert.Nil(t, app.Status.Suspend)
		assert.Nil(t, storedOperation(t, ctrl, app))
	})

	t.Run("SyncWindowPostponesSync", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, nil)
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		_, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), false)

		assert.False(t, initiated)
		assert.Equal(t, v1alpha1.ApplicationSuspendPhaseSuspending, app.Status.Suspend.Phase)
		assert.Nil(t, storedOperation(t, ctrl, app))
	})

	t.Run("WaitsForRunningOperation", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, phase(v1alpha1.ApplicationSuspendPhaseSuspending))
		app.Operation = &v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{}}
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		_, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.False(t, initiated)
		assert.False(t, isSuspendOperation(storedOperation(t, ctrl, app)))
	})

	t.Run("UnknownComparisonDoesNotComplete", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, phase(v1alpha1.ApplicationSuspendPhaseSuspending))
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		ctrl.suspendSync(t.Context(), app, &comparisonResult{syncStatus: &v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeUnknown}}, true)

		assert.Equal(t, v1alpha1.ApplicationSuspendPhaseSuspending, app.Status.Suspend.Phase)
	})

	t.Run("ComparisonErrorDoesNotComplete", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, phase(v1alpha1.ApplicationSuspendPhaseSuspending))
		app.Status.Conditions = []v1alpha1.ApplicationCondition{{Type: v1alpha1.ApplicationConditionComparisonError, Message: "boom"}}
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)

		ctrl.suspendSync(t.Context(), app, synced, true)

		assert.Equal(t, v1alpha1.ApplicationSuspendPhaseSuspending, app.Status.Suspend.Phase)
	})

	t.Run("FailedAttemptBacksOff", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, phase(v1alpha1.ApplicationSuspendPhaseSuspending))
		finishedAt := metav1.Now()
		app.Status.OperationState = &v1alpha1.OperationState{
			Operation: v1alpha1.Operation{
				Sync: &v1alpha1.SyncOperation{SelfHealAttemptsCount: 1},
				Info: []*v1alpha1.Info{{Name: suspendOperationInfoName, Value: suspendOperationReason}},
			},
			Phase:      synccommon.OperationFailed,
			Message:    "quota exceeded",
			FinishedAt: &finishedAt,
		}
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)
		ctrl.selfHealTimeout = time.Minute

		cond, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.False(t, initiated)
		require.NotNil(t, cond)
		assert.Equal(t, v1alpha1.ApplicationConditionSyncError, cond.Type)
		assert.Contains(t, cond.Message, "Failed last suspend attempt: quota exceeded")
		assert.Nil(t, storedOperation(t, ctrl, app))
	})

	t.Run("FailedAttemptIsRetried", func(t *testing.T) {
		app := withSuspend(newFakeApp(), true, phase(v1alpha1.ApplicationSuspendPhaseSuspending))
		finishedAt := metav1.NewTime(time.Now().Add(-time.Hour))
		app.Status.OperationState = &v1alpha1.OperationState{
			Operation: v1alpha1.Operation{
				Sync: &v1alpha1.SyncOperation{SelfHealAttemptsCount: 1},
				Info: []*v1alpha1.Info{{Name: suspendOperationInfoName, Value: suspendOperationReason}},
			},
			Phase:      synccommon.OperationFailed,
			FinishedAt: &finishedAt,
		}
		ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app}}, nil)
		ctrl.selfHealTimeout = time.Minute

		_, _, initiated := ctrl.suspendSync(t.Context(), app, outOfSync(deployment), true)

		assert.True(t, initiated)
		assert.Equal(t, int64(2), storedOperation(t, ctrl, app).Sync.SelfHealAttemptsCount)
	})
}

func TestProcessAppRefreshStartsSuspend(t *testing.T) {
	three := int64(3)
	app := withSuspend(newFakeApp(), true, nil)
	ctrl := newFakeController(t.Context(), &fakeData{
		apps: []runtime.Object{app, &defaultProj},
		manifestResponse: &apiclient.ManifestResponse{
			Manifests: []string{manifestOf(t, suspendTestDeployment(&three, nil))},
			Namespace: test.FakeDestNamespace,
			Server:    test.FakeClusterURL,
			Revision:  "abc123",
		},
		managedLiveObjs: map[kube.ResourceKey]*unstructured.Unstructured{
			kube.GetResourceKey(suspendTestDeployment(&three, nil)): suspendTestDeployment(&three, nil),
		},
	}, nil)
	key, err := cache.MetaNamespaceKeyFunc(app)
	require.NoError(t, err)
	ctrl.appRefreshQueue.AddRateLimited(key)
	ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatest.Pointer(), nil)

	ctrl.processAppRefreshQueueItem()

	stored, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, stored.Operation)
	assert.Equal(t, suspendOperationReason, suspendReasonOf(stored.Operation))
	assert.Equal(t, []v1alpha1.SyncOperationResource{{Group: "apps", Kind: "Deployment", Name: "guestbook", Namespace: test.FakeDestNamespace}}, stored.Operation.Sync.Resources)
	require.NotNil(t, stored.Status.Suspend)
	assert.Equal(t, v1alpha1.ApplicationSuspendPhaseSuspending, stored.Status.Suspend.Phase)
}

func TestSyncHealthOverride(t *testing.T) {
	pausedDeployment := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "guestbook", "namespace": test.FakeDestNamespace, "generation": int64(1)},
		"spec":       map[string]any{"paused": true},
		"status":     map[string]any{"observedGeneration": int64(1)},
	}}
	suspendedConfigMap := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "config", "namespace": test.FakeDestNamespace},
	}}
	overrides := map[string]v1alpha1.ResourceOverride{
		"ConfigMap": {HealthLua: `return {status = "Suspended", message = "on purpose"}`},
	}

	t.Run("SuspendedApplication", func(t *testing.T) {
		override := syncHealthOverride(withSuspend(newFakeApp(), true, nil), overrides)

		status, err := health.GetResourceHealth(pausedDeployment, override)
		require.NoError(t, err)
		assert.Equal(t, health.HealthStatusHealthy, status.Status)

		status, err = health.GetResourceHealth(suspendedConfigMap, override)
		require.NoError(t, err)
		assert.Equal(t, health.HealthStatusHealthy, status.Status)
		assert.Equal(t, "on purpose", status.Message)
	})

	t.Run("NotSuspendedApplication", func(t *testing.T) {
		override := syncHealthOverride(newFakeApp(), overrides)

		status, err := health.GetResourceHealth(pausedDeployment, override)
		require.NoError(t, err)
		assert.Equal(t, health.HealthStatusSuspended, status.Status)

		status, err = health.GetResourceHealth(suspendedConfigMap, override)
		require.NoError(t, err)
		assert.Equal(t, health.HealthStatusSuspended, status.Status)
	})
}

func TestSyncSkipHooks(t *testing.T) {
	tests := []struct {
		name      string
		suspended bool
		syncOp    v1alpha1.SyncOperation
		want      bool
	}{
		{name: "full sync", want: false},
		{name: "selective sync", syncOp: v1alpha1.SyncOperation{Resources: []v1alpha1.SyncOperationResource{{Kind: "Deployment", Name: "guestbook"}}}, want: true},
		{name: "apply strategy", syncOp: v1alpha1.SyncOperation{SyncStrategy: &v1alpha1.SyncStrategy{Apply: &v1alpha1.SyncStrategyApply{}}}, want: true},
		{name: "suspended application", suspended: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := withSuspend(newFakeApp(), tt.suspended, nil)
			assert.Equal(t, tt.want, syncSkipHooks(app, &tt.syncOp))
		})
	}
}
