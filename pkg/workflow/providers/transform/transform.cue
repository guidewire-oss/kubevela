// transform.cue

#Reshape: {
	#provider: "transform"
	#do:       "reshape"

	$params: {
		input?: _
		query: string
		vars?: {[string]: _}
	}

	$returns?: {
		output?: _
	}
}
