package lua

import (
	"errors"
	"fmt"

	lua "github.com/yuin/gopher-lua"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	luajson "layeh.com/gopher-json"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const suspendScriptFile = "suspend.lua"

// GetSuspendScript returns the script that puts objects of obj's kind into their suspended form. A suspend script in
// the resource overrides takes precedence over the built-in one. Unlike health checks, suspend scripts are looked up by
// exact group and kind only. It returns an empty string if the kind has no suspend script.
func (vm VM) GetSuspendScript(obj *unstructured.Unstructured) (script string, useOpenLibs bool, err error) {
	key := GetConfigMapKey(obj.GroupVersionKind())
	if override, ok := vm.ResourceOverrides[key]; ok && override.SuspendLua != "" {
		return override.SuspendLua, override.UseOpenLibs, nil
	}
	builtInScript, err := vm.getPredefinedLuaScripts(key, suspendScriptFile)
	if errors.Is(err, errScriptDoesNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// standard libraries are enabled for all built-in scripts
	return builtInScript, true, nil
}

// ExecuteSuspendScript runs a suspend script against obj and returns the object in its suspended form. obj itself is not
// modified.
func (vm VM) ExecuteSuspendScript(obj *unstructured.Unstructured, script string) (*unstructured.Unstructured, error) {
	l, err := vm.runLua(obj, script)
	if err != nil {
		return nil, err
	}
	returnValue := l.Get(-1)
	if returnValue.Type() != lua.LTTable {
		return nil, fmt.Errorf(incorrectReturnType, "table", returnValue.Type().String())
	}
	jsonBytes, err := luajson.Encode(returnValue)
	if err != nil {
		return nil, err
	}
	newObj, err := appv1.UnmarshalToUnstructured(string(jsonBytes))
	if err != nil {
		return nil, err
	}
	newObj.Object = cleanReturnedObj(newObj.Object, obj.Object)
	if newObj.GroupVersionKind().GroupKind() != obj.GroupVersionKind().GroupKind() ||
		newObj.GetName() != obj.GetName() ||
		newObj.GetNamespace() != obj.GetNamespace() {
		return nil, fmt.Errorf("suspend script changed the group, kind, name or namespace of %s/%s", obj.GetKind(), obj.GetName())
	}
	return newObj, nil
}
