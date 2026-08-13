import (
	"vela/module"
)

"module": {
	annotations: {}
	attributes: workload: type: "autodetects.core.oam.dev"
	description: "Install a module: fetch it and render its owned Application (XRD, Compositions, definitions) with health-gated tier ordering."
	labels: {}
	type: "component"
}

template: {
	_render: module.#Render & {
		$params: {
			module:    parameter.module
			registry:  parameter.registry
			namespace: parameter.namespace
		}
	}

	output: _render.$returns.application

	parameter: {
		// Module name; defaults to the component name.
		module: *context.name | string
		// Registry name; empty means the configured default.
		registry: *"" | string
		// Install namespace; empty means the default system namespace (vela-system).
		namespace: *"" | string
	}
}
