---
title: Suspend an Application
authors:
  - "@purisev"
sponsors:
  - TBD
reviewers:
  - TBD
approvers:
  - TBD

creation-date: 2026-10-21
last-updated: 2026-10-21
---

# Suspend an Application

This proposal lets a user scale every workload of an Application down with one action and bring it back
with another, while the Application stays in sync with Git the whole time.

## Summary

A user marks an Application as suspended with the `argocd.argoproj.io/suspend: "true"` annotation, either
in Git or with the `toggle-suspend` resource action on the Application. While the annotation is set, Argo CD
transforms the desired state of the Application's resources before it compares and syncs them, for example
`replicas: 0` for a Deployment or `suspend: true` for a CronJob. Each kind is transformed by a Lua script,
shipped for common kinds and overridable in `argocd-cm`.

Because the transformed manifests are the desired state, the Application stays `Synced` and self-heal keeps
the workloads stopped instead of fighting the change. Argo CD applies the transformation even when automated
sync is disabled, skips hooks, and reports the Application health as `Suspended`. Removing the annotation
makes Git the desired state again, and Argo CD restores the workloads.

## Motivation

Parking an Application today means one of:

* scaling each workload by hand, after disabling automated sync, and remembering the original replica counts;
* deleting the Application or its resources and recreating them later;
* committing replica changes to Git and reverting them.

Users ask for this mostly to save compute on non-production environments. Another common reason is a
database maintenance or credential rotation that needs every client disconnected for a while, after which
the environment should come back with its configuration intact.

### Goals

* One action suspends all suspendable workloads of an Application, and one action resumes them.
* While suspended, the Application is `Synced`, its health is `Suspended`, and self-heal holds the suspended
  state.
* Resuming restores exactly what Git declares. Argo CD does not store original replica counts.
* Suspend and resume work whether or not automated sync is enabled.
* Admins can change how a kind is suspended, or add kinds, without code changes.
* Applications managed by an ApplicationSet or by a parent Application can be suspended from the UI without
  the parent reverting the change.
* No new health status code.

### Non-Goals

* Scheduled suspend and resume. A sync window, a CronJob or an external controller can set the annotation.
* Suspending resources that have no scale-to-zero semantics, such as DaemonSets or bare Pods. Argo CD lists
  them in a warning and leaves them as they are.
* Deleting resources. Suspend keeps every resource, PersistentVolumeClaims included.
* A dedicated suspend panel in the UI. Suspend and resume are offered in the context menu of the
  Application's root node, where resource actions on the Application already appear.

## Proposal

### Use cases

#### Use case 1: park a development environment

As a developer, I suspend my Application in the evening and resume it in the morning. Pods are gone
overnight, volumes and configuration are kept, and the Application shows `Synced` and `Suspended`.

#### Use case 2: maintenance window for a database

As an operator, I suspend the Application that uses a database, rotate the database credentials, update the
Secret in Git and resume the Application. No Pod connects with the old credentials during the rotation.

#### Use case 3: an environment of many Applications

As a platform engineer, I suspend the parent Application of an app-of-apps tree. Every child Application is
suspended with it, and resuming the parent resumes the children.

### Implementation Details/Notes/Constraints

#### The suspend annotation

Only the value `"true"` suspends an Application. The flag is an annotation rather than a `spec` field so that
the ApplicationSet controller and parent Applications can leave it alone when it is set from the UI (see
below).

#### Suspend scripts

A suspend script is a Lua script that receives a desired object as `obj` and returns it in its suspended
form. The result must keep the object's group, kind, name and namespace. Built-in scripts live next to the
health checks in `resource_customizations/<group>/<kind>/suspend.lua`, and admins override or add them in
`argocd-cm` under `resource.customizations.suspend.<group>_<kind>`.

A kind gets a built-in script when it has a way to stop its Pods while keeping its data and configuration,
and when the script's tests confirm how the kind behaves once stopped. Switches that stop only an operator's
reconciliation or a rollout, such as Flux `suspend` or Deployment `spec.paused`, do not qualify. Neither do
switches that stop cluster infrastructure, such as Cluster API machine replicas, since other Applications
run on it; an admin who wants that adds the script in `argocd-cm`. The list of built-in scripts is
generated into the operator manual, like the list of built-in resource actions.

#### Desired state while suspended

While the annotation is set, Argo CD runs the suspend scripts on the desired objects before diffing, so the
diff, the sync status and every sync of the Application use the suspended form. Argo CD applies the fields
that the scripts set and therefore owns them, with client-side and with server-side apply. Once the
annotation is removed, the next sync of those resources removes the fields or restores the values from Git.

Every sync of a suspended Application, whether automated, manual or a rollback, follows two more rules:

* Hooks do not run. `PreSync` migrations and similar hooks would start Pods or fail against hibernated
  dependencies.
* Sync waves do not wait for a resource whose health is `Suspended`, so a paused ScaledObject or a
  hibernated database does not block the waves after it. The resource health shown to users is unchanged.

#### Suspend and resume without automated sync

Argo CD suspends and resumes the workloads itself, so the action takes effect even when automated sync is
disabled. When the annotation appears or disappears, Argo CD syncs the out-of-sync resources that have a
suspend script, without hooks and without pruning. These syncs follow the rules of automated sync: they wait
for a running operation to finish, respect sync windows and back off after a failure.

To resume correctly, Argo CD has to know that it suspended the workloads, otherwise it could not tell them
from workloads that someone scaled to zero by hand. The Application status gets an optional field for this:

```yaml
status:
  suspend:
    phase: Suspended   # Suspending | Suspended | Resuming
    startedAt: "2026-10-21T18:00:00Z"
```

`Suspending` and `Resuming` last until the affected resources are `Synced`. After a resume completes, the
field is removed.

#### Application health

The health of a suspended Application is `Suspended` when all of its resources are healthy. A worse state,
such as `Progressing` while Pods terminate or `Degraded`, is reported as it is. `Suspended` is an existing
status code that the UI, the CLI, badges, metrics and notifications already handle.

#### The `toggle-suspend` action

The Application gets a `toggle-suspend` resource action, shown as "Suspend" or "Resume" in the context menu
of the root node. It only sets or removes the annotation on that Application, and RBAC grants it as
`action/argoproj.io/Application/toggle-suspend`, separately from `applications, update`.

Resource actions on an Application must run against the control plane cluster, where Applications are
stored. Today they run against the destination cluster, so they fail for Applications that deploy to another
cluster. This proposal fixes that first.

#### ApplicationSets and app of apps

The ApplicationSet controller keeps a small set of operational annotations, such as the refresh annotation,
when it updates the Applications it manages. The suspend annotation joins that set, with one difference:
when the ApplicationSet template sets the annotation, the template wins. A suspend set from the UI therefore
survives the ApplicationSet, and a suspend declared in Git can still be lifted in Git.

A parent Application does not revert the annotation on a child unless the parent's manifest declares it. If
it does, Git wins as for any other field.

The built-in suspend script for `argoproj.io/Application` sets the annotation on child Applications, so
suspending a parent suspends the whole tree and resuming the parent resumes it. A child that was suspended on
its own before the parent was suspended is resumed together with the parent.

### Detailed examples

Suspend from the CLI:

```shell
argocd app actions run guestbook toggle-suspend --kind Application --group argoproj.io
```

Suspend declaratively:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: guestbook
  namespace: argocd
  annotations:
    argocd.argoproj.io/suspend: "true"
spec:
  # unchanged
```

Change how Deployments are suspended:

```yaml
data:
  resource.customizations.suspend.apps_Deployment: |
    obj.spec.replicas = 0
    return obj
```

Grant the action to a group that cannot otherwise update Applications:

```csv
p, role:night-shift, applications, action/argoproj.io/Application/toggle-suspend, dev/*, allow
```

### Test plan

* Suspend and resume an Application with automated sync enabled and disabled.
* A `PreSync` hook does not run, and a multi-wave sync completes, while the Application is suspended.
* A Deployment with an HPA stays at 0 replicas while suspended and is scaled by the HPA after resume, with
  and without `spec.replicas` in Git.
* A child Application suspended from the UI is not reverted by its parent, with client-side and server-side
  apply, and a suspended parent suspends its children.
* An Application generated by an ApplicationSet keeps a suspend set from the UI.
* A deny sync window postpones suspend until the window closes.

### Security Considerations

* Suspend takes workloads offline. Whoever may run `toggle-suspend` or update an Application can cause an
  outage of that Application, as they already can with sync, delete or rollback. The dedicated RBAC action
  lets admins grant suspend without granting `update`.
* Suspend and resume syncs use the same credentials as automated syncs, including sync impersonation when it
  is enabled.
* Suspend scripts come from Argo CD itself or from `argocd-cm`, which only admins control, and run in the
  same sandboxed Lua VM as health checks and actions.
* The `toggle-suspend` action changes only the Application it runs on.

### Risks and Mitigations

* When Git does not set a workload's replicas, usually because an HPA manages them, the workload comes back
  with the Kubernetes default of 1 replica on resume before the HPA takes over. An HPA is expected to leave a
  target with 0 replicas alone while it is suspended. Tests must confirm both before the design relies on
  them.
* Someone who scales a workload of a suspended Application to 0 by hand gets the Git value back on resume.
  This is intended, since resume returns to what Git declares.
* The diff shows the suspended form, for example `replicas: 0` where Git says 3. The health message states
  that the Application is suspended.

### Upgrade / Downgrade Strategy

* Upgrade needs no changes. The annotation is opt-in, and Applications without it behave as before.
* The new status field and the new `argocd-cm` key are additive.
* Older versions ignore the annotation, so after a downgrade suspended Applications resume at their next
  sync.

## Drawbacks

* Without the annotation, the desired state is what the Application's sources render with the parameters in
  its spec. Suspend scripts add one more input, so while the annotation is set Argo CD applies something
  that Git alone does not describe.
* A suspend set from the UI is state outside Git. It is visible on the Application and reverts to Git on
  resume, but it is not reviewed like a commit.

## Alternatives

### An action that patches every workload

Resource actions can only patch the object they run on, so this needs a server change that lets an
Application action write to its child resources on the destination cluster. The patched workloads then
drift from Git, self-heal reverts them, and resume needs stored replica counts.

### Automatically added `ignoreDifferences`

`ignoreDifferences` only hides fields from the diff. Whether a later sync keeps or resets them depends on
the apply mode and on `RespectIgnoreDifferences`. The Application reports `Synced` while it drifts from Git,
and Argo CD would have to edit the user's `spec.ignoreDifferences` and undo exactly that edit on resume.

### A new health status such as `Paused`

Health codes are a closed set that sync waves, `app wait`, badges, metrics labels, notifications, the UI and
user-written Lua health checks of Applications all depend on. A new code breaks each of them, and the
existing `Suspended` already means what this feature needs.

### A `spec` field instead of an annotation

The ApplicationSet and parent Applications would revert a suspend set from the UI unless every user
configured `ignoreApplicationDifferences` or `ignoreDifferences` on the parent.

### Deleting the resources and keeping the Application

Deleting loses state that scaling keeps, and recreating resources takes longer than scaling them.
