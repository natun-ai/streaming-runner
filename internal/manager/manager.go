/*
Copyright (c) 2022 RaptorML authors.

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

package manager

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	raptorApi "github.com/raptor-ml/raptor/api/v1alpha1"
	"github.com/raptor-ml/streaming-runner/pkg/brokers"
	pbRuntime "go.buf.build/raptor/api-go/raptor/core/raptor/runtime/v1alpha1"
	"gocloud.dev/pubsub"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"net/url"
	ctrlCache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Manager interface {
	Start(context.Context) error
	Ready(context.Context) bool
}
type manager struct {
	client  ctrlCache.Cache
	logger  logr.Logger
	cancel  context.CancelFunc
	src     client.ObjectKey
	runtime pbRuntime.RuntimeServiceClient
	bs      *BaseStreaming
	ready   bool
}

func New(src client.ObjectKey, runtime pbRuntime.RuntimeServiceClient, cfg *rest.Config, logger logr.Logger) (Manager, error) {
	c, err := ctrlCache.New(cfg, ctrlCache.Options{
		Namespace: src.Namespace,
		DefaultSelector: ctrlCache.ObjectSelector{
			Field: fields.OneTermEqualSelector("metadata.name", src.Name),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create controler cache client: %w", err)
	}

	return &manager{
		client:  c,
		logger:  logger,
		runtime: runtime,
	}, nil
}

func (m *manager) Ready(_ context.Context) bool {
	return m.ready
}

func (m *manager) Start(ctx context.Context) error {
	m.logger.Info("Starting...")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	i, err := m.client.GetInformer(ctx, &raptorApi.DataSource{})
	if err != nil {
		panic(err)
	}

	i.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			m.Add(ctx, obj.(*raptorApi.DataSource))
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			m.Update(ctx, oldObj.(*raptorApi.DataSource), newObj.(*raptorApi.DataSource))
		},
		DeleteFunc: func(obj interface{}) {
			m.logger.Info("DataSource deleted. Gracefully closing...")
			cancel()
		},
	})
	go func() {
		<-ctx.Done()
		if m.cancel != nil {
			m.cancel()
		}
	}()

	return m.client.Start(ctx)
}

type BaseStreaming struct {
	BrokerKind string `mapstructure:"kind"`
	Workers    int
	Schema     *url.URL

	subscription *pubsub.Subscription
	mdExtractor  brokers.MetadataExtractor
	features     []*Feature
}

func (m *manager) Add(ctx context.Context, in *raptorApi.DataSource) {
	m.ready = false
	if in.Spec.Kind != "streaming" {
		m.logger.Error(fmt.Errorf("unsupported DataConenctor kind: %s", in.Spec.Kind), "kind is not streaming")
		return
	}

	ctx = brokers.ContextWithDataSource(ctx, in)

	cfg, err := in.ParseConfig(ctx, m.client)
	if err != nil {
		m.logger.Error(err, "failed to retrieve config")
	}

	bs := BaseStreaming{}
	err = cfg.Unmarshal(&bs)
	if err != nil {
		m.logger.Error(err, "failed to unmarshal streaming config")
		return
	}
	if bs.Workers == 0 {
		bs.Workers = 1
	}

	if bs.Schema != nil {
		err := m.registerSchema(ctx, bs.Schema.String())
		if err != nil {
			m.logger.Error(err, "failed to register schema")
			return
		}
	}

	broker := brokers.Get(bs.BrokerKind)
	if broker == nil {
		m.logger.Error(fmt.Errorf("broker %s not found", bs.BrokerKind), "invalid broker kind")
		return
	}
	bs.mdExtractor = broker.Metadata

	// Spawn a sub context for the broker
	// This allowing us to replace the broker context with a new one using cancel
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	// Create a new subscription
	ctx = brokers.ContextWithDataSource(ctx, in)
	ctx, bs.subscription, err = broker.Subscribe(ctx, cfg)
	if err != nil {
		m.logger.Error(err, "failed to create subscription")
		return
	}
	go func(ctx context.Context) {
		<-ctx.Done()
		err := bs.subscription.Shutdown(context.TODO())
		if err != nil {
			m.logger.Error(err, "failed to shutdown streaming")
		}
		m.cancel = nil
	}(ctx)

	bs.features = m.getFeatureDefinitions(ctx, in, bs)
	m.subscribe(ctx, bs)
	m.ready = true
	m.bs = &bs
	m.logger.Info("Listening for streaming events...")
}

func (m *manager) Update(ctx context.Context, _ *raptorApi.DataSource, in *raptorApi.DataSource) {
	if m.cancel != nil {
		m.cancel()
		m.bs = nil
	}

	m.Add(ctx, in)
}

func (m *manager) subscribe(ctx context.Context, bs BaseStreaming) {
	for i := 0; i < bs.Workers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					msg, err := bs.subscription.Receive(ctx)
					if err != nil {
						m.logger.Error(err, "failed to receive message")
						return
					}
					md := bs.mdExtractor(ctx, msg)
					if err := m.handle(ctx, msg, md, bs); err != nil {
						if msg.Nackable() {
							msg.Nack()
						}
						m.logger.Error(err, "failed to handle message")
					}

					msg.Ack()
				}
			}
		}()
	}
}

func newUUID() string {
	return uuid.New().String()
}
