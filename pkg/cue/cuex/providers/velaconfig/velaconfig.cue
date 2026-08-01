package velaconfig

// #Read reads the properties of a KubeVela Config.
//
// A Config is what `vela config create` produces and what a ConfigTemplate
// validates - the platform's own store of parameterised settings. Reading one
// through this provider rather than through the Secret it happens to live in
// means the source keeps working when Config graduates to a CRD.
#Read: {
	#do:       "read"
	#provider: "velaconfig"

	// +usage=The params of this action
	$params: {
		// +usage=Name of the Config
		name: string
		// +usage=Namespace it lives in. Defaults to vela-system.
		namespace?: string
	}

	// +usage=The Config's properties
	$returns?: {
		properties: {...}
		...
	}
	...
}
