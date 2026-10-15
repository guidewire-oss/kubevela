import (
	"list"
	"strings"
	"vela/module"
)

"module": {
	// What this renders is fetched from a registry and can change while the
	// properties stay the same (a new tag, or a tag re-pushed), so every workflow
	// run applies it again rather than only when the properties change.
	annotations: "definition.oam.dev/redispatch-on-workflow-run": "true"
	attributes: {
		workload: type: "autodetects.core.oam.dev"
		// The rendered output is the module's own Application, so this component is
		// only as healthy as that Application is. Without this, a module whose
		// Application is failing reports healthy, because a component with no
		// healthPolicy is healthy by default.
		status: {
			healthPolicy: #"""
				_app: {
					phase:    *"" | string
					services: *[] | [...{...}]
					if context.output.status != _|_ {
						if context.output.status.status != _|_ {
							phase: context.output.status.status
						}
						if context.output.status.services != _|_ {
							services: context.output.status.services
						}
					}
				}
				_unhealthy: [ for s in _app.services if !s.healthy {s}]
				isHealth: _app.phase == "running" && len(_unhealthy) == 0
				"""#
			customStatus: #"""
				_app: {
					phase:    *"" | string
					services: *[] | [...{...}]
					if context.output.status != _|_ {
						if context.output.status.status != _|_ {
							phase: context.output.status.status
						}
						if context.output.status.services != _|_ {
							services: context.output.status.services
						}
					}
				}
				_unhealthy: [ for s in _app.services if !s.healthy {s}]
				// Ready:<healthy>/<total> of the owned Application's components, the same
				// shape webservice reports, naming the first failing one so there is
				// somewhere to look. Its own message stays on its own Application.
				_ready: len(_app.services) - len(_unhealthy)
				_first: name: *"" | string
				_reason: *"" | string
				if len(_unhealthy) > 0 {
					_first:  _unhealthy[0]
					_reason: " \(_first.name) unhealthy"
				}
				if len(_unhealthy) == 0 {
					if _app.phase != "running" {
						if _app.phase != "" {
							_reason: " \(_app.phase)"
						}
					}
				}
				if len(_app.services) > 0 {
					message: "Ready:\(_ready)/\(len(_app.services))\(_reason)"
				}
				// Nothing to count yet, so the phase is all there is to report.
				if len(_app.services) == 0 {
					if _app.phase == "" {
						message: "pending"
					}
					if _app.phase != "" {
						message: _app.phase
					}
				}
				"""#
		}
	}
	description: "Install a module: fetch it and render its owned Application"
	labels: {}
	type: "component"
}

template: {
	// The definitions install into the namespace of the Application that
	// installs this module, never one a property picks. An addon's owned
	// Application always lives in vela-system, so the addon renderer records
	// the installing Application's namespace on it in an annotation; that is
	// honored only on an owned addon Application, so a user's own Application
	// cannot use it to redirect a module.
	_namespace: string
	_fromAddon: context.appLabels["addons.oam.dev/name"] != _|_ &&
		context.appAnnotations["modules.oam.dev/install-namespace"] != _|_
	if _fromAddon {
		_namespace: context.appAnnotations["modules.oam.dev/install-namespace"]
	}
	if !_fromAddon {
		_namespace: context.namespace
	}

	// namespace used to be a parameter. A component still setting it must fail
	// rather than install somewhere its author did not ask for.
	if parameter.namespace != _|_ {
		_namespace: "namespace is no longer accepted: a module's definitions install into the namespace of the Application that installs it"
	}

	// The runtime clusters come from the clusters parameter and from any topology
	// policies the parameter names. Only a policy that lists its clusters can be
	// read here: a clusterLabelSelector needs the cluster list, which a component
	// cannot see, so it is refused rather than silently resolving to nothing.
	_policies: *[] | [...{...}]
	if context.appPolicies != _|_ {
		_policies: context.appPolicies
	}
	_topologies: {for p in _policies if p.type == "topology" {"\(p.name)": p}}
	_resolvable: [for n in parameter.policies if _topologies[n] != _|_ if _topologies[n].properties != _|_ if _topologies[n].properties.clusters != _|_ {n}]
	_unresolved: [for n in parameter.policies if !list.Contains(_resolvable, n) {n}]
	_policyCheck: "ok"
	if len(_unresolved) > 0 {
		_policyCheck: "policies must name topology policies of this Application that list their clusters; not usable: \(strings.Join(_unresolved, ", "))"
	}
	_policyClusters: [for n in _resolvable for c in _topologies[n].properties.clusters {c}]

	_render: module.#Render & {
		$params: {
			module:    parameter.module
			registry:  parameter.registry
			namespace: _namespace
			version:   parameter.version
			clusters:  list.Concat([parameter.clusters, _policyClusters])
		}
	}

	output: _render.$returns.application

	parameter: {
		// Module name; defaults to the component name.
		module: *context.name | string
		// Registry name; empty means the configured default.
		registry: *"" | string
		// Module package version (the OCI/ECR tag vela module publish writes from
		// _module.cue's version field). Empty means the latest published version.
		// This is not the API line (apiVersion v1/v2), which is unaffected.
		version: *"" | string
		// Names of the runtime clusters the module's runtime-placed objects install
		// to. The hub is always included for hub-placed objects and definitions.
		// Required only when the module places objects on the runtime.
		clusters: *[] | [...string]
		// Names of topology policies of this Application whose clusters are the
		// runtime clusters. Joined with clusters. A policy must list its clusters by
		// name; one that selects them by label is refused.
		policies: *[] | [...string]
	}
}
