# API Reference: `OperationTemplate` & `Operation`

Human-readable field reference for the two CRDs this KEP introduces. This is
a companion to [`README.md`](./README.md) — the KEP is the source of truth
for rationale and behavior; this page just tabulates the shape.

Both kinds use `apiVersion: core.oam.dev/v2alpha1`.

> **Don't confuse `source` with `sources[]`.** `spec.source` (singular, this
> KEP) is what an `Operation` acts on — a Component or Application.
> `spec.sources[]` (plural, KEP-2.16, Option 3 only) is an unrelated
> declarative-data-binding list. Coincidentally similar names, different
> concepts.

This reference reflects [Option 1](./README.md#option-1-static-template-context-read-by-the-step-definition)
(the KEP's baseline): `OperationTemplate` as static YAML with a literal
`workflow: WorkflowSpec`, no `sources[]` field. The `OperationParameters`,
`OperationSource`, `Operation`, and `status` shapes below hold under
[Option 3](./README.md#option-3-expressions-carry-context-into-generic-steps)
too, but Option 3 adds a `spec.sources[]` field to **both** `OperationTemplate`
and `Operation` (declarative source bindings, consumed via KEP-2.16's bounded
`$( )` expressions in step properties and in `Operation.spec.parameters`) —
see [Option 3 in Detail](./README.md#option-3-in-detail-expression-based-inputs).
Rows below marked "Option 3 only" reflect that addition; everything else is
common to both. Neither reflects [Option 2](./README.md#option-2-cue-template-rendered-at-invocation):
under Option 2 the artifact is a CUE-rendered X-Definition named
`OperationDefinition`, and `workflow` is produced by evaluating a CUE
template rather than written directly. Which option to adopt is still open
— see [Open Questions](./README.md#open-questions).

## `OperationTemplate`

Namespaced. No status subresource — it's a template, not a running thing.

| Field | Type | Required | Notes |
| - | - | - | - |
| `description` | string | no | human-readable summary shown by `vela operation list` |
| `attach` | [`OperationAttach`](#operationattach) | yes | what this template may be run against |
| `parameters` | [`OperationParameters`](#operationparameters) | no | what an operator may supply at invocation |
| `workflow` | `WorkflowSpec` (`github.com/kubevela/pkg`, reused verbatim) | yes | the steps this template runs — this shape is Option 1/3; see the note above for Option 2 |
| `sources` | `[]SourceBinding` (KEP-2.16 shape, reused verbatim) | no | **Option 3 only, unrelated to `Operation.spec.source` below** — declarative source bindings consumable from step properties via `$(source["name"].field)` |
| `runAs` | [`RunAs`](#runas) | no | identity the operation executes under; defaults to `mode: Platform` with no named account |
| `requireDirectGrant` | bool, default `false` | no | if `true`, this template cannot be reached transitively as a dispatched child — the invoker must hold a grant on it directly, see [Requiring a direct grant instead](./README.md#requiring-a-direct-grant-instead) |

### `OperationAttach`

| Field | Type | Required | Applies when | Notes |
| - | - | - | - | - |
| `scope` | `Component \| Application \| None`, default `Component` | no | — | see [Attachment](./README.md#attachment) |
| `allowedComponentTypes` | `[]string` | no | `scope: Component` | empty means unrestricted |
| `selector.matchLabels` | `map[string]string` | no | `scope: Application` | |
| `selector.matchExpressions` | `[]LabelSelectorRequirement` | no | `scope: Application` | |
| `selector.requiredComponentTypes` | `[]string` | no | `scope: Application` | all listed types must be present |
| `clusterSelector.matchLabels` | `map[string]string` | no | every scope, `None` included | restricts which clusters the template may run against; matched against `VirtualCluster` labels |

`allowedComponentTypes` and `selector` are rejected at admission when set
under the wrong scope, and both are rejected under `scope: None`.
`clusterSelector` is the one field valid in all three.

Under `scope: None`, `spec.source` on the `Operation` is omitted entirely —
see [Scope: None, the unattached case](./README.md#scope-none-the-unattached-case).

### `OperationParameters`

Exactly one of the two must be set if `parameters` is present at all — the
controller resolves whichever is there when the `Operation` is admitted.

| Field | Type | Notes |
| - | - | - |
| `openAPIV3` | OpenAPI v3 schema (object) | used by the controller to validate supplied parameters at admission |
| `cue` | string, a CUE `parameter{}` block | unified against supplied parameters; CUE's own errors reported |

### `RunAs`

| Field | Type | Required | Notes |
| - | - | - | - |
| `mode` | `Platform \| Invoker`, default `Platform` | yes | `Platform` runs as a service account; `Invoker` impersonates whoever created the `Operation` |
| `serviceAccountName` | string | only meaningful with `mode: Platform` | the template author must hold `use` on this service account; rejected at admission if combined with `mode: Invoker` |

See [Choosing the identity, per template](./README.md#choosing-the-identity-per-template)
for the full resolution order, including the cluster-wide
`OperationsRunAsInvoker` setting that can override `mode` entirely.

---

## `Operation`

Namespaced, run-to-completion. Has a status subresource — one execution of
an `OperationTemplate`. Recurring operations are new `Operation` objects,
not a reused one.

| Field | Type | Required | Notes |
| - | - | - | - |
| `template` | string | yes | names the `OperationTemplate` to invoke |
| `source` | [`OperationSource`](#operationsource) | required for every scope except `None`, where it must be omitted entirely | what this operation acts on — not to be confused with `sources[]` below |
| `clusters` | `[]string` | no | restricts which of the source's clusters are operated on; omitted means every cluster the source is dispatched to. Under `scope: None` there is no source to dispatch from, so `clusters` names them directly |
| `parameters` | raw values (schema-validated per `OperationParameters`) | no | flags only — anything describing the source is read from context by the steps themselves |
| `sources` | `[]SourceBinding` (KEP-2.16 shape, reused verbatim) | no | **Option 3 only, unrelated to `source` above** — declarative source bindings consumable from `spec.parameters` via `$(source["name"].field)` |
| `retention.ttlAfterFinished` | duration | no | deletes the `Operation` record after this delay once finished |
| `retention.onFailure` | string, e.g. `Retain` | no | overrides the TTL for failed runs so a failure stays available for diagnosis |

### `OperationSource`

See [Source](./README.md#source).

| Field | Type | Required | Notes |
| - | - | - | - |
| `app` | string | yes | the owning Application's name |
| `component` | string | required for `attach.scope: Component`; omitted for `attach.scope: Application`, which targets the Application itself | the source Component's name within `app` |

### `status`

| Field | Type | Notes |
| - | - | - |
| `phase` | `Pending \| Running \| Succeeded \| Failed \| Suspended \| Cancelled` | |
| `startTime` / `completionTime` | `metav1.Time` | |
| `template` | object (`name`, `hash`, `spec`) | the template as copied at creation, with expressions intact; the snapshot is reused while values resolve according to their documented timing |
| `resolved.parameters` | object | the parameters actually used |
| `resolved.sources` | object | **Option 3 only** — what each declared source resolved to, for audit |
| `workflows` | `[]OperationWorkflowStatus` | one entry per cluster the operation ran a workflow on, even for a single-cluster run |

Each `workflows[]` entry carries `cluster`, `phase`, `finished`,
`contextBackend`, its `steps[]` (name, type, phase, and optional `meta`),
and any dispatched `children[]` (name, template, component, cluster, phase).

---

## Example: `scope: Component`

A complete, self-consistent set — the `OperationTemplate`, and the
`Operation` that invokes it. Taken directly from the KEP's own worked
example (see [`OperationTemplate`](./README.md#operationtemplate) and
[`Operation`](./README.md#operation)).

```yaml
apiVersion: core.oam.dev/v2alpha1
kind: OperationTemplate
metadata:
  name: s3-backup
  namespace: payments-prod
spec:
  attach:
    scope: Component
    allowedComponentTypes: [aws-s3-bucket]
  parameters:
    openAPIV3:
      type: object
      properties:
        verify:
          type: boolean
          default: true
          description: Verify the backup after writing it
        retentionDays:
          type: integer
          default: 30
  workflow:
    steps:
      - name: backup
        type: s3-backup-job
        properties:
          destination: acme-archive
      - name: verify
        if: context.operationParams.verify
        type: s3-verify-backup
      - name: record
        type: write-status
        properties:
          patch:
            lastBackup:
              status: success
      - name: cleanup
        if: always
        type: clean-jobs
        properties:
          labelSelector:
            operation.oam.dev/name: context.operationName
```

```yaml
apiVersion: core.oam.dev/v2alpha1
kind: Operation
metadata:
  name: backup-payments-db-20260804
  namespace: payments-prod
spec:
  template: s3-backup
  source:
    app: payments
    component: payments-db
  clusters: [eu-west-1, eu-central-1]
  parameters:
    retentionDays: 90
    verify: true
  retention:
    ttlAfterFinished: 1h
    onFailure: Retain
```

For the `scope: Application` and `scope: None` shapes, see the KEP's own
[Application-scope example](./README.md#worked-example) and
[`scope: None`](./README.md#scope-none-the-unattached-case) section
respectively — both follow the same `attach`/`source` pairing shown above.
