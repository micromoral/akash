// Code generated by informer-gen. DO NOT EDIT.

package v1

import (
	"context"
	time "time"

	akashnetworkv1 "github.com/ovrclk/akash/pkg/apis/akash.network/v1"
	versioned "github.com/ovrclk/akash/pkg/client/clientset/versioned"
	internalinterfaces "github.com/ovrclk/akash/pkg/client/informers/externalversions/internalinterfaces"
	v1 "github.com/ovrclk/akash/pkg/client/listers/akash.network/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
)

// ManifestInformer provides access to a shared informer and lister for
// Manifests.
type ManifestInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1.ManifestLister
}

type manifestInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewManifestInformer constructs a new informer for Manifest type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewManifestInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredManifestInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredManifestInformer constructs a new informer for Manifest type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredManifestInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AkashV1().Manifests(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AkashV1().Manifests(namespace).Watch(context.TODO(), options)
			},
		},
		&akashnetworkv1.Manifest{},
		resyncPeriod,
		indexers,
	)
}

func (f *manifestInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredManifestInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *manifestInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&akashnetworkv1.Manifest{}, f.defaultInformer)
}

func (f *manifestInformer) Lister() v1.ManifestLister {
	return v1.NewManifestLister(f.Informer().GetIndexer())
}
