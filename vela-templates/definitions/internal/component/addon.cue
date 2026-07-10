import (
	"vela/addon"
)

"addon": {
	annotations: {}
	attributes: workload: type: "autodetects.core.oam.dev"
	description: "Install an addon as a component; the addon's Application and auxiliaries are tracked by this Application."
	labels: {}
	type: "component"
}

template: {
	_render: addon.#Render & {
		$params: {
			addon:               parameter.addon
			version:             parameter.version
			registry:            parameter.registry
			properties:          parameter.properties
			skipVersionValidate: parameter.skipVersionValidate
		}
	}

	output: _render.$returns.application

	outputs: {
		for i, r in _render.$returns.resources {
			"aux-\(i)": r
		}
	}

	parameter: {
		// Addon name; defaults to the component name.
		addon: *context.name | string
		// Exact version; empty means latest stable version.
		version: *"" | string
		// Registry name; empty means the configured default.
		registry: *"" | string
		// The addon's own parameters, passed through to its templates.
		properties: *{} | {...}
		// Skip the addon's vela/kubernetes SystemRequirements check (mirrors the
		// imperative skipVersionValidate escape hatch). Defaults to enforcing it.
		skipVersionValidate: *false | bool
	}
}
