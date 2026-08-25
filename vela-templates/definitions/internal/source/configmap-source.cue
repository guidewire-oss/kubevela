import "vela/kube"

"configmap": {
	type: "source"
	annotations: {}
	labels: {}
	description: "Reads a ConfigMap's data, optionally from another cluster. Values are strings, as Kubernetes stores them."
}

template: {
	// A ConfigMap's data, as it actually is: string to string.
	//
	// Kubernetes stores every value in `data` as a string - a ConfigMap holding
	// `replicas: "3"` gives you "3", not 3. Declaring that honestly is better than a
	// schema that promises ints and fails at render, and a consumer wanting a number
	// converts at the point of use.
	//
	// The keys are not known here, so `data` is an open map. That means a read of one
	// carries the same obligation as any value that might be missing: supply a
	// default, or admission refuses it where the target is required.
	schema: {
		data: [string]: string
	}

	storage: {
		// A ConfigMap read is cheap and local, but re-reading it on every reconcile
		// of every component is still waste.
		storageTTL:     "5m"
		onStaleFailure: "use-stale"
	}

	parameter: {
		// +usage=Name of the ConfigMap
		name: string
		// +usage=Namespace it lives in. Defaults to the Application's own namespace.
		namespace?: string
		// +usage=Cluster to read from. Defaults to the cluster this render targets.
		cluster?: string
	}

	// Read from the cluster the Application is being rendered for, so a multi-cluster
	// deployment picks up each cluster's own copy - which is also why the generated
	// key is per-cluster.
	_cm: kube.#Get & {
		$params: {
			// A named cluster reads someone else's copy - a control-plane ConfigMap
			// consumed by workloads on a spoke, say. Note that the generated key's
			// readable prefix still names the *rendering* cluster, since only context
			// is inlined there; the parameter is in the hash, so entries stay
			// distinct, but an operator grepping should read the prefix as "who
			// asked", not "where it came from".
			if parameter.cluster != _|_ {
				cluster: parameter.cluster
			}
			if parameter.cluster == _|_ {
				cluster: context.cluster
			}
			resource: {
				apiVersion: "v1"
				kind:       "ConfigMap"
				metadata: {
					name: parameter.name
					if parameter.namespace != _|_ {
						namespace: parameter.namespace
					}
					if parameter.namespace == _|_ {
						namespace: context.namespace
					}
				}
			}
		}
	}

	output: data: _cm.$returns.data
}
