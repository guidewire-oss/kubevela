import (
	"list"
	"strings"
	"vela/builtin"
	"vela/oam"
)

"deploy-components": {
	type: "workflow-step"
	annotations: {
		"category": "Application Delivery"
	}
	labels: {
		"scope": "Application"
	}
	description: "Deploy each component to the cluster(s) resolved from its own topology policies. Applies are executed sequentially, one at a time -- unlike \"deploy\", there is no parallelism setting to configure."
}
template: {
	components: oam.#LoadComponets

	// Computed directly from the loaded component set (via iteration, not an
	// indexed lookup -- x[computedKey] against this field was empirically found
	// to evaluate before the "components" load task resolves, causing false
	// positives) so validation never depends on "deploy" and can gate it.
	_loadedNames: [for name, _ in components.$returns.value {name}]

	_missingComponents: [for entry in parameter.components if !list.Contains(_loadedNames, entry.name) {entry.name}]

	if len(_missingComponents) > 0 {
		validateComponents: builtin.#Fail & {
			$params: message: "component(s) not found in application: \(strings.Join(_missingComponents, ", "))"
		}
	}

	// Gated on validation so no component is applied to any cluster unless
	// every referenced name resolved -- avoids a partial rollout where valid
	// components deploy before an invalid name is caught.
	if len(_missingComponents) == 0 {
		deploy: {
			// Note: the loop variable must not be named "value" -- that would collide
			// with the "value:" field label inside $params below, and CUE would
			// resolve it as a self-reference (an unconstrained value) instead of
			// this binding, silently passing an empty component to ApplyComponent.
			for name, comp in components.$returns.value {
				for entry in parameter.components if entry.name == name {
					"\(name)": {
						placements: #GetPlacements & {
							$params: policies: entry.policies
						}
						apply: {
							for p in placements.$returns.placements {
								"\(p.cluster)-\(p.namespace)": oam.#ApplyComponent & {
									$params: {
										value:     comp
										cluster:   p.cluster
										namespace: p.namespace
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Declared locally (instead of reusing multicluster.#GetPlacementsFromTmulticlusterologyPolicies,
	// whose #do string is mistyped and does not match the registered provider handler) with the
	// correct #provider/#do values for the already-registered topology-placement resolver.
	#GetPlacements: {
		#provider: "multicluster"
		#do:       "get-placements-from-topology-policies"
		$params: policies: [...string]
		$returns?: placements: [...{
			cluster:   string
			namespace: string
		}]
	}

	parameter: {
		// +usage=Per-component mapping of which topology policies determine its target cluster(s)
		components: [...{
			// +usage=the name of the component in the application to apply
			name: string
			// +usage=names of topology policies (declared at the Application level) used to resolve this component's target cluster(s)
			policies: [...string]
		}]
	}
}
