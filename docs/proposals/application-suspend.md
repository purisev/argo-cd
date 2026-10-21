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
from Git or with the `toggle-suspend` resource action on the Application. While the annotation is set, the
application controller transforms the desired state before diffing and syncing: Deployments and StatefulSets
get `replicas: 0`, CronJobs get `suspend: true`, KEDA ScaledObjects are paused at zero replicas,
CloudNativePG Clusters are hibernated, and so on. The transformation for each kind is a Lua script, shipped
for common kinds and overridable in `argocd-cm`.

Because the transformed manifests are the desired state, the Application stays `Synced` and self-heal keeps
the workloads asleep instead of fighting the change. The controller applies the transformation itself even
when automated sync is disabled, skips hooks, and reports the Application health as `Suspended`. Removing the
annotation brings the desired state back to Git and the controller restores the workloads.

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
* The set of suspendable kinds and how each one is suspended is configurable without code changes.
* Applications managed by an ApplicationSet or by a parent Application can be suspended from the UI without
  the parent reverting the change.
* No new health status code.

### Non-Goals

* Scheduled suspend and resume. A sync window, a CronJob or an external controller can set the annotation.
* Suspending resources that have no scale-to-zero semantics, such as DaemonSets or bare Pods. They are
  reported, not changed.
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

### Implementation Details

#### The suspend annotation

```yaml
metadata:
  annotations:
    argocd.argoproj.io/suspend: "true"
```

Any other value, or no annotation, means the Application is not suspended. The flag is an annotation rather
than a `spec` field for two reasons. The ApplicationSet controller preserves selected annotations of the
Applications it manages (see below), so a flag set from the UI survives the ApplicationSet. A parent
Application in the app-of-apps pattern does not treat an annotation that its manifest does not declare as
drift, so the parent does not revert it either.

#### Per-kind suspend scripts

A suspend script is a Lua script that receives the desired object as `obj` and returns it in its suspended
form. Built-in scripts live in `resource_customizations/<group>/<kind>/suspend.lua` and are tested by
`suspend_test.yaml` next to them, in the same way as `health.lua` and `health_test.yaml`. An admin overrides
or adds a script in `argocd-cm`:

```yaml
data:
  resource.customizations.suspend.apps_Deployment: |
    obj.spec.replicas = 0
    return obj
```

The script is stored in a new `SuspendLua` field of `ResourceOverride`, next to `HealthLua` and `Actions`.

Built-in scripts cover kinds that have a way to stop their Pods while keeping their data and configuration:

| Kind | Suspended form |
|------|----------------|
| `apps/Deployment`, `apps/StatefulSet`, `apps/ReplicaSet`, `v1/ReplicationController` | `spec.replicas: 0` |
| `apps.openshift.io/DeploymentConfig` | `spec.replicas: 0` |
| `argoproj.io/Rollout` | `spec.replicas: 0` |
| `apps.kruise.io/CloneSet`, `apps.kruise.io/StatefulSet` | `spec.replicas: 0` |
| `rabbitmq.com/RabbitmqCluster` | `spec.replicas: 0` |
| `batch/CronJob` | `spec.suspend: true` |
| `batch/Job` | `spec.suspend: true` |
| `argoproj.io/CronWorkflow` | `spec.suspend: true` |
| `apps.kruise.io/AdvancedCronJob`, `apps.kruise.io/BroadcastJob` | `spec.paused: true` |
| `keda.sh/ScaledObject` | annotations `autoscaling.keda.sh/paused-replicas: "0"` and `autoscaling.keda.sh/paused: "true"` |
| `keda.sh/ScaledJob` | annotation `autoscaling.keda.sh/paused: "true"` |
| `postgresql.cnpg.io/Cluster` | annotation `cnpg.io/hibernation: "on"` |
| `psmdb.percona.com/PerconaServerMongoDB`, `pxc.percona.com/PerconaXtraDBCluster` | `spec.pause: true` |
| `kubevirt.io/VirtualMachine` | `spec.running: false` when the manifest sets `spec.running`, otherwise `spec.runStrategy: Halted` |
| `serving.kserve.io/InferenceService`, `serving.kserve.io/LLMInferenceService` | annotation `serving.kserve.io/stop: "true"` |
| `flink.apache.org/FlinkDeployment`, `flink.apache.org/FlinkSessionJob` | `spec.job.state: suspended` |
| `numaflow.numaproj.io/Pipeline`, `numaflow.numaproj.io/MonoVertex` | `spec.lifecycle.desiredPhase: Paused` |
| `numaplane.numaproj.io/PipelineRollout`, `numaplane.numaproj.io/MonoVertexRollout` | `desiredPhase: Paused` in the lifecycle of the embedded Pipeline or MonoVertex spec |
| `argoproj.io/Application` | annotation `argocd.argoproj.io/suspend: "true"` (cascade, see below) |

A Job that has already completed is not affected by `spec.suspend`, so only running Jobs stop.

Most of these fields already appear in health checks, actions or their test data in `resource_customizations`.
`argoproj.io/CronWorkflow` `spec.suspend` and `pxc.percona.com/PerconaXtraDBCluster` `spec.pause` do not; they
come from the operators' APIs, and the script tests must confirm them.

Several customizations pause something other than the Pods, and they get no suspend script:

* Flux sources, Kustomizations and HelmReleases, MariaDB `spec.suspend`, and the CloudNativePG
  `reconciliationLoop` annotation stop the controller's reconciliation while the workloads keep running.
* `apps/Deployment` `spec.paused` and the `updateStrategy` pause of OpenKruise workloads stop rollouts, not
  Pods.
* Cluster API `MachineDeployment` and `MachinePool` replicas and Gardener `Shoot` hibernation manage cluster
  nodes. Scaling them takes down infrastructure that other Applications run on, so an admin who wants that
  has to add the script in `argocd-cm`.

Kinds that may support scaling to zero but whose behaviour at zero is not confirmed by any existing health
check or action, such as OpenFaaS `Function`, SpinKube `SpinApp`, Strimzi `KafkaConnect`, Altinity
`ClickHouseInstallation` and OpenKruise `GameServerSet`, get built-in scripts only after that behaviour is
verified against the operator's documentation.

A script must return an object of the same group, kind, name and namespace. Any other result is a
comparison error.

#### Transforming the desired state

`appStateManager.CompareAppState` (`controller/state.go`) applies the suspend script of each target object
right after `NormalizeTargetObjects`, when the Application carries the annotation. Diffing, the sync status,
the resource tree and syncs all work on the transformed targets, because `SyncAppState` obtains its targets
from `CompareAppState` as well.

The controller leaves target objects alone when their kind has no suspend script. If any of them is a
DaemonSet or a Pod, the Application gets a `SuspendWarning` condition that lists them.

Fields that a suspend script sets are applied by Argo CD, so Argo CD owns them: they are recorded in
`last-applied-configuration` with client-side apply and owned by the Argo CD field manager with server-side
apply. When the annotation is removed, the next sync removes those fields or restores the values from Git.

#### Suspend status

The Application status gets an optional `suspend` field:

```yaml
status:
  suspend:
    phase: Suspended   # Suspending | Suspended | Resuming
    startedAt: "2026-10-21T18:00:00Z"
```

The controller uses this field to remember that a transition is in progress. Resuming an Application with
automated sync disabled depends on it, since otherwise the controller cannot tell a resource it suspended
from one that someone scaled to zero by hand.

#### Controller-initiated suspend and resume syncs

After `CompareAppState`, `processAppRefreshQueueItem` calls a new `suspendSync` before `autoSync`:

| Annotation | `status.suspend.phase` | Action |
|------------|------------------------|--------|
| set | absent or `Resuming` | set phase `Suspending` |
| set | `Suspending` | sync the out-of-sync resources that have a suspend script; once all of them are `Synced`, set phase `Suspended` |
| set | `Suspended` | nothing; self-heal, if enabled, keeps the state |
| absent | `Suspending` or `Suspended` | set phase `Resuming` |
| absent | `Resuming` | sync the out-of-sync resources that have a suspend script; once all of them are `Synced`, clear `status.suspend` |

The sync operation lists the resources explicitly, does not prune, and is initiated as automated. Selective
syncs already skip hooks (`controller/sync.go`). The operation follows the same rules as automated sync: it
is not started while another operation runs, it respects sync windows as an automated sync
(`CanSync(false, …)`), and a failed attempt is retried with the self-heal backoff and reported as a
`SyncError` condition. If `suspendSync` starts an operation, `autoSync` sees it and does nothing in that
iteration.

#### Syncs while suspended

Any sync of a suspended Application (automated, manual or rollback) applies the transformed desired state,
and `SyncAppState` follows two extra rules for it:

* Hooks are skipped. `PreSync` migrations and similar hooks would start Pods or fail against hibernated
  dependencies.
* Sync waves do not wait on a `Suspended` resource. The health override passed to the sync context reports
  `Suspended` as `Healthy` for wave progression only, so a paused ScaledObject or a hibernated database does
  not block the waves after it. The resource health shown to users is unchanged.

#### Application health

`setApplicationHealth` keeps computing the worst health of the resources. When the Application carries the
annotation and that worst health is `Healthy`, the Application health becomes `Suspended` with the message
`Application is suspended`. Worse states, such as `Progressing` while Pods terminate or `Degraded`, are
reported as they are.

`Suspended` is an existing status code, documented as "suspended and waiting for some external event to
resume". The UI, the CLI (`argocd app wait --suspended`), badges, metrics and notifications already handle
it.

#### The `toggle-suspend` action

`resource_customizations/argoproj.io/Application/actions/` gets a `toggle-suspend` action that sets or
removes the annotation. `discovery.lua` offers it as "Suspend" or "Resume" depending on the current value.
The action patches only the Application, and access to it is granted with
`action/argoproj.io/Application/toggle-suspend` in RBAC, separately from `applications, update`.

Resource actions on an Application must run against the control plane cluster, where Applications are
stored. The server resolves the destination cluster for them instead, so actions on Applications that deploy
to another cluster fail with `NotFound`. This proposal fixes that routing first: the Application itself is
patched on the control plane, and objects that an action creates still go to the destination cluster.

#### Applications managed by an ApplicationSet

The ApplicationSet controller overwrites the metadata of the Applications it manages, except for
annotations it preserves. `argocd.argoproj.io/suspend` is added to `defaultPreservedAnnotations`
(`applicationset/controllers/applicationset_controller.go`) with one difference from the other preserved
annotations: when the ApplicationSet template sets the annotation, the template value wins. A suspend set
from the UI therefore survives the ApplicationSet, and a suspend declared in Git can still be lifted in Git.

#### App of apps and cascade

A child Application suspended from the UI keeps the annotation, because the parent's manifest does not
declare it. If the parent's manifest does declare it, Git wins and self-heal reverts the UI change, as for
any other field.

The built-in suspend script for `argoproj.io/Application` sets the annotation on the child Applications in
the parent's desired state. Suspending the parent therefore suspends the whole tree, and resuming the parent
removes the annotation from the children. A child that was suspended individually before its parent was
suspended is resumed together with the parent, because the parent now owns the annotation.

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

Grant the action to a group that cannot otherwise update Applications:

```csv
p, role:night-shift, applications, action/argoproj.io/Application/toggle-suspend, dev/*, allow
```

### Security Considerations

* Suspend takes workloads offline. Whoever may run `toggle-suspend` or update an Application can cause an
  outage of that Application, as they already can with `sync`, `delete` or `rollback`. The dedicated RBAC
  action lets admins grant suspend without granting `update`.
* Suspend and resume syncs are controller syncs: they use the controller's credentials or, when sync
  impersonation is enabled, the impersonated service account of the destination, as automated syncs do.
* Suspend scripts come from Argo CD itself or from `argocd-cm`, which only admins control, and run in the
  same sandboxed Lua VM as health checks and actions.
* The `toggle-suspend` action patches only the Application it runs on. It cannot target another Application
  or a workload.

### Risks and Mitigations

* When Git does not set a workload's replicas, usually because an HPA manages them, the script scales the
  workload to 0, and on resume it comes back with the Kubernetes default of 1 replica before the HPA takes
  over. An HPA is expected to leave a target with 0 replicas alone. E2e tests must confirm both behaviours
  before the design relies on them.
* Someone who scales a workload of a suspended Application to 0 by hand gets the Git value back on resume,
  because that change looks the same as the suspended state. This is intended, since resume returns to what
  Git declares.
* Downgrading to a controller without this feature works like a resume: the desired state comes from Git
  again and the next sync restores the workloads.
* The diff view shows the transformed desired state, for example `replicas: 0` where Git says 3. The
  Application health message and the status panel say that the Application is suspended.

### Upgrade / Downgrade Strategy

* Upgrade needs no changes. The annotation is opt-in, and Applications without it behave exactly as before.
* The `ResourceOverride.SuspendLua` field and the optional `status.suspend` field are additive API changes and
  require regenerated API code and manifests.
* On downgrade, older controllers ignore the annotation and drop `status.suspend`, and suspended Applications
  resume at their next sync.

## Test Plan

Unit tests:

* every built-in `suspend.lua`, through `suspend_test.yaml` cases;
* `CompareAppState` with and without the annotation, including kinds without a script and the
  `SuspendWarning` condition;
* `setApplicationHealth`: `Healthy` becomes `Suspended` only with the annotation; worse states are kept;
* the `suspendSync` state machine: each transition, automated sync disabled, an operation in progress, a deny
  sync window, retry backoff;
* `SyncAppState` while suspended: hooks skipped, waves not blocked by `Suspended` resources;
* the ApplicationSet preservation rule, with and without the annotation in the template;
* `toggle-suspend` action and discovery; actions on an Application whose destination is another cluster.

End-to-end tests:

* suspend and resume of an Application with a Deployment, a StatefulSet and a CronJob, with automated sync
  enabled and disabled;
* a `PreSync` hook does not run while the Application is suspended;
* a multi-wave sync of a suspended Application completes when a resource in an early wave is `Suspended`;
* a Deployment with an HPA stays at 0 replicas while suspended and is scaled by the HPA after resume, with
  `spec.replicas` present in Git and absent from Git;
* a child Application suspended from the UI is not reverted by its parent, with client-side and server-side
  apply;
* suspending a parent Application suspends its children, and resuming it resumes them;
* an Application generated by an ApplicationSet keeps a UI-set suspend after the ApplicationSet reconciles;
* KEDA ScaledObject and CloudNativePG Cluster manifests (CRDs installed, operators not required) receive and
  lose their annotations;
* a deny sync window postpones suspend until the window closes.

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

Ignored fields are hidden from the diff and nothing more. Whether a later sync keeps or resets them depends
on the apply mode and on `RespectIgnoreDifferences`. The Application reports `Synced` while it drifts from
Git, and Argo CD would have to edit the user's `spec.ignoreDifferences` and undo exactly that edit on
resume.

### A new health status such as `Paused`

Health codes are a closed set that sync waves, `app wait`, badges, metrics labels, notifications, the UI and
user-written Lua health checks of Applications all depend on. A new code breaks each of them, and the
existing `Suspended` already means what this feature needs.

### A `spec` field instead of an annotation

The ApplicationSet and parent Applications would revert a suspend set from the UI unless every user
configured `ignoreApplicationDifferences` or `ignoreDifferences` on the parent.

### Deleting the resources and keeping the Application

Deleting loses state that scaling keeps, and recreating resources takes longer than scaling them.
