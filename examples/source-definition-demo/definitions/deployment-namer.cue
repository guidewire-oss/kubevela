import "strings"

"deployment-namer": {
	type: "source"
	annotations: {}
	labels: {}
	description: "Chained source: joins cluster + tenant facts and the component name into a lowercased deployment name."
}
template: {
	// Assembles a deployment name from cluster + tenant facts plus the component
	// it is for. fromSource cannot concatenate values from multiple sources, so
	// this chained source takes them as inputs (fed via fromSource in
	// spec.sources[].properties) and returns the joined name.
	schema: name: string

	// The cache key is generated from the context this template reads. It reads
	// none, so entries are told apart by the properties below - which is what
	// makes one binding per component the right shape.
	storage: {
		storageTTL:     "5m"
		onStaleFailure: "use-stale"
	}

	parameter: {
		region:     string
		zone:       string
		department: string
		tenant:     string
		// The component this name is for. A source may not read the consuming
		// component - its output must not depend on who asked - so the binding
		// names it explicitly. A source used by several components therefore has
		// one binding each, which is also what gives each its own cache entry.
		component: string
	}

	_raw: "\(parameter.region)-\(parameter.zone)-\(parameter.department)-\(parameter.tenant)-\(parameter.component)"

	output: name: strings.ToLower(_raw)
}
