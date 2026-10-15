import "vela/test"

_rendered: $returns: application: {
	apiVersion: "core.oam.dev/v1beta1"
	kind:       "Application"
	metadata: {
		name:      "module-widget"
		namespace: "vela-system"
	}
}

"the module Application is the output": test.#ComponentRender & {
	definition: "module"
	context: {
		name:      "widget"
		namespace: "team-a"
	}
	mocks: "vela/module": "#Render": _rendered
	expect: {
		output: {kind: "Application", metadata: name: "module-widget"}
		calls: "vela/module": "#Render": [{$params: {
			module:    "widget"
			registry:  ""
			namespace: "team-a"
			version:   ""
		}}]
	}
}

"parameters pass through to module render": test.#ComponentRender & {
	definition: "module"
	context: {
		name:      "ignored"
		namespace: "team-a"
	}
	parameter: {
		module:   "widget-kit"
		registry: "internal"
		version:  "1.2.3"
	}
	mocks: "vela/module": "#Render": _rendered
	expect: calls: "vela/module": "#Render": [{$params: {
		module:    "widget-kit"
		registry:  "internal"
		namespace: "team-a"
		version:   "1.2.3"
	}}]
}

"an owned addon Application installs into the recorded namespace": test.#ComponentRender & {
	definition: "module"
	context: {
		name:      "widget"
		namespace: "vela-system"
		appLabels: "addons.oam.dev/name":                    "tenant-widgets"
		appAnnotations: "modules.oam.dev/install-namespace": "kit-tenant"
	}
	mocks: "vela/module": "#Render": _rendered
	expect: calls: "vela/module": "#Render": [{$params: namespace: "kit-tenant", ...}]
}

"the recorded namespace is ignored on a user Application": test.#ComponentRender & {
	definition: "module"
	context: {
		name:      "widget"
		namespace: "team-a"
		appAnnotations: "modules.oam.dev/install-namespace": "elsewhere"
	}
	mocks: "vela/module": "#Render": _rendered
	expect: calls: "vela/module": "#Render": [{$params: namespace: "team-a", ...}]
}

"a namespace property is refused": test.#ComponentRender & {
	definition: "module"
	context: {
		name:      "widget"
		namespace: "team-a"
	}
	parameter: namespace: "acme-system"
	mocks: "vela/module": "#Render": _rendered
	expect: error: =~"namespace is no longer accepted"
}

"clusters pass through to module render": test.#ComponentRender & {
	definition: "module"
	context: {
		name:      "widget"
		namespace: "team-a"
	}
	parameter: clusters: ["eu-1", "us-1"]
	mocks: "vela/module": "#Render": _rendered
	expect: calls: "vela/module": "#Render": [{$params: {
		module:   "widget"
		clusters: ["eu-1", "us-1"]
		...
	}}]
}

_topologies: [
	{name: "eu", type: "topology", properties: clusters: ["eu-1", "eu-2"]},
	{name: "us", type: "topology", properties: clusters: ["us-1"]},
	{name: "by-label", type: "topology", properties: clusterLabelSelector: region: "eu"},
	{name: "limits", type: "override", properties: components: []},
]

"a topology policy's clusters pass through to module render": test.#ComponentRender & {
	definition: "module"
	context: {
		name:        "widget"
		namespace:   "team-a"
		appPolicies: _topologies
	}
	parameter: policies: ["eu"]
	mocks: "vela/module": "#Render": _rendered
	expect: calls: "vela/module": "#Render": [{$params: {clusters: ["eu-1", "eu-2"], ...}}]
}

"policies not named are ignored": test.#ComponentRender & {
	definition: "module"
	context: {
		name:        "widget"
		namespace:   "team-a"
		appPolicies: _topologies
	}
	parameter: policies: ["us"]
	mocks: "vela/module": "#Render": _rendered
	expect: calls: "vela/module": "#Render": [{$params: {clusters: ["us-1"], ...}}]
}

"policy clusters join the clusters parameter": test.#ComponentRender & {
	definition: "module"
	context: {
		name:        "widget"
		namespace:   "team-a"
		appPolicies: _topologies
	}
	parameter: {
		clusters: ["edge-1"]
		policies: ["us"]
	}
	mocks: "vela/module": "#Render": _rendered
	expect: calls: "vela/module": "#Render": [{$params: {clusters: ["edge-1", "us-1"], ...}}]
}

"a policy that does not exist is refused": test.#ComponentRender & {
	definition: "module"
	context: {
		name:        "widget"
		namespace:   "team-a"
		appPolicies: _topologies
	}
	parameter: policies: ["nowhere"]
	mocks: "vela/module": "#Render": _rendered
	expect: error: =~"nowhere"
}

"a policy that names clusters by label is refused": test.#ComponentRender & {
	definition: "module"
	context: {
		name:        "widget"
		namespace:   "team-a"
		appPolicies: _topologies
	}
	parameter: policies: ["by-label"]
	mocks: "vela/module": "#Render": _rendered
	expect: error: =~"by-label"
}

"a policy that is not a topology is refused": test.#ComponentRender & {
	definition: "module"
	context: {
		name:        "widget"
		namespace:   "team-a"
		appPolicies: _topologies
	}
	parameter: policies: ["limits"]
	mocks: "vela/module": "#Render": _rendered
	expect: error: =~"limits"
}

"naming a policy on an Application with none is refused": test.#ComponentRender & {
	definition: "module"
	context: {
		name:      "widget"
		namespace: "team-a"
	}
	parameter: policies: ["eu"]
	mocks: "vela/module": "#Render": _rendered
	expect: error: =~"eu"
}
