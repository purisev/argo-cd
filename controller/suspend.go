package controller

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	hookutil "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/hook"
	kubeutil "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	applog "github.com/argoproj/argo-cd/v3/util/app/log"
	"github.com/argoproj/argo-cd/v3/util/argo"
	"github.com/argoproj/argo-cd/v3/util/lua"
)

const (
	suspendedHealthMessage = "Application is suspended"
	// maxSuspendWarningResources bounds the number of resources listed in a SuspendWarning condition
	maxSuspendWarningResources = 10
	// suspendOperationInfoName names the operation info entry that marks the syncs started by suspendSync
	suspendOperationInfoName = "Reason"
	suspendOperationReason   = "Suspend"
	resumeOperationReason    = "Resume"
)

// suspendScript is the suspend script of one target object
type suspendScript struct {
	script      string
	useOpenLibs bool
}

// suspendState holds what CompareAppState knows about suspending an application's target objects
type suspendState struct {
	// scripts maps the keys of target objects whose kind has a suspend script to that script
	scripts map[kubeutil.ResourceKey]suspendScript
	// unsuspendable lists target objects that keep running while the application is suspended
	unsuspendable []string
	conditions    []v1alpha1.ApplicationCondition
}

// isSuspendRelevant returns whether suspend scripts matter for the application: it is suspended, or it is still
// transitioning out of being suspended.
func isSuspendRelevant(app *v1alpha1.Application) bool {
	return app.IsSuspended() || app.Status.Suspend != nil
}

// suspendStateDiffers returns whether the suspend annotation of the application and its suspend status disagree,
// i.e. a suspend or resume was requested but has not started yet.
func suspendStateDiffers(app *v1alpha1.Application) bool {
	if app.IsSuspended() {
		return app.Status.Suspend == nil || app.Status.Suspend.Phase == v1alpha1.ApplicationSuspendPhaseResuming
	}
	return app.Status.Suspend != nil && app.Status.Suspend.Phase != v1alpha1.ApplicationSuspendPhaseResuming
}

// suspendTargetObjects looks up the suspend script of every target object of the application. If the application is
// suspended, it replaces each target object that has a script with its suspended form. Hooks and the application's
// own manifest are left alone.
func suspendTargetObjects(app *v1alpha1.Application, targetObjs []*unstructured.Unstructured, resourceOverrides map[string]v1alpha1.ResourceOverride, now metav1.Time) suspendState {
	state := suspendState{scripts: map[kubeutil.ResourceKey]suspendScript{}}
	if !isSuspendRelevant(app) {
		return state
	}
	vm := lua.VM{ResourceOverrides: resourceOverrides}
	for i, obj := range targetObjs {
		if obj == nil || hookutil.IsHook(obj) || isApplicationManifest(app, obj) {
			continue
		}
		script, useOpenLibs, err := vm.GetSuspendScript(obj)
		if err != nil {
			state.conditions = append(state.conditions, v1alpha1.ApplicationCondition{
				Type:               v1alpha1.ApplicationConditionComparisonError,
				Message:            fmt.Sprintf("Failed to get suspend script of %s: %v", resourceID(obj), err),
				LastTransitionTime: &now,
			})
			continue
		}
		if script == "" {
			if isUnsuspendableKind(obj) {
				state.unsuspendable = append(state.unsuspendable, resourceID(obj))
			}
			continue
		}
		state.scripts[kubeutil.GetResourceKey(obj)] = suspendScript{script: script, useOpenLibs: useOpenLibs}
		if !app.IsSuspended() {
			continue
		}
		suspended, err := executeSuspendScript(vm, obj, suspendScript{script: script, useOpenLibs: useOpenLibs})
		if err != nil {
			state.conditions = append(state.conditions, v1alpha1.ApplicationCondition{
				Type:               v1alpha1.ApplicationConditionComparisonError,
				Message:            fmt.Sprintf("Failed to suspend %s: %v", resourceID(obj), err),
				LastTransitionTime: &now,
			})
			continue
		}
		targetObjs[i] = suspended
	}
	if app.IsSuspended() && len(state.unsuspendable) > 0 {
		state.conditions = append(state.conditions, suspendWarning("Resources without a suspend script keep running while the application is suspended", state.unsuspendable, now))
	}
	return state
}

// isInSuspendedForm returns whether the live object is already in the form its suspend script produces.
func (s suspendState) isInSuspendedForm(resourceOverrides map[string]v1alpha1.ResourceOverride, live *unstructured.Unstructured) (bool, error) {
	script, ok := s.scripts[kubeutil.GetResourceKey(live)]
	if !ok {
		return true, nil
	}
	suspended, err := executeSuspendScript(lua.VM{ResourceOverrides: resourceOverrides}, live, script)
	if err != nil {
		return false, err
	}
	return sameJSON(live.Object, suspended.Object)
}

func executeSuspendScript(vm lua.VM, obj *unstructured.Unstructured, script suspendScript) (*unstructured.Unstructured, error) {
	vm.UseOpenLibs = script.useOpenLibs
	return vm.ExecuteSuspendScript(obj, script.script)
}

func sameJSON(a, b map[string]any) (bool, error) {
	normalize := func(obj map[string]any) (any, error) {
		data, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		var normalized any
		err = json.Unmarshal(data, &normalized)
		return normalized, err
	}
	normalizedA, err := normalize(a)
	if err != nil {
		return false, err
	}
	normalizedB, err := normalize(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(normalizedA, normalizedB), nil
}

func suspendWarning(message string, resources []string, now metav1.Time) v1alpha1.ApplicationCondition {
	listed := resources
	if len(listed) > maxSuspendWarningResources {
		listed = listed[:maxSuspendWarningResources]
	}
	text := fmt.Sprintf("%s: %s", message, strings.Join(listed, ", "))
	if remaining := len(resources) - len(listed); remaining > 0 {
		text += fmt.Sprintf(" and %d more", remaining)
	}
	return v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionSuspendWarning, Message: text, LastTransitionTime: &now}
}

// isUnsuspendableKind returns whether objects of obj's kind run Pods but cannot be scaled to zero.
func isUnsuspendableKind(obj *unstructured.Unstructured) bool {
	gk := obj.GroupVersionKind().GroupKind()
	return (gk.Group == "apps" && gk.Kind == kubeutil.DaemonSetKind) || (gk.Group == "" && gk.Kind == kubeutil.PodKind)
}

func isApplicationManifest(app *v1alpha1.Application, obj *unstructured.Unstructured) bool {
	gk := obj.GroupVersionKind().GroupKind()
	return gk.Group == application.Group && gk.Kind == application.ApplicationKind &&
		obj.GetName() == app.Name && obj.GetNamespace() == app.Namespace
}

func resourceID(obj *unstructured.Unstructured) string {
	gk := obj.GroupVersionKind().GroupKind()
	id := gk.Kind + "/" + obj.GetName()
	if gk.Group != "" {
		id = gk.Group + "/" + id
	}
	if obj.GetNamespace() != "" {
		id = obj.GetNamespace() + "/" + id
	}
	return id
}

// suspendSync moves the application through the suspend phases and starts the syncs that suspend or resume its
// workloads. Phases advance regardless of sync windows, while syncs only start when canSync is true. It returns a
// SyncError condition when the last suspend or resume sync failed, and whether it started an operation.
func (ctrl *ApplicationController) suspendSync(ctx context.Context, app *v1alpha1.Application, compareResult *comparisonResult, canSync bool) (*v1alpha1.ApplicationCondition, time.Duration, bool) {
	if !isSuspendRelevant(app) || (app.DeletionTimestamp != nil && !app.DeletionTimestamp.IsZero()) {
		return nil, 0, false
	}
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	now := metav1.Now()
	switch {
	case app.IsSuspended() && (app.Status.Suspend == nil || app.Status.Suspend.Phase == v1alpha1.ApplicationSuspendPhaseResuming):
		app.Status.Suspend = &v1alpha1.ApplicationSuspendStatus{Phase: v1alpha1.ApplicationSuspendPhaseSuspending, StartedAt: now}
	case !app.IsSuspended() && app.Status.Suspend.Phase != v1alpha1.ApplicationSuspendPhaseResuming:
		app.Status.Suspend = &v1alpha1.ApplicationSuspendStatus{Phase: v1alpha1.ApplicationSuspendPhaseResuming, StartedAt: now}
	}
	phase := app.Status.Suspend.Phase
	if phase == v1alpha1.ApplicationSuspendPhaseSuspended {
		return nil, 0, false
	}
	// without a reliable comparison an empty list of resources to sync does not mean the transition is complete
	if compareResult.syncStatus.Status == v1alpha1.SyncStatusCodeUnknown ||
		len(app.Status.GetConditions(map[v1alpha1.ApplicationConditionType]bool{v1alpha1.ApplicationConditionComparisonError: true})) > 0 {
		return nil, 0, false
	}
	if len(compareResult.suspendResources) == 0 {
		if phase == v1alpha1.ApplicationSuspendPhaseSuspending {
			app.Status.Suspend = &v1alpha1.ApplicationSuspendStatus{Phase: v1alpha1.ApplicationSuspendPhaseSuspended, StartedAt: now}
		} else {
			app.Status.Suspend = nil
		}
		return nil, 0, false
	}
	if !canSync {
		logCtx.Infof("%s prevented by sync window", strings.ToLower(string(phase)))
		return nil, 0, false
	}
	if app.Operation != nil {
		return nil, 0, false
	}

	reason := suspendOperationReason
	if phase == v1alpha1.ApplicationSuspendPhaseResuming {
		reason = resumeOperationReason
	}
	op := v1alpha1.Operation{
		Sync: &v1alpha1.SyncOperation{
			Revision:  compareResult.syncStatus.Revision,
			Revisions: compareResult.syncStatus.Revisions,
			Resources: compareResult.suspendResources,
		},
		InitiatedBy: v1alpha1.OperationInitiator{Automated: true},
		Info:        []*v1alpha1.Info{{Name: suspendOperationInfoName, Value: reason}},
		Retry:       v1alpha1.RetryStrategy{Limit: 5},
	}
	if app.Spec.HasMultipleSources() {
		op.Sync.Sources = app.Spec.Sources
	} else {
		op.Sync.Source = new(app.Spec.GetSource())
	}
	if app.Spec.SyncPolicy != nil {
		op.Sync.SyncOptions = app.Spec.SyncPolicy.SyncOptions
		if app.Spec.SyncPolicy.Retry != nil {
			op.Retry = *app.Spec.SyncPolicy.Retry
		}
	}

	// Repeated attempts, for example after a failure, back off like self-heal does.
	var syncErrCond *v1alpha1.ApplicationCondition
	if last := app.Status.OperationState; last != nil && isSuspendOperation(&last.Operation) && last.Operation.Sync != nil {
		if last.Phase.Completed() && !last.Phase.Successful() {
			syncErrCond = &v1alpha1.ApplicationCondition{
				Type:    v1alpha1.ApplicationConditionSyncError,
				Message: fmt.Sprintf("Failed last %s attempt: %s", strings.ToLower(suspendReasonOf(&last.Operation)), last.Message),
			}
		}
		op.Sync.SelfHealAttemptsCount = last.Operation.Sync.SelfHealAttemptsCount
		if remainingTime := ctrl.selfHealRemainingBackoff(app, int(op.Sync.SelfHealAttemptsCount)); remainingTime > 0 {
			logCtx.Infof("Skipping %s: retrying in %v", strings.ToLower(reason), remainingTime)
			ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatest.Pointer(), &remainingTime)
			return syncErrCond, 0, false
		}
		op.Sync.SelfHealAttemptsCount++
	}

	appIf := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace)
	start := time.Now()
	updatedApp, err := argo.SetAppOperation(appIf, app.Name, &op)
	setOpTime := time.Since(start)
	if err != nil {
		if stderrors.Is(err, argo.ErrAnotherOperationInProgress) {
			logCtx.WithError(err).Warnf("Failed to initiate %s", strings.ToLower(reason))
			return syncErrCond, 0, false
		}
		logCtx.WithError(err).Errorf("Failed to initiate %s", strings.ToLower(reason))
		return &v1alpha1.ApplicationCondition{Type: v1alpha1.ApplicationConditionSyncError, Message: err.Error()}, setOpTime, false
	}
	ctrl.writeBackToInformer(updatedApp)
	message := fmt.Sprintf("Initiated %s of %d resources", strings.ToLower(reason), len(op.Sync.Resources))
	ctrl.logAppEvent(ctx, app, argo.EventInfo{Reason: argo.EventReasonOperationStarted, Type: corev1.EventTypeNormal}, message)
	logCtx.Info(message)
	return syncErrCond, setOpTime, true
}

// isSuspendOperation returns whether the operation is a sync started by suspendSync.
func isSuspendOperation(op *v1alpha1.Operation) bool {
	return suspendReasonOf(op) != ""
}

func suspendReasonOf(op *v1alpha1.Operation) string {
	for _, info := range op.Info {
		if info != nil && info.Name == suspendOperationInfoName && (info.Value == suspendOperationReason || info.Value == resumeOperationReason) {
			return info.Value
		}
	}
	return ""
}
