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

package addon

import (
	"context"
	"strings"

	"github.com/pkg/errors"
)

// chartTagLister lists a repository's tags. It is a package variable so tests
// can substitute a fake; production always uses listOCITags.
var chartTagLister ociTagLister = listOCITags

// ociTagListerForTest swaps the package tag lister and returns a function that
// restores it. It exists for tests in this package only.
func ociTagListerForTest(fn ociTagLister) func() {
	previous := chartTagLister
	chartTagLister = fn
	return func() { chartTagLister = previous }
}

// OCIChartRef returns the full OCI reference a module chart publishes to,
// "<host>[/<prefix>]/<name>:<tag>". It is exported so a caller can print the
// target before pushing.
func OCIChartRef(reg Registry, name, tag string) (string, error) {
	if reg.OCI == nil {
		return "", errors.Errorf("registry %q is not an OCI registry", reg.Name)
	}
	repoRef, _, _ := ociRepoRef(reg.OCI.URL, name)
	return repoRef + ":" + tag, nil
}

// PushOCIChart pushes a packaged Helm chart archive to reg as name:version.
// It is the push counterpart of PullOCIChartFiles and uses the same reference
// construction and the same authenticated Helm registry client, so a module
// published here is pulled by the module fetch unchanged.
func PushOCIChart(_ context.Context, reg Registry, name, version string, archive []byte) error {
	if reg.OCI == nil {
		return errors.Errorf("registry %q is not an OCI registry", reg.Name)
	}
	repoRef, host, plainHTTP := ociRepoRef(reg.OCI.URL, name)
	client, err := newOCIClient(host, plainHTTP, reg.OCI.Username, reg.OCI.Token)
	if err != nil {
		return err
	}
	ref := repoRef + ":" + version
	if _, err := client.Push(archive, ref); err != nil {
		return errors.Wrapf(err, "failed to push chart %s", ref)
	}
	return nil
}

// OCIChartTagExists reports whether tag is already published for name in reg.
// A repository that does not exist yet is reported as "no such tag" rather
// than an error: the first publish of a module is exactly that case.
func OCIChartTagExists(ctx context.Context, reg Registry, name, tag string) (bool, error) {
	if reg.OCI == nil {
		return false, errors.Errorf("registry %q is not an OCI registry", reg.Name)
	}
	repoRef, host, plainHTTP := ociRepoRef(reg.OCI.URL, name)
	tags, err := chartTagLister(ctx, repoRef, host, plainHTTP, reg.OCI.Username, reg.OCI.Token)
	if err != nil {
		if IsOCIRepositoryNotFound(err) {
			return false, nil
		}
		return false, errors.Wrapf(err, "failed to list tags for %s", repoRef)
	}
	for _, published := range tags {
		if published == tag {
			return true, nil
		}
	}
	return false, nil
}

// IsOCIRepositoryNotFound reports whether err is a registry's "this repository
// does not exist" answer. ECR reports RepositoryNotFoundException; the
// distribution API reports NAME_UNKNOWN.
func IsOCIRepositoryNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"name_unknown", "name unknown", "repositorynotfoundexception", "repository not found"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// IsOCITagImmutable reports whether err is the registry refusing to move an
// existing tag. On ECR this is the repository's IMMUTABLE tag mutability
// setting, which no client flag can override.
func IsOCITagImmutable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "imagetagalreadyexistsexception") || strings.Contains(msg, "tag is immutable") || strings.Contains(msg, "repository is immutable")
}
