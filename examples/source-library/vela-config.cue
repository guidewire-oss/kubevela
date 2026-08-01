import "vela/velaconfig"

// A Config's properties.
//
// `properties: _` because a Config's shape is set by whichever ConfigTemplate
// created it, which this source has no way to know. A platform wanting a typed
// contract over a particular Config should write a definition that names its
// fields - the read is one line, as below.
schema: {
	properties: _
}

storage: {
	// Configs change when an operator changes them, which is rare, and the read
	// is a single in-cluster GET.
	storageTTL:     "5m"
	onStaleFailure: "use-stale"
}

parameter: {
	// +usage=Name of the Config
	name: string
	// +usage=Namespace it lives in. Defaults to vela-system.
	namespace?: string
}

_cfg: velaconfig.#Read & {
	$params: {
		name: parameter.name
		if parameter.namespace != _|_ {
			namespace: parameter.namespace
		}
	}
}

output: properties: _cfg.$returns.properties
