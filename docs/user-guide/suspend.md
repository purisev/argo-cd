# Suspend an Application

Suspending an Application stops its workloads but keeps all of its resources: for example, Deployments scale to zero
replicas and CronJobs are suspended. The Application stays `Synced` and its health is `Suspended`. Resuming it
restores what Git declares.

## Suspend and resume

In the UI, select **Suspend** or **Resume** in the menu of the Application's root node.

From the CLI, run the `toggle-suspend` action:

```shell
argocd app actions run guestbook toggle-suspend --kind Application --group argoproj.io \
  --resource-name guestbook --namespace argocd
```

`--resource-name` and `--namespace` are only needed when the Application manages other Applications.

To suspend an Application declaratively, set the `argocd.argoproj.io/suspend` annotation to `"true"`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: guestbook
  namespace: argocd
  annotations:
    argocd.argoproj.io/suspend: "true"
```

## How it works

While an Application is suspended, Argo CD compares each resource with its suspended form, for example `replicas: 0`.
A [suspend script](../operator-manual/resource_suspend.md) defines that form for each kind.

- Argo CD syncs the resources that have a suspend script itself, even when automated sync is disabled. It does not
  prune. Self-heal keeps the workloads stopped.
- Argo CD only changes resources that Git declares. Jobs that a CronJob started before the suspend run to completion,
  and the suspended CronJob starts no new ones.
- Syncs of a suspended Application do not run [hooks](resource_hooks.md), and sync waves do not wait for `Suspended`
  resources.
- The diff shows the suspended form. `argocd app manifests` shows what Git declares.
- Suspend and resume follow [sync windows](sync_windows.md). If a window blocks them, they continue at the next
  reconciliation after the window closes.

`status.suspend.phase` is `Suspending`, `Suspended` or `Resuming`. The health becomes `Suspended` once suspending is
complete and all resources are healthy. `status.suspend` is removed once resuming is complete.

## Warnings

A `SuspendWarning` condition lists:

- DaemonSets and Pods, which cannot scale to zero and keep running.
- Resources whose suspended fields are hidden by [`ignoreDifferences`](diffing.md), for example `/spec/replicas`. They
  look synced but keep running.

To let a HorizontalPodAutoscaler manage replicas, leave `spec.replicas` out of Git instead of ignoring it. The
autoscaler does not scale a Deployment with zero replicas, and after resume the Deployment starts with one replica.

## ApplicationSets and app of apps

- An ApplicationSet keeps a suspend set on its Application, unless its template sets the annotation.
- A parent Application keeps a suspend set on its child, unless the parent's manifests set the annotation.
- Suspending a parent suspends its child Applications, and resuming the parent resumes them.

## RBAC

The action requires `action/argoproj.io/Application/toggle-suspend`, which can be granted without `update`:

```csv
p, role:night-shift, applications, action/argoproj.io/Application/toggle-suspend, dev/*, allow
```

> [!NOTE]
> `PreDelete` and `PostDelete` hooks still run when a suspended Application is deleted.
