package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/health"
	. "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	. "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/test/e2e/fixture"
	. "github.com/argoproj/argo-cd/v3/test/e2e/fixture/app"
)

const suspendWaitTimeout = 90 * time.Second

func toggleSuspend(t *testing.T, ctx *Context) {
	t.Helper()
	runToggleSuspend(t, ctx.AppNamespace(), ctx.AppName())
}

// runToggleSuspend runs the toggle-suspend action of an Application. The resource name and namespace select the
// Application itself rather than a child Application it manages.
func runToggleSuspend(t *testing.T, namespace, name string) {
	t.Helper()
	_, err := fixture.RunCli("app", "actions", "run", namespace+"/"+name, "toggle-suspend",
		"--kind", "Application", "--group", "argoproj.io", "--resource-name", name, "--namespace", namespace)
	require.NoError(t, err)
}

func suspendPhaseIs(phase ApplicationSuspendPhase) Expectation {
	return App(func(app *Application) bool {
		return app.Status.Suspend != nil && app.Status.Suspend.Phase == phase
	})
}

func notSuspended() Expectation {
	return App(func(app *Application) bool {
		return !app.IsSuspended() && app.Status.Suspend == nil
	})
}

// waitForDeploymentReplicas waits until the Deployment declares the given number of replicas.
func waitForDeploymentReplicas(t *testing.T, ctx *Context, name string, replicas int32) {
	t.Helper()
	var last *int32
	err := wait.PollUntilContextTimeout(t.Context(), time.Second, suspendWaitTimeout, true, func(context.Context) (bool, error) {
		deployment, err := fixture.KubeClientset.AppsV1().Deployments(ctx.DeploymentNamespace()).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		last = deployment.Spec.Replicas
		return last != nil && *last == replicas, nil
	})
	require.NoError(t, err, "Deployment %s should declare %d replicas, declares %v", name, replicas, last)
}

func cronJobSuspended(t *testing.T, ctx *Context, name string) bool {
	t.Helper()
	cronJob, err := fixture.KubeClientset.BatchV1().CronJobs(ctx.DeploymentNamespace()).Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend
}

func TestSuspendResumeWithAutomatedSync(t *testing.T) {
	ctx := Given(t)
	ctx.Path("suspend").
		When().
		CreateFromFile(func(app *Application) {
			app.Spec.SyncPolicy = &SyncPolicy{Automated: &SyncPolicyAutomated{SelfHeal: new(true)}}
		}).
		Then().
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 2)
			assert.False(t, cronJobSuspended(t, ctx, "nightly"))
			toggleSuspend(t, ctx)
		}).
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(HealthIs(health.HealthStatusSuspended)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 0)
			assert.True(t, cronJobSuspended(t, ctx, "nightly"))

			// self-heal keeps the suspended form
			_, err := fixture.KubeClientset.AppsV1().Deployments(ctx.DeploymentNamespace()).Patch(t.Context(), "main",
				types.MergePatchType, []byte(`{"spec":{"replicas":1}}`), metav1.PatchOptions{})
			require.NoError(t, err)
			waitForDeploymentReplicas(t, ctx, "main", 0)

			toggleSuspend(t, ctx)
		}).
		Expect(notSuspended()).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(HealthIs(health.HealthStatusHealthy)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 2)
			assert.False(t, cronJobSuspended(t, ctx, "nightly"))
		})
}

func TestSuspendResumeWithoutAutomatedSync(t *testing.T) {
	ctx := Given(t)
	ctx.Path("suspend").
		When().
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(_ *Application) {
			toggleSuspend(t, ctx)
		}).
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(HealthIs(health.HealthStatusSuspended)).
		And(func(app *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 0)
			assert.True(t, cronJobSuspended(t, ctx, "nightly"))
			require.NotNil(t, app.Status.OperationState)
			assert.True(t, app.Status.OperationState.Operation.InitiatedBy.Automated)
			toggleSuspend(t, ctx)
		}).
		Expect(notSuspended()).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 2)
			assert.False(t, cronJobSuspended(t, ctx, "nightly"))
		})
}

func TestSuspendedSyncSkipsHooks(t *testing.T) {
	var hookUID types.UID
	ctx := Given(t)
	ctx.Path("suspend-with-hook").
		When().
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		And(func(app *Application) {
			ranHook := false
			for _, res := range app.Status.OperationState.SyncResult.Resources {
				ranHook = ranHook || (res.Kind == "Job" && res.Name == "migrate" && res.HookType == HookTypePreSync)
			}
			require.True(t, ranHook, "the PreSync hook should run before the application is suspended")
			job, err := fixture.KubeClientset.BatchV1().Jobs(ctx.DeploymentNamespace()).Get(t.Context(), "migrate", metav1.GetOptions{})
			require.NoError(t, err)
			hookUID = job.UID
			toggleSuspend(t, ctx)
		}).
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		When().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(app *Application) {
			for _, res := range app.Status.OperationState.SyncResult.Resources {
				assert.Empty(t, res.HookType, "hook %s/%s ran while the application was suspended", res.Kind, res.Name)
			}
			job, err := fixture.KubeClientset.BatchV1().Jobs(ctx.DeploymentNamespace()).Get(t.Context(), "migrate", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, hookUID, job.UID, "the PreSync hook was recreated")
			waitForDeploymentReplicas(t, ctx, "main", 0)
		})
}

// TestSuspendedSyncDoesNotWaitOnSuspendedResources suspends a ConfigMap with a suspend script from argocd-cm and a
// health check that reports it Suspended, as an operator-managed database would be. A full sync of the suspended
// application must still get past the wave of that ConfigMap.
func TestSuspendedSyncDoesNotWaitOnSuspendedResources(t *testing.T) {
	ctx := Given(t)
	ctx.Path("suspend-waves").
		ResourceOverrides(map[string]ResourceOverride{
			"ConfigMap": {
				SuspendLua: `if obj.metadata.annotations == nil then
  obj.metadata.annotations = {}
end
obj.metadata.annotations["e2e.argoproj.io/hibernated"] = "true"
return obj`,
				HealthLua: `if obj.metadata.annotations ~= nil and obj.metadata.annotations["e2e.argoproj.io/hibernated"] == "true" then
  return {status = "Suspended", message = "hibernated"}
end
return {status = "Healthy"}`,
			},
		}).
		When().
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(HealthIs(health.HealthStatusHealthy)).
		And(func(_ *Application) {
			toggleSuspend(t, ctx)
		}).
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		Expect(ResourceHealthIs("ConfigMap", "database", health.HealthStatusSuspended)).
		Expect(HealthIs(health.HealthStatusSuspended)).
		When().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 0)
		})
}

func TestSuspendWarnsAboutDaemonSets(t *testing.T) {
	ctx := Given(t)
	ctx.Path("suspend-daemonset").
		When().
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(NoConditions()).
		And(func(_ *Application) {
			toggleSuspend(t, ctx)
		}).
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		Expect(Condition(ApplicationConditionSuspendWarning, "apps/DaemonSet/agent")).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 0)
			toggleSuspend(t, ctx)
		}).
		Expect(notSuspended()).
		Expect(NoConditions())
}

// TestSuspendWithHorizontalPodAutoscaler suspends a Deployment whose replicas are managed by an HPA and not declared
// in Git. The HPA must not scale the suspended Deployment back up, and must take over again after resume.
func TestSuspendWithHorizontalPodAutoscaler(t *testing.T) {
	ctx := Given(t)
	ctx.Path("suspend-hpa").
		When().
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 1)
			toggleSuspend(t, ctx)
		}).
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 0)
			// the HPA reports that it does not scale a target with zero replicas
			err := wait.PollUntilContextTimeout(t.Context(), time.Second, suspendWaitTimeout, true, func(context.Context) (bool, error) {
				hpa, err := fixture.KubeClientset.AutoscalingV2().HorizontalPodAutoscalers(ctx.DeploymentNamespace()).Get(t.Context(), "main", metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				return hasHPAScalingDisabled(hpa), nil
			})
			require.NoError(t, err, "the HPA should report ScalingDisabled while the Deployment is suspended")
			// and the Deployment stays at zero
			time.Sleep(20 * time.Second)
			waitForDeploymentReplicas(t, ctx, "main", 0)
			toggleSuspend(t, ctx)
		}).
		Expect(notSuspended()).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 1)
		})
}

func hasHPAScalingDisabled(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	for _, condition := range hpa.Status.Conditions {
		if condition.Type == autoscalingv2.ScalingActive && condition.Reason == "ScalingDisabled" {
			return true
		}
	}
	return false
}

// givenParentOfSuspendableChild creates a parent Application that manages a child Application, which deploys the
// suspend testdata into the test's deployment namespace.
func givenParentOfSuspendableChild(t *testing.T, syncOptions ...string) (*Context, string) {
	t.Helper()
	ctx := Given(t)
	childName := ctx.GetName() + "-child"
	ctx.Path("suspend-app-of-apps").
		When().
		PatchFile("child.yaml", fmt.Sprintf(`[
			{"op": "replace", "path": "/metadata/name", "value": %q},
			{"op": "replace", "path": "/metadata/namespace", "value": %q},
			{"op": "replace", "path": "/spec/source/repoURL", "value": %q},
			{"op": "replace", "path": "/spec/destination/namespace", "value": %q}
		]`, childName, fixture.TestNamespace(), fixture.RepoURL(fixture.RepoURLTypeFile), ctx.DeploymentNamespace())).
		CreateFromFile(func(app *Application) {
			app.Spec.SyncPolicy = &SyncPolicy{
				Automated:   &SyncPolicyAutomated{SelfHeal: new(true)},
				SyncOptions: syncOptions,
			}
		}).
		Then().
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(_ *Application) {
			waitForApplication(t, childName, func(child *Application) bool {
				return child.Status.Sync.Status == SyncStatusCodeSynced
			})
			waitForDeploymentReplicas(t, ctx, "main", 2)
		})
	return ctx, childName
}

func waitForApplication(t *testing.T, name string, predicate func(*Application) bool) *Application {
	t.Helper()
	var app *Application
	err := wait.PollUntilContextTimeout(t.Context(), time.Second, suspendWaitTimeout, true, func(context.Context) (bool, error) {
		var err error
		app, err = fixture.AppClientset.ArgoprojV1alpha1().Applications(fixture.TestNamespace()).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return predicate(app), nil
	})
	require.NoError(t, err, "Application %s did not reach the expected state, status: %+v", name, app)
	return app
}

func testSuspendedChildIsKeptByParent(t *testing.T, syncOptions ...string) {
	t.Helper()
	ctx, childName := givenParentOfSuspendableChild(t, syncOptions...)
	runToggleSuspend(t, fixture.TestNamespace(), childName)

	waitForApplication(t, childName, func(child *Application) bool {
		return child.Status.Suspend != nil && child.Status.Suspend.Phase == ApplicationSuspendPhaseSuspended
	})
	waitForDeploymentReplicas(t, ctx, "main", 0)

	// the parent neither reports the annotation as drift nor removes it
	ctx.When().Refresh(RefreshTypeNormal).Then().
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(_ *Application) {
			time.Sleep(10 * time.Second)
			child := waitForApplication(t, childName, func(*Application) bool { return true })
			assert.True(t, child.IsSuspended(), "the parent removed the suspend annotation of its child")
		})
}

func TestSuspendedChildIsKeptByParent(t *testing.T) {
	testSuspendedChildIsKeptByParent(t)
}

func TestSuspendedChildIsKeptByParentWithServerSideApply(t *testing.T) {
	testSuspendedChildIsKeptByParent(t, "ServerSideApply=true")
}

func TestSuspendParentSuspendsChildren(t *testing.T) {
	ctx, childName := givenParentOfSuspendableChild(t)
	toggleSuspend(t, ctx)

	ctx.When().Then().
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		And(func(_ *Application) {
			waitForApplication(t, childName, func(child *Application) bool {
				return child.IsSuspended() && child.Status.Suspend != nil && child.Status.Suspend.Phase == ApplicationSuspendPhaseSuspended
			})
			waitForDeploymentReplicas(t, ctx, "main", 0)
			toggleSuspend(t, ctx)
		}).
		Expect(notSuspended()).
		And(func(_ *Application) {
			waitForApplication(t, childName, func(child *Application) bool {
				return !child.IsSuspended() && child.Status.Suspend == nil
			})
			waitForDeploymentReplicas(t, ctx, "main", 2)
		})
}

// setDenySyncWindow adds or removes a sync window that denies all syncs of the default project.
func setDenySyncWindow(t *testing.T, deny bool) {
	t.Helper()
	proj, err := fixture.AppClientset.ArgoprojV1alpha1().AppProjects(fixture.TestNamespace()).Get(t.Context(), "default", metav1.GetOptions{})
	require.NoError(t, err)
	proj.Spec.SyncWindows = nil
	if deny {
		proj.Spec.SyncWindows = SyncWindows{&InlineSyncWindow{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Applications: []string{"*"}}}
	}
	_, err = fixture.AppClientset.ArgoprojV1alpha1().AppProjects(fixture.TestNamespace()).Update(t.Context(), proj, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func TestSuspendWaitsForSyncWindow(t *testing.T) {
	ctx := Given(t)
	ctx.Path("suspend").
		When().
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 2)
			setDenySyncWindow(t, true)
			toggleSuspend(t, ctx)
		}).
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspending)).
		And(func(app *Application) {
			// the workloads keep running while the window denies syncs
			time.Sleep(15 * time.Second)
			waitForDeploymentReplicas(t, ctx, "main", 2)
			assert.NotEqual(t, health.HealthStatusSuspended, app.Status.Health.Status)
			setDenySyncWindow(t, false)
		}).
		// like automated sync, the suspend continues at the next reconciliation after the window closes
		When().
		Refresh(RefreshTypeNormal).
		Then().
		Expect(suspendPhaseIs(ApplicationSuspendPhaseSuspended)).
		Expect(HealthIs(health.HealthStatusSuspended)).
		And(func(_ *Application) {
			waitForDeploymentReplicas(t, ctx, "main", 0)
		})
}
