// dispatch.cue

#PrepareDispatch: {
	#provider: "dispatch"
	#do:       "prepare-dispatch"

	$params: {
		type: *"cluster-gateway" | string
		component: {
			name: string
		}
		target: {
			cluster:   string
			namespace: *"" | string
		}
		resources: {
			output?:  {...}
			outputs?: {...}
		}
	}

	$returns?: {
		output?:  {...}
		outputs?: {...}
	}
}

// Backward-compat alias; prefer #PrepareDispatch.
#Transform: #PrepareDispatch & {
	#do: "transform"
}

#GetPolicyByName: {
	#provider: "dispatch"
	#do:       "get-policy-by-name"
	$params: {
		name: string
	}
	$returns?: {
		policy?: {...}
	}
}

#GetPoliciesByType: {
	#provider: "dispatch"
	#do:       "get-policies-by-type"
	$params: {
		type: string
	}
	$returns?: {
		policies?: [...{...}]
	}
}
