# Claude Progress

## Current State

- **Branch:** feat/module-component-v3
- **Last updated:** 2026-09-19
- **Summary:** 263 files (96 new, 150 modified, 17 deleted); 34 commits: fix(registry): read a package only when its revision has moved, fix(status): report owned Application health on the app that renders it, fix(status): propagate addon and module render failures to application health, fix(webhook): stop resolving addon and module packages during admission, feat(module): refuse type: module render when the gate is off (+29 more)

## What to do next

- (not set)

## Session Log


### 2026-09-19T01:55:01Z
- 263 files (96 new, 150 modified, 17 deleted); 34 commits: fix(registry): read a package only when its revision has moved, fix(status): report owned Application health on the app that renders it, fix(status): propagate addon and module render failures to application health, fix(webhook): stop resolving addon and module packages during admission, feat(module): refuse type: module render when the gate is off (+29 more)
  New:
  - .claude
  - .claude-session-head
  - addon_test_CR.yml
  - cmd/core/app/config/helm.go
  - cmd/objtest/main.go
  - design/vela-core/keps/2.15-operations/design/02-permission-scenarios.md
  - design/vela-core/keps/2.15-operations/design/03-permission-components.md
  - design/vela-core/keps/2.15-operations/design/04-cluster-scope.md
  - design/vela-core/keps/2.23-plugins/README.md
  - design/vela-core/keps/2.24-component-config-policies/README.md
  - ... (+86 more)
  Modified:
  - .github/ISSUE_TEMPLATE/bug_report.yml
  - .github/ISSUE_TEMPLATE/enhancement_request.yml
  - .github/ISSUE_TEMPLATE/feature_request.yml
  - .github/workflows/e2e-test.yml
  - .gitignore
  - COMMUNITY.md
  - CONTRIBUTING.md
  - README.md
  - apis/types/types.go
  - charts/vela-core/README.md
  - ... (+140 more)
  Deleted:
  - docs/README.md
  - docs/WEBHOOK_DEBUGGING.md
  - pkg/addon/cache_oci_test.go
  - pkg/addon/cache_versioned_test.go
  - pkg/addon/helper_conflict_test.go
  - pkg/addon/helper_version_pin_test.go
  - pkg/addon/oci_registry.go
  - pkg/addon/reader_github.go
  - pkg/cue/definition/k8s_objects_health_test.go
  - pkg/cue/definition/module_template_test.go
  - ... (+7 more)

## Past Sessions
