#!/usr/bin/env bash
find resource_customizations -name suspend.lua \
    | sed 's~resource_customizations/\(.*\)/suspend.lua~- [\1](https://github.com/argoproj/argo-cd/blob/master/resource_customizations/\1/suspend.lua)~' \
    | sort \
    | uniq \
    > docs/operator-manual/resource_suspend_builtin.md
