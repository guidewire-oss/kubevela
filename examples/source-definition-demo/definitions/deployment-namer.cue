import "strings"

"deployment-namer": {
	type: "source"
	annotations: {}
	labels: {}
	description: "Chained source: joins cluster + tenant facts and the component name into a lowercased deployment name."
}
template: {
	// Assembles a deployment name from cluster + tenant facts plus the
	// consuming component's name. fromSource cannot concatenate values from
	// multiple sources, so this chained source takes them as inputs (fed via
	// fromSource in spec.sources[].properties) and returns the joined name.
	schema: name: string

	// Per-component name, so key on the component and the inputs.
	storage: {
		storageTTL:     "5m"
		onStaleFailure: "use-stale"
	}

	parameter: {
		region:     string
		zone:       string
		department: string
		tenant:     string
	}

	// Lowercase and join with '-'; context.name is the component name.
	_raw: "\(parameter.region)-\(parameter.zone)-\(parameter.department)-\(parameter.tenant)-\(context.name)"

	output: name: strings.ToLower(_raw)
}
