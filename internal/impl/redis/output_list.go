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
		return newRedisListOutput(c, nm, nm.Logger(), nm.Metrics())
	}), docs.ComponentSpec{
		Name: "redis_list",
		Summary: `
Pushes messages onto the end of a Redis list (which is created if it doesn't
already exist) using the RPUSH command.`,
		Description: output.Description(true, true, `
The field `+"`key`"+` supports
[interpolation functions](/docs/configuration/interpolation#bloblang-queries), allowing
you to create a unique key for each message.`),
		Config: docs.FieldComponent().WithChildren(old.ConfigDocs()...).WithChildren(
			docs.FieldString(
				"key", "The key for each message, function interpolations can be optionally used to create a unique key per message.",
				"benthos_list", "${!meta(\"kafka_key\")}", "${!json(\"doc.id\")}", "${!count(\"msgs\")}",
			).IsInterpolated(),
			docs.FieldInt("max_in_flight", "The maximum number of messages to have in flight at a given time. Increase this to improve throughput."),
			policy.FieldSpec(),
		).ChildDefaultAndTypesFromStruct(output.NewRedisListConfig()),
		Categories: []string{
			"Services",
		},
	})
	if err != nil {
		panic(err)
	}
}

func newRedisListOutput(conf output.Config, mgr bundle.NewManagement, log log.Modular, stats metrics.Type) (output.Streamed, error) {
	w, err := newRedisListWriter(conf.RedisList, mgr, log)
	if err != nil {
		return nil, err
	}
	a, err := output.NewAsyncWriter("redis_list", conf.RedisList.MaxInFlight, w, log, stats)
	if err != nil {
		return nil, err
	}
	return batcher.NewFromConfig(conf.RedisList.Batching, a, mgr, log, stats)
}

type redisListWriter struct {
	log log.Modular

	conf output.RedisListConfig

	keyStr *field.Expression

	client  redis.UniversalClient
	connMut sync.RWMutex
}

func newRedisListWriter(conf output.RedisListConfig, mgr bundle.NewManagement, log log.Modular) (*redisListWriter, error) {
	r := &redisListWriter{
		log:  log,
		conf: conf,
	}

	var err error
	if r.keyStr, err = mgr.BloblEnvironment().NewField(conf.Key); err != nil {
		return nil, fmt.Errorf("failed to parse key expression: %v", err)
	}
	if _, err := clientFromConfig(conf.Config); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *redisListWriter) ConnectWithContext(ctx context.Context) error {
	r.connMut.Lock()
	defer r.connMut.Unlock()

	client, err := clientFromConfig(r.conf.Config)
	if err != nil {
		return err
	}
	if _, err = client.Ping().Result(); err != nil {
		return err
	}

	r.client = client
	return nil
}

func (r *redisListWriter) WriteWithContext(ctx context.Context, msg *message.Batch) error {
	r.connMut.RLock()
	client := r.client
	r.connMut.RUnlock()

	if client == nil {
		return component.ErrNotConnected
	}

	if msg.Len() == 1 {
		key := r.keyStr.String(0, msg)
		if err := client.RPush(key, msg.Get(0).Get()).Err(); err != nil {
			_ = r.disconnect()
			r.log.Errorf("Error from redis: %v\n", err)
			return component.ErrNotConnected
		}
		return nil
	}

	pipe := client.Pipeline()
	_ = msg.Iter(func(i int, p *message.Part) error {
		key := r.keyStr.String(0, msg)
		_ = pipe.RPush(key, p.Get())
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

func (r *redisListWriter) disconnect() error {
	r.connMut.Lock()
	defer r.connMut.Unlock()
	if r.client != nil {
		err := r.client.Close()
		r.client = nil
		return err
	}
	return nil
}

func (r *redisListWriter) CloseAsync() {
	go func() {
		_ = r.disconnect()
	}()
}

func (r *redisListWriter) WaitForClose(timeout time.Duration) error {
	return nil
}
