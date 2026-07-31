/*
Copyright 2026 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package core_oam_dev

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A validating webhook path is written in three places: the Go registration, the
// Helm chart that installs the webhook configuration, and the local-debug script
// that points the configuration at a developer's machine. A handler missing from
// either of the latter two is silently never invoked - the resource is admitted
// without validation, and a test proving the handler's logic still passes.
//
// That is not hypothetical: the SourceDefinition webhook was registered in Go and
// present in the chart while absent from the debug script, so local runs exercised
// none of its checks.
func TestWebhookPathsAreRegisteredEverywhere(t *testing.T) {
	const (
		goSources  = "v1beta1"
		chartFile  = "../../../charts/vela-core/templates/admission-webhooks/validatingWebhookConfiguration.yaml"
		debugFile  = "../../../hack/debug-webhook-setup.sh"
		pathRegexp = `/validating-core-oam-dev-v1beta1-[a-z]+`
	)

	re := regexp.MustCompile(pathRegexp)

	pathsIn := func(paths []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}

	collectFile := func(path string) []string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		return pathsIn(re.FindAllString(string(raw), -1))
	}

	// The Go registrations are spread across the per-resource handler packages.
	var registered []string
	err := filepath.Walk(goSources, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		registered = append(registered, re.FindAllString(string(raw), -1)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", goSources, err)
	}
	registered = pathsIn(registered)

	if len(registered) == 0 {
		t.Fatal("found no registered webhook paths — the scan is broken, not the code")
	}

	for _, target := range []struct {
		name  string
		paths []string
	}{
		{"helm chart (" + chartFile + ")", collectFile(chartFile)},
		{"debug script (" + debugFile + ")", collectFile(debugFile)},
	} {
		missing := difference(registered, target.paths)
		if len(missing) > 0 {
			t.Errorf("%s is missing webhook paths that are registered in Go: %v\n"+
				"A handler absent here is never invoked, so the resource is admitted unvalidated.", target.name, missing)
		}
		if extra := difference(target.paths, registered); len(extra) > 0 {
			t.Errorf("%s declares webhook paths with no Go handler: %v\n"+
				"Requests to these paths will fail rather than be validated.", target.name, extra)
		}
	}
}

// difference returns the elements of a that are absent from b.
func difference(a, b []string) []string {
	inB := map[string]bool{}
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if !inB[s] {
			out = append(out, s)
		}
	}
	return out
}
