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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/repo"
	"k8s.io/klog/v2"

	"github.com/oam-dev/kubevela/pkg/registry/component"
)

// ociPuller pulls a Helm-chart artifact from an OCI registry and returns the raw
// chart archive bytes. It is a seam so unit tests can avoid the network.
type ociPuller func(ctx context.Context, ref, host, username, password string) ([]byte, error)

// ociTagLister lists the semver tags of an OCI repository, highest first. It is
// a seam so unit tests can avoid the network.
type ociTagLister func(ctx context.Context, repoRef, host, username, password string) ([]string, error)

// ociCatalogLister lists addon repository names below an OCI registry prefix.
// It is a seam so unit tests can avoid the network.
type ociCatalogLister func(ctx context.Context, registryURL, username, password string) ([]string, error)

// ociHelmBackend resolves addons stored as OCI Helm charts (e.g. in ECR/GHCR).
// OCI tags are the addon versions. An empty version resolves to the highest
// semver tag; there is no reliance on a floating "latest" tag, which `helm push`
// does not create.
//
// An OCI registry has no index.yaml, so cross-addon discovery needs a catalog of
// its own and version enumeration is a tag listing. That is what this backend
// supplies; everything downstream of the chart archive is shared.
type ociHelmBackend struct {
	name     string
	url      string
	username string
	token    string
	// pullFn/tagsFn/catalogFn/catalogIndexFn default to the production
	// implementations;
	// overridden in tests.
	pullFn         ociPuller
	tagsFn         ociTagLister
	catalogFn      ociCatalogLister
	catalogIndexFn ociCatalogIndexLister
}

// resolveVersion returns the tag to pull. A pinned version is used as-is; an
// empty version is resolved to the highest semver tag published in the repo.
// resolveVersion picks the tag to pull and also reports the tags it saw getting
// there, so callers can fill in AvailableVersions without a second round trip.
// A pinned version needs no listing and returns no tag list.
func (b *ociHelmBackend) resolveVersion(ctx context.Context, repoRef, host, version string) (string, []string, error) {
	if version != "" {
		return version, nil, nil
	}
	list := b.tagsFn
	if list == nil {
		list = component.ListOCITags
	}
	tags, err := list(ctx, repoRef, host, b.username, b.token)
	if err != nil {
		return "", nil, errors.Wrapf(err, "failed to list tags for OCI addon %s", repoRef)
	}
	if len(tags) == 0 {
		return "", nil, errors.Wrapf(ErrNotExist, "no semver tags found for OCI addon %s; push a versioned tag or pin an explicit version", repoRef)
	}
	// helm's Tags returns semver-filtered, highest-first.
	return tags[0], tags, nil
}

// classify translates a load failure into the shared error vocabulary.
// installDependency uses isSkippableRegistryError to decide whether to try the
// next registry; without this, any OCI error (a missing tag, an auth failure, an
// unreachable host) would abort dependency resolution instead of falling
// through. The HTTP backend deliberately does not do this, which is why it is an
// optional capability rather than part of chartBackend.
func (b *ociHelmBackend) classify(err error) error {
	if err == nil || errors.Is(err, ErrNotExist) || errors.Is(err, ErrFetch) {
		return err
	}
	return errors.Wrapf(ErrFetch, "OCI registry %s: %v", b.name, err)
}

// supportsVersionRequirements reports false. Versions here are synthesized from
// repository tags and carry no annotations, so LoadSystemRequirements would read
// nil for every one of them and report the newest tag as meeting any
// requirement. Answering "no opinion" is the honest result.
func (b *ociHelmBackend) supportsVersionRequirements() bool { return false }

// Resolve pulls the addon's OCI chart and decodes the archive.
func (b *ociHelmBackend) resolve(ctx context.Context, addonName, version string) (*resolvedChart, error) {
	repoRef, host := component.OCIRepoRef(b.url, addonName)
	resolved, available, err := b.resolveVersion(ctx, repoRef, host, version)
	if err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("%s:%s", repoRef, resolved)
	pull := b.pullFn
	if pull == nil {
		pull = component.PullOCIChart
	}
	archive, err := pull(ctx, ref, host, b.username, b.token)
	if err != nil {
		return nil, err
	}
	files, err := loader.LoadArchiveFiles(bytes.NewReader(archive))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load addon chart archive %s", ref)
	}
	klog.V(5).Infof("Addon '%s' loaded from OCI registry '%s' (%s)", addonName, b.name, ref)
	return &resolvedChart{
		files:   files,
		version: resolved,
		// A pinned request lists no tags and so carries no list: the caller asked
		// about one version.
		availableVersions: available,
		// The chart's own metadata.yaml is the only source of requirements here.
		// Unlike an index entry, a tag carries no annotations to override it with.
		requirementsSet: false,
	}, nil
}

// Versions lists the semver tags of an OCI addon, highest first.
func (b *ociHelmBackend) versions(ctx context.Context, addonName string) ([]*repo.ChartVersion, error) {
	repoRef, host := component.OCIRepoRef(b.url, addonName)
	list := b.tagsFn
	if list == nil {
		list = component.ListOCITags
	}
	tags, err := list(ctx, repoRef, host, b.username, b.token)
	if err != nil {
		return nil, err
	}
	versions := make([]*repo.ChartVersion, 0, len(tags))
	for _, tag := range tags {
		versions = append(versions, &repo.ChartVersion{
			Metadata: &chart.Metadata{Name: addonName, Version: tag},
		})
	}
	return versions, nil
}

// loadUIData builds one listing entry from a real chart, for the discovery path
// that has only repository names to work with.
func (b *ociHelmBackend) loadUIData(ctx context.Context, addonName, version string) (data *UIData, err error) {
	defer func() {
		if err != nil {
			err = b.classify(err)
		}
	}()
	resolved, err := b.resolve(ctx, addonName, version)
	if err != nil {
		return nil, err
	}
	pkg, err := loadAddonPackage(addonName, resolved.files)
	if err != nil {
		return nil, err
	}
	pkg.AvailableVersions = resolved.availableVersions
	return uiDataFromPackage(pkg), nil
}

// ListUIData enumerates repositories below the configured OCI prefix and loads
// the latest semver-tagged addon metadata for each repository.
func (b *ociHelmBackend) listUIData(ctx context.Context) ([]*UIData, error) {
	var indexErr error
	if b.catalogIndexFn != nil {
		addons, err := b.catalogIndexFn(ctx, b.url, b.username, b.token)
		if err == nil {
			return addons, nil
		}
		indexErr = err
		klog.V(4).Infof("Portable OCI addon catalog is unavailable for registry %q, falling back to registry catalog discovery: %v", b.name, err)
	}

	list := b.catalogFn
	if list == nil {
		list = listOCIRepositories
	}
	names, err := list(ctx, b.url, b.username, b.token)
	if err != nil {
		if indexErr != nil {
			// Only a genuine absence on BOTH sides means there is no catalog. If
			// either failure was a read error, callers must not treat the result as
			// an empty catalog.
			if errors.Is(indexErr, ErrOCICatalogAbsent) && errors.Is(err, ErrOCICatalogAbsent) {
				return nil, errors.Wrapf(ErrOCICatalogAbsent, "no OCI addon catalog at portable location (%v) or registry catalog (%v)", indexErr, err)
			}
			return nil, errors.Errorf("failed to list OCI addons from portable catalog (%v) and registry catalog (%v)", indexErr, err)
		}
		return nil, err
	}

	var addons []*UIData
	for _, name := range names {
		repoRef, host := component.OCIRepoRef(b.url, name)
		tags := b.tagsFn
		if tags == nil {
			tags = component.ListOCITags
		}
		versions, err := tags(ctx, repoRef, host, b.username, b.token)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to list versions for OCI addon %s", name)
		}
		if len(versions) == 0 {
			continue
		}
		addon, err := b.loadUIData(ctx, name, versions[0])
		if err != nil {
			return nil, errors.Wrapf(err, "failed to load metadata for OCI addon %s", name)
		}
		addon.AvailableVersions = versions
		addons = append(addons, addon)
	}
	return addons, nil
}

// classifyCatalogListStatus turns a /v2/_catalog response status into the
// enumeration result. Split out from listOCIRepositoriesWithScheme's HTTP
// loop so the Docker-Hub-specific 401 handling is unit-testable without a
// real HTTP round trip.
//
// Docker Hub's registry front door never grants the registry:catalog:* scope
// to anyone, including the account owner, so it deterministically answers
// /v2/_catalog with 401 regardless of credentials. For every other registry a
// 401 here usually means a real permission problem worth surfacing, so the
// relaxation is scoped to Docker Hub's known hosts rather than applied
// globally. This is safe even for a Docker Hub user with wrong credentials:
// listOCIRepositoriesWithScheme is only ever called alongside a portable
// catalog read that performs its own real login, and a genuine auth failure
// there still surfaces as a hard error (see ociHelmBackend.listUIData).
//
// It stays in pkg/addon rather than moving to pkg/registry/component with the
// other OCI primitives because it speaks addon's catalog protocol: the
// ErrOCICatalogAbsent sentinel it returns is what the addon push path reads to
// decide it may bootstrap a portable catalog.
func classifyCatalogListStatus(host string, status int, statusText string) error {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return errors.Wrapf(ErrOCICatalogAbsent, "OCI catalog enumeration is unsupported at %s: server returned %s", host, statusText)
	case http.StatusUnauthorized:
		if component.IsDockerHubHost(host) {
			return errors.Wrapf(ErrOCICatalogAbsent, "OCI catalog enumeration is unsupported at %s: Docker Hub does not grant catalog listing to any credential: server returned %s", host, statusText)
		}
	}
	return errors.Errorf("failed to list OCI catalog at %s: server returned %s", host, statusText)
}

// listOCIRepositories enumerates the OCI distribution catalog and returns
// repository names relative to the configured registry prefix. The catalog API
// is paginated through RFC 5988 Link headers.
func listOCIRepositories(ctx context.Context, registryURL, username, password string) ([]string, error) {
	return listOCIRepositoriesWithScheme(ctx, registryURL, username, password, "https")
}

func listOCIRepositoriesWithPlainHTTP(ctx context.Context, registryURL, username, password string) ([]string, error) {
	return listOCIRepositoriesWithScheme(ctx, registryURL, username, password, "http")
}

func listOCIRepositoriesWithScheme(ctx context.Context, registryURL, username, password, scheme string) ([]string, error) {
	host, prefix := component.OCIRegistryLocation(registryURL)
	next := &url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     "/v2/_catalog",
		RawQuery: "n=1000",
	}
	seen := map[string]bool{}
	var addons []string

	for next != nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next.String(), nil)
		if err != nil {
			return nil, errors.Wrap(err, "failed to build OCI catalog request")
		}
		if username != "" || password != "" {
			req.SetBasicAuth(username, password)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to list OCI catalog at %s", host)
		}

		var page struct {
			Repositories []string `json:"repositories"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		closeErr := resp.Body.Close()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			// Not every registry implements /v2/_catalog. Treat a refusal that
			// names the route as unsupported (or, for Docker Hub specifically, its
			// deterministic 401) as "no catalog to enumerate" rather than a read
			// failure, so a push can still bootstrap a portable catalog there.
			// Anything else stays a read failure, because rebuilding the catalog
			// from a misread empty list would drop every entry already published.
			return nil, classifyCatalogListStatus(host, resp.StatusCode, resp.Status)
		}
		if decodeErr != nil {
			return nil, errors.Wrap(decodeErr, "failed to decode OCI catalog response")
		}
		if closeErr != nil {
			return nil, errors.Wrap(closeErr, "failed to close OCI catalog response")
		}

		for _, repository := range page.Repositories {
			addonName := strings.Trim(repository, "/")
			if prefix != "" {
				prefixWithSlash := prefix + "/"
				if !strings.HasPrefix(addonName, prefixWithSlash) {
					continue
				}
				addonName = strings.TrimPrefix(addonName, prefixWithSlash)
			}
			if addonName != "" && addonName != ociCatalogChartName && !seen[addonName] {
				seen[addonName] = true
				addons = append(addons, addonName)
			}
		}

		next = nil
		link := resp.Header.Get("Link")
		if start, end := strings.Index(link, "<"), strings.Index(link, ">"); start >= 0 && end > start && strings.Contains(link[end:], `rel="next"`) {
			candidate, parseErr := req.URL.Parse(link[start+1 : end])
			if parseErr != nil {
				return nil, errors.Wrap(parseErr, "failed to parse OCI catalog pagination link")
			}
			// The Link header is registry-supplied and url.Parse resolves an
			// absolute URL by replacing scheme and host outright. Every request in
			// this loop attaches the registry's BasicAuth credentials, so following
			// such a link would hand them to a host we were never configured to
			// talk to. Accept only links that stay on the original scheme and host.
			if candidate.Scheme != scheme || candidate.Host != host {
				return nil, errors.Errorf("refusing OCI catalog pagination link %q: expected an %s link on host %s", candidate.Redacted(), scheme, host)
			}
			next = candidate
		}
	}

	sort.Strings(addons)
	return addons, nil
}
