package lua

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

type suspendTestStructure struct {
	Tests []suspendTest `yaml:"tests"`
}

type suspendTest struct {
	InputPath          string `yaml:"inputPath"`
	ExpectedOutputPath string `yaml:"expectedOutputPath"`
}

// TestLuaSuspendScript runs the cases in the suspend_test.yaml next to every built-in suspend.lua. Each script must
// produce the expected output and leave its own output unchanged.
func TestLuaSuspendScript(t *testing.T) {
	err := filepath.Walk("../../resource_customizations", func(path string, _ os.FileInfo, err error) error {
		require.NoError(t, err)
		if filepath.Base(path) != suspendScriptFile {
			return nil
		}
		dir := filepath.Dir(path)
		yamlBytes, err := os.ReadFile(filepath.Join(dir, "suspend_test.yaml"))
		require.NoError(t, err)
		var resourceTest suspendTestStructure
		require.NoError(t, yaml.Unmarshal(yamlBytes, &resourceTest))
		require.NotEmpty(t, resourceTest.Tests, "%s has no suspend tests", dir)
		for _, test := range resourceTest.Tests {
			t.Run(filepath.Join(strings.TrimPrefix(dir, "../../resource_customizations/"), test.InputPath), func(t *testing.T) {
				vm := VM{UseOpenLibs: true}
				obj := getObj(t, filepath.Join(dir, test.InputPath))
				script, _, err := vm.GetSuspendScript(obj)
				require.NoError(t, err)
				require.NotEmpty(t, script)

				result, err := vm.ExecuteSuspendScript(obj, script)
				require.NoError(t, err)
				assertSameObject(t, getObj(t, filepath.Join(dir, test.ExpectedOutputPath)), result)

				again, err := vm.ExecuteSuspendScript(result, script)
				require.NoError(t, err)
				assertSameObject(t, result, again)
			})
		}
		return nil
	})
	require.NoError(t, err)
}

func assertSameObject(t *testing.T, expected, actual *unstructured.Unstructured) {
	t.Helper()
	expectedJSON, err := json.Marshal(expected)
	require.NoError(t, err)
	actualJSON, err := json.Marshal(actual)
	require.NoError(t, err)
	assert.JSONEq(t, string(expectedJSON), string(actualJSON))
}

const suspendTestDeployment = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: guestbook
  namespace: default
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: guestbook
        resources: {}
`

func TestGetSuspendScript(t *testing.T) {
	deployment := StrToUnstructured(suspendTestDeployment)

	t.Run("BuiltIn", func(t *testing.T) {
		script, useOpenLibs, err := VM{}.GetSuspendScript(deployment)
		require.NoError(t, err)
		assert.Contains(t, script, "replicas = 0")
		assert.True(t, useOpenLibs)
	})

	t.Run("OverrideTakesPrecedence", func(t *testing.T) {
		vm := VM{ResourceOverrides: map[string]appv1.ResourceOverride{
			"apps/Deployment": {SuspendLua: "return obj", UseOpenLibs: false},
		}}
		script, useOpenLibs, err := vm.GetSuspendScript(deployment)
		require.NoError(t, err)
		assert.Equal(t, "return obj", script)
		assert.False(t, useOpenLibs)
	})

	t.Run("WildcardOverrideIsIgnored", func(t *testing.T) {
		vm := VM{ResourceOverrides: map[string]appv1.ResourceOverride{
			"*/*": {SuspendLua: "return obj"},
		}}
		script, _, err := vm.GetSuspendScript(deployment)
		require.NoError(t, err)
		assert.Contains(t, script, "replicas = 0")
	})

	t.Run("NoScript", func(t *testing.T) {
		configMap := StrToUnstructured(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
`)
		script, _, err := VM{}.GetSuspendScript(configMap)
		require.NoError(t, err)
		assert.Empty(t, script)
	})
}

func TestExecuteSuspendScript(t *testing.T) {
	t.Run("ReturnsSuspendedCopy", func(t *testing.T) {
		deployment := StrToUnstructured(suspendTestDeployment)
		result, err := VM{}.ExecuteSuspendScript(deployment, "obj.spec.replicas = 0\nreturn obj")
		require.NoError(t, err)
		assert.EqualValues(t, 0, result.Object["spec"].(map[string]any)["replicas"])
		assert.EqualValues(t, 2, deployment.Object["spec"].(map[string]any)["replicas"])

		containers := result.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
		assert.Equal(t, map[string]any{}, containers[0].(map[string]any)["resources"])
	})

	t.Run("NonTableResult", func(t *testing.T) {
		_, err := VM{}.ExecuteSuspendScript(StrToUnstructured(suspendTestDeployment), "return 1")
		require.ErrorContains(t, err, "expect table output")
	})

	t.Run("ChangedIdentity", func(t *testing.T) {
		_, err := VM{}.ExecuteSuspendScript(StrToUnstructured(suspendTestDeployment), `obj.metadata.name = "other"
return obj`)
		require.ErrorContains(t, err, "changed the group, kind, name or namespace")
	})
}
