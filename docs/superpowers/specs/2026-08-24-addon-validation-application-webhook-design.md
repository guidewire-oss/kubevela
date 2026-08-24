# Addon validation in the Application webhook

**Date:** 2026-08-24
**Branch:** `feat/addon-component`
**Status:** Approved revised design

## Problem

Addon-as-component compatibility validation currently has a separate
`ValidatingWebhook` entry for `Application` resources. On Kubernetes 1.28 and
newer, that entry uses a CEL `matchConditions` expression so the API server only
sends Applications containing a `type: addon` component to the addon webhook.

The default certificate path uses `oamdev/kube-webhook-certgen:v2.4.1`. That
binary reads the whole `ValidatingWebhookConfiguration` through Kubernetes
v0.21 typed clients, changes `caBundle` and `failurePolicy`, and writes the whole
object back. The old API type has no `matchConditions` field, so the live object
loses the expression. The addon webhook then receives every Application create
and update request.

## Decision

Run addon compatibility validation as an optional branch of the existing shared
Application validating webhook. Remove the separate addon admission route and
chart webhook entry.

The Application webhook already receives every Application. Folding the check
into that path removes an extra admission round trip, avoids dependence on
`matchConditions`, and leaves certificate patching unchanged.

## Goals

- Run addon compatibility validation only when `EnableAddonComponent` is on.
- Avoid invoking addon validation for Applications without a `type: addon`
  component.
- Reuse the compatibility and component-property validation already implemented
  in the addon webhook package.
- Preserve fail-open behavior for registry, resolution, and discovery failures.
- Deny admission only for a confirmed system-requirement mismatch.
- Keep the addon validator independent of admission decoding and HTTP response
  construction.
- Produce structured, request-correlated logs without adding info-level noise to
  ordinary Application admissions.
- Keep the chart default for `enableAddonComponent` set to `false`; test and
  development installations enable it explicitly.

## Non-goals

- Changing or publishing a new `kube-webhook-certgen` image.
- Changing addon rendering or reconciliation behavior.
- Changing compatibility requirements or registry resolution semantics.
- Adding a new webhook, controller, CRD, or external chart dependency.
- Refactoring unrelated Application validation code.

## Architecture

### Application addon subpackage

Addon-specific implementation lives under:

```text
pkg/webhook/core.oam.dev/v1beta1/application/addon
```

The parent path communicates that this code validates addon components only as
part of Application admission. It must not live at `v1beta1/addon`, because
sibling directories at that level represent independently admitted Kubernetes
resource kinds and the standalone path implies an Addon webhook API that does
not exist.

The nested `application/addon` package owns addon-specific parsing and
compatibility validation, but it does not own an admission handler or route.

The nested package exposes a focused `Validator` with this responsibility:

1. Iterate Application components.
2. Ignore components whose type is not `addon`.
3. Decode the addon-specific property subset.
4. Respect `skipVersionValidate`.
5. Resolve the effective addon name, using the component name as the default.
6. Run the compatibility checker.
7. Return indexed `field.Error` values for confirmed incompatibilities.

The validator exposes a constructor for production use. Its compatibility
checker remains replaceable inside nested-package tests so registry-dependent
behavior can be tested hermetically.

`compat.go` continues to own registry resolution and system-requirement checks.
Its production checker becomes independent of an admission-handler receiver.

### Parent Application package

The Application package defines the narrow interface it consumes:

```go
type addonComponentValidator interface {
	ValidateComponents(context.Context, *v1beta1.Application) field.ErrorList
}
```

Defining the interface at the consumer keeps the addon package independent of
Application admission orchestration and allows Application tests to inject a
small fake.

The parent package imports its child with the `addonvalidation` alias. It keeps
feature-gate checks, `type: addon` detection, validator dispatch, and shared
admission error aggregation. The nested package must not import its parent, so
the dependency remains one-way and cycle-free.

`ValidatingHandler` receives an `addonComponentValidator`. Production
registration supplies the addon package's validator. Tests may supply a fake.
The handler keeps a safe default for existing tests that construct it directly.

### Validation flow

For Application creates and non-deleting updates:

1. Decode the Application once in the shared handler.
2. Run the existing annotation, definition, workflow, component, and trait
   validations.
3. Check `EnableAddonComponent`.
4. If disabled, return from the addon branch before scanning components.
5. Use `slices.ContainsFunc` to check for at least one component whose type is
   `addon`.
6. If none exists, return before invoking the addon validator.
7. Invoke addon validation once for the Application.
8. Append addon errors to the shared `field.ErrorList` so existing admission
   error formatting and request UID handling remain authoritative.

`slices.ContainsFunc` is part of the standard library supported by the project's
configured Go 1.23.8 toolchain. It makes the fast-path predicate explicit while
keeping the orchestration readable.

Updates reuse `ValidateCreate` through the existing `ValidateUpdate` flow, so the
same addon validation runs against the new Application state. Deleting updates
remain excluded by the handler before validation. Delete and unsupported
operations remain allowed without addon work.

## Logging

The shared handler stores its structured `ApplicationValidator` logger in the
request context. Addon validation retrieves that logger from the context, so all
addon logs include the admission UID and Application identity.

Logging levels and fields:

- Gate disabled: debug, with reason `feature-gate-disabled`.
- No addon component: debug, with reason `no-addon-component`.
- Addon validation started/completed: info, only for Applications that contain
  addon components, with `addonComponentCount` and duration where available.
- Malformed addon properties skipped: debug, with component name and error.
- Fail-open registry, version-resolution, or discovery outcome: info, with addon
  name, requested version, registry, reason, and error.
- Confirmed incompatibility: returned as a field error; the shared handler emits
  the final structured admission failure log with the request UID.

Logs must not include registry credentials, addon parameter payloads, or complete
Application objects.

## Error behavior

The current fail-open contract is preserved:

- Registry unavailable: allow and log.
- Addon or pinned version cannot be resolved: allow and log.
- Discovery client cannot be created: continue checks that do not require it and
  log the skipped Kubernetes-version portion.
- Requirement lookup cannot be performed: allow and log.
- Malformed component properties: let CUE/schema validation report the malformed
  input; do not introduce a second denial reason.
- `skipVersionValidate: true`: skip the compatibility checker.
- Confirmed `ErrVersionMismatch`: deny with the existing indexed component
  property path.

Because addon validation now shares the Application webhook, it also shares that
webhook's `failurePolicy`. The internal compatibility checker still fails open on
infrastructure errors, limiting denials to evaluated mismatches.

## Chart and registration changes

Remove from the validating webhook chart:

- The addon-specific webhook entry.
- Its `matchConditions` expression.
- Its CA-bundle lookup key.
- The certgen limitation comment that is no longer applicable.

Remove from webhook registration:

- The feature-gated addon route registration.
- The addon webhook path constant and decoder-based handler.

Keep unchanged:

- `featureGates.enableAddonComponent` in chart values.
- The `EnableAddonComponent` controller argument.
- The shared Application validating webhook configuration.
- Existing certgen jobs and RBAC.

Remove the obsolete top-level
`pkg/webhook/core.oam.dev/v1beta1/addon` directory after moving its validator,
compatibility checker, and tests into `application/addon`.

## Tests

### Addon validator unit tests

- No components and only non-addon components produce no checks or errors.
- One and multiple addon components invoke the checker with the expected addon,
  version, and registry.
- Empty addon name defaults to component name.
- `skipVersionValidate` suppresses the checker.
- Malformed properties fail open at this layer.
- Confirmed incompatibilities return indexed property paths.
- Registry and lookup errors preserve the fail-open contract.

### Application validator unit tests

- Gate disabled: the addon validator is not called, including for an addon
  component.
- Gate enabled with no addon component: the addon validator is not called.
- Gate enabled with an addon component: the addon validator is called once.
- Addon errors are included by `ValidateCreate`.
- Addon errors are included by `ValidateUpdate` for the new state.
- Deleting updates do not invoke addon validation.
- Existing Application validation behavior remains unchanged.

Feature-gate tests use the repository's feature-gate testing helper and do not
run in parallel while mutating global gate state.

### Chart and integration verification

- `helm template` contains the shared Application webhook and no addon-specific
  webhook name or path.
- The chart default remains `enableAddonComponent: false`.
- E2E setup explicitly sets `featureGates.enableAddonComponent=true`.
- The existing `e2e/addon-component` suite passes through the shared Application
  webhook.
- A live default-certgen installation has no addon webhook entry and therefore no
  `matchConditions` field to lose.

## Compatibility and rollout

- Kubernetes versions below 1.28 no longer incur a second addon webhook call.
- Kubernetes versions 1.28 and newer no longer depend on CEL admission match
  conditions for addon validation.
- Install, upgrade, and rollback use the existing Application webhook and
  certificate lifecycle.
- The public Application API and addon component schema do not change.
- The feature remains opt-in through `EnableAddonComponent`.
