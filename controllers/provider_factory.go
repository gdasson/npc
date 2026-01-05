package controllers

import (
    "context"
    "fmt"
    "sync"

    "github.com/aws/aws-sdk-go-v2/config"

    v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
    "github.com/myorg/nodepatcher/pkg/provider"
    awsprov "github.com/myorg/nodepatcher/pkg/provider/aws"
)

type ProviderFactory struct {
    mu    sync.Mutex
    cache map[string]provider.Provider // key: type|region
}

func NewProviderFactory() *ProviderFactory {
    return &ProviderFactory{cache: map[string]provider.Provider{}}
}

func (f *ProviderFactory) Get(ctx context.Context, pref v1alpha1.ProviderRef) (provider.Provider, error) {
    switch pref.Type {
    case "aws":
        if pref.AWS == nil || pref.AWS.Region == "" {
            return nil, fmt.Errorf("provider aws requires spec.provider.aws.region")
        }
        key := "aws|" + pref.AWS.Region
        f.mu.Lock()
        if p, ok := f.cache[key]; ok {
            f.mu.Unlock()
            return p, nil
        }
        f.mu.Unlock()

        cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(pref.AWS.Region))
        if err != nil {
            return nil, err
        }
        p := awsprov.New(cfg, pref.AWS.Region)

        f.mu.Lock()
        f.cache[key] = p
        f.mu.Unlock()
        return p, nil
    default:
        return nil, fmt.Errorf("unsupported provider type %q", pref.Type)
    }
}
