package application

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	apitypes "github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/config"
	"github.com/oam-dev/kubevela/pkg/oam"
)

const (
	sourceCacheNamespace = "vela-system"
	sourceCacheSyncAtKey = "config.oam.dev/last-sync-at"
)

type configAPISourceCacheStore struct {
	factory          config.Factory
	templateBySource map[string]string
}

func newConfigAPISourceCacheStore(cli client.Client, templateBySource map[string]string) *configAPISourceCacheStore {
	if templateBySource == nil {
		templateBySource = map[string]string{}
	}
	return &configAPISourceCacheStore{
		factory:          config.NewConfigFactory(cli),
		templateBySource: templateBySource,
	}
}

// sourceTemplateRefsByType maps each referenced SourceDefinition type to its
// versioned ConfigTemplate name. The appfile's RelatedSourceDefinitions have
// their Status wiped during parsing (consistent with Component/TraitDefinition,
// so volatile status does not leak into the ApplicationRevision snapshot), so
// ConfigTemplateRef is read from the LIVE SourceDefinition via the client. This
// name is used to label cache Config entries with config.oam.dev/type, linking
// them back to the ConfigTemplate version. A missing/not-yet-populated ref is
// skipped; the cache store then falls back to the raw source type label.
func sourceTemplateRefsByType(ctx context.Context, cli client.Client, af *appfile.Appfile) map[string]string {
	out := map[string]string{}
	if af == nil || cli == nil {
		return out
	}
	for sourceType := range af.RelatedSourceDefinitions {
		def := &v1beta1.SourceDefinition{}
		if err := cli.Get(ctx, client.ObjectKey{Namespace: af.Namespace, Name: sourceType}, def); err != nil {
			if !apierrors.IsNotFound(err) {
				continue
			}
			if err := cli.Get(ctx, client.ObjectKey{Namespace: oam.SystemDefinitionNamespace, Name: sourceType}, def); err != nil {
				continue
			}
		}
		if def.Status.ConfigTemplateRef == nil || def.Status.ConfigTemplateRef.Name == "" {
			continue
		}
		out[sourceType] = def.Status.ConfigTemplateRef.Name
	}
	return out
}

func (s *configAPISourceCacheStore) Read(ctx context.Context, cacheKey string, ttl time.Duration) (map[string]interface{}, bool, bool, time.Time, error) {
	cfg, err := s.factory.GetConfig(ctx, sourceCacheNamespace, cacheKey, false)
	if err != nil {
		if err == config.ErrConfigNotFound {
			return nil, false, false, time.Time{}, nil
		}
		return nil, false, false, time.Time{}, err
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	lastSyncAt := cfg.CreateTime
	if cfg.Secret != nil && cfg.Secret.Annotations != nil {
		if raw, ok := cfg.Secret.Annotations[sourceCacheSyncAtKey]; ok && raw != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, raw); parseErr == nil {
				lastSyncAt = parsed
			}
		}
	}
	expiresAt := lastSyncAt.Add(ttl)
	return cfg.Properties, true, time.Now().After(expiresAt), expiresAt, nil
}

func (s *configAPISourceCacheStore) Write(ctx context.Context, cacheKey, sourceType string, data map[string]interface{}) error {
	templateName := s.templateBySource[sourceType]
	template := config.NamespacedName{}
	if templateName != "" {
		template = config.NamespacedName{
			Name:      templateName,
			Namespace: sourceCacheNamespace,
		}
	}
	cfg, err := s.factory.ParseConfig(ctx, template, config.Metadata{
		NamespacedName: config.NamespacedName{
			Name:      cacheKey,
			Namespace: sourceCacheNamespace,
		},
		Properties: data,
	})
	if err != nil {
		return err
	}
	if cfg.Secret.Annotations == nil {
		cfg.Secret.Annotations = map[string]string{}
	}
	cfg.Secret.Annotations[sourceCacheSyncAtKey] = time.Now().UTC().Format(time.RFC3339)
	if cfg.Secret.Labels == nil {
		cfg.Secret.Labels = map[string]string{}
	}
	cfg.Secret.Labels[apitypes.LabelConfigCatalog] = apitypes.VelaCoreConfig
	if templateName == "" {
		cfg.Secret.Labels[apitypes.LabelConfigType] = sourceType
	}
	return s.factory.CreateOrUpdateConfig(ctx, cfg, sourceCacheNamespace)
}
