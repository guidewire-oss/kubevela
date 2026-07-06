import (
	"list"
	"strings"
	"vela/builtin"
	"vela/multicluster"
	"vela/oam"
)

"deploy-component": {
	type: "workflow-step"
	annotations: {
		"category": "Application Delivery"
	}
	labels: {
		"scope": "Application"
	}
	description: "Deploy a single component to target cluster(s) resolved from topology policies."
}

template: {
	components: oam.#LoadComponets

	// Iterating (not an indexed lookup) avoids evaluating before "components" resolves.
	_loadedNames: [for name, _ in components.$returns.value {name}]
	_missingComponents: [for name in [parameter.component] if !list.Contains(_loadedNames, name) {name}]

	if len(_missingComponents) > 0 {
		validateComponent: builtin.#Fail & {
			$params: message: "component not found in application: \(strings.Join(_missingComponents, ", "))"
		}
	}

	if len(_missingComponents) == 0 {
		deploy: multicluster.#Deploy & {
			$params: {
				policies:                 parameter.policies
				components:               [parameter.component]
				parallelism:              1
				ignoreTerraformComponent: true
				dispatcher:               parameter.dispatcher
			}
		}
	}

	parameter: {
		// +usage=the name of the component in the application to apply
		component: string
		// +usage=names of topology policies (declared at the Application level) used to resolve this component's target cluster(s)
		policies: *[] | [...string]
		// +usage=Optional Dispatcher CRD name. If omitted, controller-level default dispatcher is used (default: "default").
		dispatcher?: string
	}
}
