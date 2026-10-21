package controller

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	hookutil "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/hook"
	kubeutil "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/lua"
)

const (
	suspendedHealthMessage = "Application is suspended"
	// maxSuspendWarningResources bounds the number of resources listed in a SuspendWarning condition
	maxSuspendWarningResources = 10
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
