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
	"sort"

	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/repo"
	"k8s.io/klog/v2"

	"github.com/oam-dev/kubevela/pkg/utils"
	"github.com/oam-dev/kubevela/pkg/utils/common"
	"github.com/oam-dev/kubevela/pkg/utils/helm"
)

// httpHelmBackend reads addons from an indexed Helm repository over HTTP. The
// repository's index.yaml supplies discovery, the version list, and the
// per-version metadata, so this backend needs no catalog of its own.
type httpHelmBackend struct {
	url  string
	name string
	h    *helm.Helper
	// opts carries the basic-auth and TLS options for repositories that need them
	opts *common.HTTPOption
}

// ListUIData lists every addon in the repository from one index fetch.
func (b *httpHelmBackend) listUIData(_ context.Context) ([]*UIData, error) {
	chartIndex, err := b.h.GetIndexInfo(b.url, false, b.opts)
	if err != nil {
		return nil, err
	}
	return b.resolveAddonListFromIndex(chartIndex), nil
}

// Versions returns every published version of the addon, newest first.
func (b *httpHelmBackend) versions(_ context.Context, addonName string) ([]*repo.ChartVersion, error) {
	return b.loadAddonVersions(addonName)
}

// supportsVersionRequirements reports true: index entries carry the annotations
// that SystemRequirements are read from, so this backend can answer which
// version meets a requirement.
func (b *httpHelmBackend) supportsVersionRequirements() bool { return true }

// Resolve selects a version from the index and downloads its chart. The index
// may list several URLs for one version, and a URL that cannot be downloaded or
// cannot be decoded is a reason to try the next one rather than to fail.
func (b *httpHelmBackend) resolve(ctx context.Context, addonName, version string) (*resolvedChart, error) {
	versions, err := b.loadAddonVersions(addonName)
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return nil, err
		}
		return nil, errors.Wrapf(ErrFetch, "registry %s: %v", b.name, err)
	}
	addonVersion, availableVersions := chooseVersion(version, versions)
	if addonVersion == nil {
		return nil, errors.Errorf("specified version %s for addon %s not exist", utils.Sanitize(version), addonName)
	}
	for _, chartURL := range addonVersion.URLs {
		if !utils.IsValidURL(chartURL) {
			chartURL, err = utils.JoinURL(b.url, chartURL)
			if err != nil {
				return nil, fmt.Errorf("cannot join repository URL %s and chart URL %s, %w", b.url, chartURL, err)
			}
		}
		archive, err := common.HTTPGetWithOption(ctx, chartURL, b.opts)
		if err != nil {
			klog.Warningf("failed to download the addon package %s:%s", chartURL, err.Error())
			continue
		}
		bufferedFile, err := loader.LoadArchiveFiles(bytes.NewReader(archive))
		if err != nil {
			klog.Warningf("failed to load the addon package:%s", err.Error())
			continue
		}
		return &resolvedChart{
			files:             bufferedFile,
			version:           addonVersion.Version,
			availableVersions: availableVersions,
			// The index is authoritative for requirements, including when it
			// declares none: an entry without the annotations must clear whatever
			// the packaged metadata.yaml claimed.
			requirements:    LoadSystemRequirements(addonVersion.Annotations),
			requirementsSet: true,
		}, nil
	}
	return nil, ErrFetch
}

func (b *httpHelmBackend) resolveAddonListFromIndex(index *repo.IndexFile) []*UIData {
	var res []*UIData
	for addonName, versions := range index.Entries {
		if len(versions) == 0 {
			continue
		}
		sort.Sort(sort.Reverse(versions))
		latestVersion := versions[0]
		var availableVersions []string
		for _, version := range versions {
			availableVersions = append(availableVersions, version.Version)
		}
		o := UIData{Meta: Meta{
			Name:        addonName,
			Icon:        latestVersion.Icon,
			Tags:        latestVersion.Keywords,
			Description: latestVersion.Description,
			Version:     latestVersion.Version,
		}, AvailableVersions: availableVersions}
		res = append(res, &o)
	}
	return res
}

// loadAddonVersions loads all available versions of the addon, newest first.
func (b *httpHelmBackend) loadAddonVersions(addonName string) ([]*repo.ChartVersion, error) {
	versions, err := b.h.ListVersions(b.url, addonName, false, b.opts)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, ErrNotExist
	}
	sort.Sort(sort.Reverse(versions))
	return versions, nil
}
