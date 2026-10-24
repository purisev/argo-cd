# Resource Suspend Scripts

## Overview

When an Application is [suspended](../user-guide/suspend.md), Argo CD compares each of its resources with the resource's
suspended form. A suspend script is a Lua script that receives the resource from Git as `obj` and returns its
suspended form, for example with `spec.replicas` set to `0`.

## Built-in Suspend Scripts

The following kinds have built-in suspend scripts. Each kind links to its script:

{!docs/operator-manual/resource_suspend_builtin.md!}

Kinds get a built-in script when they can stop their Pods and keep their data. Settings that only pause a controller
or a rollout, such as Flux `suspend` or Deployment `spec.paused`, have no script. Neither do kinds that manage cluster
nodes, such as Cluster API `MachineDeployment`.

## Custom Suspend Scripts

Suspend scripts are defined in the `resource.customizations.suspend.<group>_<kind>` field of `argocd-cm`. A custom
script replaces the built-in one.

```yaml
resource.customizations.suspend.example.com_Database: |
  if obj.spec == nil then
    obj.spec = {}
  end
  obj.spec.paused = true
  return obj
```

A suspend script:

* must return the same object, with the same group, kind, name and namespace. Otherwise the Application gets a
  `ComparisonError` and is not synced.
* should not change its own output when it runs on it again. Argo CD runs it on live resources to check whether they
  are already suspended.
* applies to its exact group and kind. Wildcards are not supported.

To keep a kind running while its Application is suspended, return the object unchanged:

```yaml
resource.customizations.suspend.apps_StatefulSet: |
  return obj
```
