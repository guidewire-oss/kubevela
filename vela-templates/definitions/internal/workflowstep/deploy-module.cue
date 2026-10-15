import (
	"vela/oam"
)

"deploy-module": {
	type: "workflow-step"
	annotations: {
		"category": "Application Delivery"
	}
	labels: {
		"scope": "Application"
	}
	description: "Install a module's components by placement: components of objects marked modules.oam.dev/placement=runtime go to each cluster in clusters, everything else (including definitions) goes to the hub. Components install in dependsOn order."
}
template: {
	// In dependsOn order, so an XRD tier is applied before the Compositions that
	// need it, whichever cluster each lands on.
	components: oam.#LoadComponetsInOrder

	_placementAnnotation: "modules.oam.dev/placement"

	deploy: {
		for comp in components.$returns.value {
			"\(comp.name)": {
				// A component is runtime-placed when any of its objects says so. The
				// renderer keeps a tier to one placement, and definitions never carry it.
				_runtimeObjects: [
					if comp.properties != _|_ if comp.properties.objects != _|_ for o in comp.properties.objects if o.metadata != _|_ if o.metadata.annotations != _|_ if o.metadata.annotations[_placementAnnotation] != _|_ if o.metadata.annotations[_placementAnnotation] == "runtime" {o},
				]

				if len(_runtimeObjects) == 0 {
					hub: oam.#ApplyComponent & {
						$params: {
							value:   comp
							cluster: "local"
						}
					}
				}

				if len(_runtimeObjects) > 0 {
					for target in parameter.clusters {
						"\(target)": oam.#ApplyComponent & {
							$params: {
								value:   comp
								cluster: target
							}
						}
					}
				}
			}
		}
	}

	parameter: {
		// +usage=The runtime clusters that runtime-placed components install to. The hub is always used for hub-placed components and definitions.
		clusters: [...string]
	}
}
