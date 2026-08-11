"k8s-objects": {
	type: "component"
	annotations: {}
	labels: {}
	description: "K8s-objects allow users to specify raw K8s objects in properties"
	attributes: {
		workload: type: "autodetects.core.oam.dev"
		status: healthPolicy: #"""
			ready: {
				conditions: *[] | [...]
			} & {
				if context.output.status != _|_ {
					if context.output.status.conditions != _|_ {
						conditions: context.output.status.conditions
					}
				}
			}
			// readyConditionType is optional. An omitted OR empty value keeps the
			// historical behavior (healthy once applied) — which is what keeps every
			// existing k8s-objects component backward compatible: the health evaluation
			// gets the raw parameter, so the schema default (*"") is not applied here and
			// the absent field must be handled explicitly. A named type demands that
			// condition be True before this component reports healthy, so a component
			// that dependsOn it waits for real readiness rather than mere application.
			_wantType: *"" | string
			if parameter.readyConditionType != _|_ {
				_wantType: parameter.readyConditionType
			}
			isHealth: _wantType == "" || len([for c in ready.conditions if c.type == _wantType if c.status == "True" {c}]) > 0
			"""#
	}
}
template: {
	output: {
		if len(parameter.objects) > 0 {
			parameter.objects[0]
		}
		...
	}

	outputs: {
		for i, v in parameter.objects {
			if i > 0 {
				"objects-\(i)": v
			}
		}
	}
	parameter: {
		// The raw objects to apply. The first one carries the health check.
		objects: [...{}]
		// The status condition type that must be True before this component
		// reports healthy. Empty (default) means healthy once applied.
		readyConditionType: *"" | string
	}
}
