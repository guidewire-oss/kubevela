package registry

// #ReadFile fetches a single file from a named addon registry.
//
// The registry is looked up in the cluster's registry ConfigMap, so a
// SourceDefinition names a registry the platform has already configured rather
// than carrying a URL and a token of its own. Whatever auth that registry was
// registered with is what the read uses.
#ReadFile: {
	#do:       "read-file"
	#provider: "registry"

	// +usage=The params of this action
	$params: {
		// +usage=Name of a registry configured in this cluster, as listed by `vela registry ls`
		registry: string
		// +usage=Path of the file within the registry, relative to the registry's own root path
		path: string
		// +usage=Branch, tag or commit to read at. Defaults to whatever the registry's URL pinned.
		ref?: string
	}

	// +usage=The file's contents
	$returns?: {
		// +usage=The file contents, verbatim
		content: string
		...
	}
	...
}
