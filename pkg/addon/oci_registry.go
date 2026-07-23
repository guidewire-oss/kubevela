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
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"k8s.io/klog/v2"
)

// ociPuller pulls a Helm-chart artifact from an OCI registry and returns the raw
// chart archive bytes. It is a seam so unit tests can avoid the network.
type ociPuller func(ctx context.Context, ref, host, username, password string) ([]byte, error)

// ociTagLister lists the semver tags of an OCI repository, highest first. It is
// a seam so unit tests can avoid the network.
type ociTagLister func(ctx context.Context, repoRef, host, username, password string) ([]string, error)

// ociRegistry resolves addons stored as OCI Helm charts (e.g. in ECR/GHCR).
// It satisfies VersionedRegistry: OCI tags are the addon versions. An empty
// version resolves to the highest semver tag (there is no reliance on a
// floating "latest" tag, which `helm push` does not create). Catalog-style
// listing across all addons is not supported (the OCI API has no such index).
type ociRegistry struct {
	name     string
	url      string
	username string
	token    string
	// pullFn/tagsFn default to the production implementations; overridden in tests.
	pullFn ociPuller
	tagsFn ociTagLister
}

// BuildOCIRegistry builds an OCI addon registry reader.
func BuildOCIRegistry(name, url, username, token string) VersionedRegistry {
	return &ociRegistry{
		name:     name,
		url:      url,
		username: username,
		token:    token,
		pullFn:   pullOCIChart,
		tagsFn:   listOCITags,
	}
}

// ociRepoRef builds the OCI repository reference (no tag) and host from a
// registry URL and addon name. The URL may carry an "oci://" scheme and/or a
// trailing slash. The host is the registry authority (everything before the
// first path separator), used for login.
func ociRepoRef(url, addon string) (repoRef, host string) {
	base := strings.TrimSuffix(strings.TrimPrefix(url, "oci://"), "/")
	repoRef = fmt.Sprintf("%s/%s", base, addon)
	host = base
	if i := strings.Index(base, "/"); i >= 0 {
		host = base[:i]
	}
	return repoRef, host
}

// resolveVersion returns the tag to pull. A pinned version is used as-is; an
// empty version is resolved to the highest semver tag published in the repo.
func (i *ociRegistry) resolveVersion(ctx context.Context, repoRef, host, version string) (string, error) {
	if version != "" {
		return version, nil
	}
	list := i.tagsFn
	if list == nil {
		list = listOCITags
	}
	tags, err := list(ctx, repoRef, host, i.username, i.token)
	if err != nil {
		return "", errors.Wrapf(err, "failed to list tags for OCI addon %s", repoRef)
	}
	if len(tags) == 0 {
		return "", errors.Errorf("no semver tags found for OCI addon %s; push a versioned tag or pin an explicit version", repoRef)
	}
	// helm's Tags returns semver-filtered, highest-first.
	return tags[0], nil
}

// newOCIClient builds an authenticated Helm OCI registry client.
func newOCIClient(host, username, password string) (*registry.Client, error) {
	client, err := registry.NewClient()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create OCI registry client")
	}
	if username != "" || password != "" {
		if err := client.Login(host, registry.LoginOptBasicAuth(username, password)); err != nil {
			return nil, errors.Wrapf(err, "failed to login to OCI registry %s", host)
		}
	}
	return client, nil
}

// pullOCIChart is the production puller: it logs in (when credentials are set)
// and pulls the chart layer from the OCI registry via the Helm registry client.
func pullOCIChart(_ context.Context, ref, host, username, password string) ([]byte, error) {
	client, err := newOCIClient(host, username, password)
	if err != nil {
		return nil, err
	}
	result, err := client.Pull(ref, registry.PullOptWithChart(true))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to pull addon chart %s", ref)
	}
	if result == nil || result.Chart == nil || len(result.Chart.Data) == 0 {
		return nil, errors.Errorf("addon chart %s has no chart layer", ref)
	}
	return result.Chart.Data, nil
}

// listOCITags lists the repository's semver tags (highest first) via the Helm
// registry client, which filters non-semver tags and sorts descending.
func listOCITags(_ context.Context, repoRef, host, username, password string) ([]string, error) {
	client, err := newOCIClient(host, username, password)
	if err != nil {
		return nil, err
	}
	return client.Tags(repoRef)
}

// loadAddon pulls the addon's OCI chart and turns it into a WholeAddonPackage,
// reusing the shared archive -> InstallPackage pipeline.
func (i *ociRegistry) loadAddon(ctx context.Context, name, version string) (*WholeAddonPackage, error) {
	repoRef, host := ociRepoRef(i.url, name)
	resolved, err := i.resolveVersion(ctx, repoRef, host, version)
	if err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("%s:%s", repoRef, resolved)
	pull := i.pullFn
	if pull == nil {
		pull = pullOCIChart
	}
	archive, err := pull(ctx, ref, host, i.username, i.token)
	if err != nil {
		return nil, err
	}
	files, err := loader.LoadArchiveFiles(bytes.NewReader(archive))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load addon chart archive %s", ref)
	}
	pkg, err := loadAddonPackage(name, files)
	if err != nil {
		return nil, err
	}
	pkg.RegistryName = i.name
	klog.V(5).Infof("Addon '%s' loaded from OCI registry '%s' (%s)", name, i.name, ref)
	return pkg, nil
}

// GetAddonInstallPackage returns the addon's install package from the OCI registry.
func (i *ociRegistry) GetAddonInstallPackage(ctx context.Context, addonName, version string) (*InstallPackage, error) {
	pkg, err := i.loadAddon(ctx, addonName, version)
	if err != nil {
		return nil, err
	}
	return &pkg.InstallPackage, nil
}

// GetDetailedAddon returns the whole addon package from the OCI registry.
func (i *ociRegistry) GetDetailedAddon(ctx context.Context, addonName, version string) (*WholeAddonPackage, error) {
	return i.loadAddon(ctx, addonName, version)
}

// GetAddonUIData returns the addon's UI data from the OCI registry.
func (i *ociRegistry) GetAddonUIData(ctx context.Context, addonName, version string) (*UIData, error) {
	pkg, err := i.loadAddon(ctx, addonName, version)
	if err != nil {
		return nil, err
	}
	return &UIData{
		Meta:      pkg.Meta,
		APISchema: pkg.APISchema,
		Detail:    pkg.Detail,
	}, nil
}

// ListAddon is not supported for OCI registries: the OCI distribution API does
// not expose an addon catalog, so addons must be referenced by name. Callers get
// an explicit error rather than a silently empty list.
func (i *ociRegistry) ListAddon() ([]*UIData, error) {
	return nil, errors.Errorf("listing addons is not supported for OCI registry %q; reference the addon by name", i.name)
}

// GetAddonAvailableVersion is not supported for OCI registries (no tag enumeration).
func (i *ociRegistry) GetAddonAvailableVersion(_ string) ([]*repo.ChartVersion, error) {
	return nil, errors.Errorf("version listing is not supported for OCI registry %q", i.name)
}
