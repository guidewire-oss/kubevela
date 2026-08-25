import (
	"encoding/json"
	"encoding/yaml"
	"strings"
	"vela/registry"
)

"git-file": {
	type: "source"
	annotations: {}
	labels: {}
	description: "Reads a file from a registry configured in this cluster, optionally at a given branch or tag. YAML and JSON are parsed; anything else comes back as a string."
}

template: {
	// A file from a registry, parsed if we can recognise it.
	//
	// `content: _` rather than a declared shape: this source is generic over
	// whatever file it is pointed at, so there is nothing honest to declare about
	// the value. The field itself is still declared, which is what keeps `schema:`
	// a struct and keeps the surface - a consumer reads `content`, and nothing else
	// exists to read.
	//
	// This is a deliberate trade. A source that names its fields gets output
	// validation and typed expressions; this one gives those up in exchange for
	// working on any file. A platform that cares about a particular file should
	// write a definition that parses it and declares its fields - the parse is one
	// line, as below.
	schema: {
		content: _
		found:   bool
	}

	storage: {
		// A file in a repository changes rarely, and fetching one is a round trip
		// against someone else's service.
		storageTTL:     "30m"
		onStaleFailure: "use-stale"
	}

	parameter: {
		// +usage=Name of a registry configured in this cluster
		registry: string
		// +usage=Path of the file within that registry
		path: string
		// +usage=Branch, tag or commit to read at. Defaults to whatever the registry pinned.
		ref?: string
		// +usage=Fail resolution when the file is absent. Set false to read an optional file, which then resolves with found: false and content: null.
		required: *true | bool
	}

	_file: registry.#ReadFile & {
		$params: {
			registry: parameter.registry
			path:     parameter.path
			// Omitted rather than sent empty, so the provider can tell "use the
			// registry's default" from "read at the empty ref".
			if parameter.ref != _|_ {
				ref: parameter.ref
			}
		}
	}

	// A required file that is not there fails here rather than resolving to an
	// empty value. The provider reports absence as found: false instead of an
	// error, which is what an optional file wants; a required one wants to be told,
	// because an empty parse looks exactly like legitimately empty data once it has
	// been cached.
	if parameter.required {
		// The reason is carried inside the conflict on purpose. CUE has no custom
		// error construct, so unifying an explanatory string against true is how a
		// user gets told which file was missing from which registry, rather than
		// "_mustExist: conflicting values false and true".
		_mustExist: true & [
			if _file.$returns.found {true},
			"required file \"\(parameter.path)\" is not in registry \"\(parameter.registry)\"; set required: false to read it as an optional file",
		][0]
	}

	// Structured formats are parsed so a consumer can read a field out of them;
	// anything else comes back as the bytes it is. Detection is by suffix, which is
	// the only signal available - the registry readers return content, not a media
	// type.
	_isYAML: strings.HasSuffix(parameter.path, ".yaml") || strings.HasSuffix(parameter.path, ".yml")
	_isJSON: strings.HasSuffix(parameter.path, ".json")

	output: {
		found: _file.$returns.found
		content: [
			// Nothing is parsed when there was no file. null is the honest answer,
			// and it is distinguishable from an empty file, which parses to "".
			if !_file.$returns.found {null},
			if _isYAML {yaml.Unmarshal(_file.$returns.content)},
			if _isJSON {json.Unmarshal(_file.$returns.content)},
			_file.$returns.content,
		][0]
	}
}
