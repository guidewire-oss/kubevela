package module

#Render: {
	#do:       "render"
	#provider: "module"

	$params: {
		module:   string
		registry: *"" | string
	}
	$returns?: {
		application: {...}
		...
	}
	...
}
