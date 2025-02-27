package pure

import (
	"context"
	"time"

	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component/output"
	"github.com/benthosdev/benthos/v4/internal/component/output/processors"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
)

func init() {
	err := bundle.AllOutputs.Add(processors.WrapConstructor(func(c output.Config, nm bundle.NewManagement) (output.Streamed, error) {
		return output.NewAsyncWriter("drop", 1, newDropWriter(c.Drop, nm.Logger()), nm.Logger(), nm.Metrics())
	}), docs.ComponentSpec{
		Name:       "drop",
		Summary:    `Drops all messages.`,
		Categories: []string{"Utility"},
		Config:     docs.FieldObject("", "").HasDefault(struct{}{}),
	})
	if err != nil {
		panic(err)
	}
}

type dropWriter struct {
	log log.Modular
}

func newDropWriter(conf output.DropConfig, log log.Modular) *dropWriter {
	return &dropWriter{log: log}
}

func (d *dropWriter) ConnectWithContext(ctx context.Context) error {
	d.log.Infoln("Dropping messages.")
	return nil
}

func (d *dropWriter) WriteWithContext(ctx context.Context, msg *message.Batch) error {
	return nil
}

func (d *dropWriter) CloseAsync() {
}

func (d *dropWriter) WaitForClose(time.Duration) error {
	return nil
}
