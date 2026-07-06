// multicluster.cue

#ListClusters: {
	#provider: "multicluster"
	#do:       "list-clusters"

	$returns?: {
		outputs: {
			clusters: [...string]
		}
	}
}

#GetClusterLabels: {
	#provider: "multicluster"
	#do:       "get-cluster-labels"

	$params: {
		cluster: string
	}
	$returns?: {
		labels: [string]: string
	}
}

#GetPlacementsFromTopologyPolicies: {
	#provider: "multicluster"
	#do:       "get-placements-from-topology-policies"

	$params: {
		policies: [...string]
	}
	$returns?: {
		placements: [...{
			cluster:   string
			namespace: string
		}]
	}
}

#Deploy: {
	#provider: "multicluster"
	#do:       "deploy"

	$params: {
		policies: [...string]
		components: *[] | [...string]
		parallelism:              int
		ignoreTerraformComponent: bool
		inlinePolicies: *[] | [...{...}]
		dispatcher?: string
	}
	$returns?: {...}
}
