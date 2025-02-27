package redis

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v7"

	ibatch "github.com/benthosdev/benthos/v4/internal/batch"
	"github.com/benthosdev/benthos/v4/internal/batch/policy"
	"github.com/benthosdev/benthos/v4/internal/bloblang/field"
	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/component/output"
	"github.com/benthosdev/benthos/v4/internal/component/output/batcher"
	"github.com/benthosdev/benthos/v4/internal/component/output/processors"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/impl/redis/old"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
)

func init() {
	err := bundle.AllOutputs.Add(processors.WrapConstructor(func(c output.Config, nm bundle.NewManagement) (output.Streamed, error) {
		return newRedisPubSubOutput(c, nm, nm.Logger(), nm.Metrics())
	}), docs.ComponentSpec{
		Name: "redis_pubsub",
		Summary: `
Publishes messages through the Redis PubSub model. It is not possible to
guarantee that messages have been received.`,
		Description: output.Description(true, true, `
This output will interpolate functions within the channel field, you
can find a list of functions [here](/docs/configuration/interpolation#bloblang-queries).`),
		Config: docs.FieldComponent().WithChildren(old.ConfigDocs()...).WithChildren(
			docs.FieldString("channel", "The channel to publish messages to.").IsInterpolated(),
			docs.FieldInt("max_in_flight", "The maximum number of messages to have in flight at a given time. Increase this to improve throughput."),
			policy.FieldSpec(),
		).ChildDefaultAndTypesFromStruct(output.NewRedisPubSubConfig()),
		Categories: []string{
			"Services",
		},
	})
	if err != nil {
		panic(err)
	}
}

func newRedisPubSubOutput(conf output.Config, mgr bundle.NewManagement, log log.Modular, stats metrics.Type) (output.Streamed, error) {
	w, err := newRedisPubSubWriter(conf.RedisPubSub, mgr, log)
	if err != nil {
		return nil, err
	}
	a, err := output.NewAsyncWriter("redis_pubsub", conf.RedisPubSub.MaxInFlight, w, log, stats)
	if err != nil {
		return nil, err
	}
	return batcher.NewFromConfig(conf.RedisPubSub.Batching, a, mgr, log, stats)
}

type redisPubSubWriter struct {
	log log.Modular

	conf       output.RedisPubSubConfig
	channelStr *field.Expression

	client  redis.UniversalClient
	connMut sync.RWMutex
}

func newRedisPubSubWriter(conf output.RedisPubSubConfig, mgr bundle.NewManagement, log log.Modular) (*redisPubSubWriter, error) {
	r := &redisPubSubWriter{
		log:  log,
		conf: conf,
	}
	var err error
	if r.channelStr, err = mgr.BloblEnvironment().NewField(conf.Channel); err != nil {
		return nil, fmt.Errorf("failed to parse channel expression: %v", err)
	}
	if _, err = clientFromConfig(conf.Config); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *redisPubSubWriter) ConnectWithContext(ctx context.Context) error {
	r.connMut.Lock()
	defer r.connMut.Unlock()

	client, err := clientFromConfig(r.conf.Config)
	if err != nil {
		return err
	}
	if _, err = client.Ping().Result(); err != nil {
		return err
	}

	r.log.Infof("Pushing messages to Redis channel: %v\n", r.conf.Channel)

	r.client = client
	return nil
}

func (r *redisPubSubWriter) WriteWithContext(ctx context.Context, msg *message.Batch) error {
	r.connMut.RLock()
	client := r.client
	r.connMut.RUnlock()

	if client == nil {
		return component.ErrNotConnected
	}

	if msg.Len() == 1 {
		channel := r.channelStr.String(0, msg)
		if err := client.Publish(channel, msg.Get(0).Get()).Err(); err != nil {
			_ = r.disconnect()
			r.log.Errorf("Error from redis: %v\n", err)
			return component.ErrNotConnected
		}
		return nil
	}

	pipe := client.Pipeline()
	_ = msg.Iter(func(i int, p *message.Part) error {
		_ = pipe.Publish(r.channelStr.String(i, msg), p.Get())
		return nil
	})
	cmders, err := pipe.Exec()
	if err != nil {
		_ = r.disconnect()
		r.log.Errorf("Error from redis: %v\n", err)
		return component.ErrNotConnected
	}

	var batchErr *ibatch.Error
	for i, res := range cmders {
		if res.Err() != nil {
			if batchErr == nil {
				batchErr = ibatch.NewError(msg, res.Err())
			}
			batchErr.Failed(i, res.Err())
		}
	}
	if batchErr != nil {
		return batchErr
	}
	return nil
}

func (r *redisPubSubWriter) disconnect() error {
	r.connMut.Lock()
	defer r.connMut.Unlock()
	if r.client != nil {
		err := r.client.Close()
		r.client = nil
		return err
	}
	return nil
}

func (r *redisPubSubWriter) CloseAsync() {
	_ = r.disconnect()
}

func (r *redisPubSubWriter) WaitForClose(timeout time.Duration) error {
	return nil
}
