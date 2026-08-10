"k8s-objects-ready": {
	type:        "component"
	annotations: {}
	labels:      {}
	description: "Raw K8s objects whose health is gated on a real status condition, so a component that dependsOn this one waits for actual readiness rather than mere application."
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
			// An empty readyConditionType means "applied is enough", matching
			// plain k8s-objects. A named type demands that condition be True.
			isHealth: parameter.readyConditionType == "" || len([for c in ready.conditions if c.type == parameter.readyConditionType if c.status == "True" {c}]) > 0
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
		// reports healthy. Empty means healthy once applied.
		readyConditionType: *"" | string
	}
}
