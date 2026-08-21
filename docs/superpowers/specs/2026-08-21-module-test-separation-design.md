# Module Test Separation Design

## Scope

This refactor is limited to `pkg/module` and its `service` subpackage. Other
packages under `pkg` are intentionally left unchanged.

The goal is to leave `pkg/module` with business logic, isolated unit tests,
and only the smallest checked-in fixture directly required by those unit
tests. Tests that communicate with an OCI registry or Kubernetes cluster,
and the data supporting those tests, belong under `test/e2e-test`.

## Current State

`pkg/module` contains two build-tagged integration tests:

- `pkg/module/publish_integration_test.go` publishes and pulls through an OCI
  registry, optionally using a real ECR registry.
- `pkg/module/service/fetch_integration_test.go` publishes to a real registry
  and fetches through `service.FetchModule`.

The package also contains two fixture modules. `minimal` is a small parser
fixture whose module metadata, version metadata, XRD, composition, and single
definition are each exercised by unit tests. `s3` contains large realistic
Crossplane resources and is used by integration tests and a few package tests
that do not depend on its domain-specific content.

The E2E suite already contains `test/e2e-test/testdata/module/e2e-widget`, an
in-cluster registry manifest, and a publish/deploy/controller workflow. Two
real-registry tests are also present in the working tree under
`test/e2e-test`: one validates publish/pull and OCI annotations, and one
validates fetching through the module service.

## Test Classification

The following remain unit tests in `pkg/module`:

- parsing and validation against `fs.FS`, temporary directories, or the
  minimal fixture;
- packaging and archive-shape behavior performed entirely in process;
- registry selection using the controller-runtime fake client;
- service fetch behavior with injected readers, pullers, and stores;
- memory filesystem behavior; and
- application rendering from in-memory module values.

The following belong in `test/e2e-test`:

- publishing to and pulling from an OCI registry;
- fetching a module through the service against a live registry;
- publishing and deploying through the CLI against an in-cluster registry;
  and
- asserting controller-created definitions and Application health.

## Repository Changes

Package tests that currently use `testdata/modules/s3` for generic valid-module
behavior will use `testdata/modules/minimal` instead. Expected module names and
archive paths will be updated accordingly. Tests that need a deliberately
special tree will continue creating it in `t.TempDir`, keeping each scenario
local and explicit.

After all package references are removed, delete
`pkg/module/testdata/modules/s3`. Keep
`pkg/module/testdata/modules/minimal` unchanged because all five files are
directly exercised: the valid parse checks all resources, and the optional
resource tests remove the XRD or composition from private copies.

Delete the two integration test files from `pkg/module`. Their live-registry
coverage will live in `test/e2e-test` and reuse the existing `e2e-widget`
fixture, avoiding a second realistic module tree. The existing cluster E2E
scenario remains the authoritative full publish/deploy/controller workflow.

Move the registry-backed CI job out of the unit-test workflow and into the
E2E workflow. It will run only the two plain Go registry tests, set both
registry environment variables to the job's local registry service, and not
invoke the cluster-backed Ginkgo suite. The regular E2E job continues to run
the full module workflow against Kubernetes.

## Path and Dependency Rules

No test under `pkg/module` may reference `test/e2e-test` or another package's
fixture tree. Package unit tests may reference only the local minimal fixture
or data created during the test.

E2E tests resolve their fixture paths from the repository root rather than
depending on the process working directory. The two registry tests share the
existing module E2E path constants and fixture.

Unrelated tracked and untracked worktree changes must remain untouched.

## Verification

Verification will cover:

1. A repository-wide reference scan proving no path still names the removed
   integration files or `pkg/module/testdata/modules/s3`.
2. `go test ./pkg/module/... -count=1` for all retained unit tests.
3. Targeted live-registry tests under `test/e2e-test` against a local OCI
   registry.
4. The focused module publish/deploy Ginkgo scenario against the supplied
   Kubernetes cluster.
5. A final diff and fixture inventory confirming `pkg/module/testdata`
   contains only the minimal module fixture.

If cluster access is unavailable from the execution sandbox, the exact failed
command and connectivity error will be reported rather than treating the E2E
verification as successful.
