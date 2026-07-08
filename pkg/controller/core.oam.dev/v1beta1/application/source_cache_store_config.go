package application

import (
	"context"
	"time"

	apitypes "github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func sourceTemplateRefsByType(af *appfile.Appfile) map[string]string {
	out := map[string]string{}
	if af == nil {
		return out
	}
	for sourceType, def := range af.RelatedSourceDefinitions {
		if def == nil || def.Status.ConfigTemplateRef == nil || def.Status.ConfigTemplateRef.Name == "" {
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
