"ocm-topology": {
	annotations: {
		"policy.oam.dev/no-outputs": "true"
	}
	description: "Configure OCM hub and ManifestWork naming for dispatcher-based deployments."
	labels: {}
	attributes: {}
	type: "policy"
}

template: {
	parameter: {
		// +usage=Name of the hub cluster in KubeVela multicluster registry.
		hubCluster: string
		// +usage=Namespace where ManifestWorks are created on the hub cluster.
		workNamespace: string
		// +usage=Prefix used when generating ManifestWork names.
		namePrefix?: string
	}
}
