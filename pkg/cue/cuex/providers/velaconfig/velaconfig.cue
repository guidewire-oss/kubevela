package velaconfig

// #Read reads a KubeVela Config.
//
// A Config is what `vela config create` produces and what a ConfigTemplate
// validates - the platform's own store of parameterised settings. Reading one
// through this provider rather than through the Secret it happens to live in
// means the source keeps working when Config graduates to a CRD.
//
// `outputs` lists the objects the Config's template produced, as references
// only. Those objects are frequently Secrets, so materialising them is left to
// the definition - see examples/source-library/vela-config-outputs.cue, which
// ranges over these references with kube.#Get.
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

	// +usage=The Config
	$returns?: {
		// +usage=The properties the Config was created with
		properties: {...}
		// +usage=The ConfigTemplate it was rendered from
		template: {
			name:       string
			namespace?: string
		}
		// +usage=References to the objects the template produced
		outputs: [...{
			apiVersion: string
			kind:       string
			name:       string
			namespace?: string
		}]
		...
	}
	...
}
