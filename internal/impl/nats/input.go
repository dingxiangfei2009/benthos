package nats

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/input"
	"github.com/benthosdev/benthos/v4/internal/component/input/processors"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/impl/nats/auth"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	btls "github.com/benthosdev/benthos/v4/internal/tls"
)

func init() {
	err := bundle.AllInputs.Add(processors.WrapConstructor(func(c input.Config, nm bundle.NewManagement) (input.Streamed, error) {
		return newNATSInput(c, nm, nm.Logger(), nm.Metrics())
	}), docs.ComponentSpec{
		Name:    "nats",
		Summary: `Subscribe to a NATS subject.`,
		Description: `
### Metadata

This input adds the following metadata fields to each message:

` + "``` text" + `
- nats_subject
- All message headers (when supported by the connection)
` + "```" + `

You can access these metadata fields using [function interpolation](/docs/configuration/interpolation#metadata).

` + auth.Description(),
		Config: docs.FieldComponent().WithChildren(
			docs.FieldString(
				"urls",
				"A list of URLs to connect to. If an item of the list contains commas it will be expanded into multiple URLs.",
				[]string{"nats://127.0.0.1:4222"},
				[]string{"nats://username:password@127.0.0.1:4222"},
			).Array(),
			docs.FieldString("queue", "The queue to consume from."),
			docs.FieldString("subject", "A subject to consume from."),
			docs.FieldInt("prefetch_count", "The maximum number of messages to pull at a time.").Advanced(),
			btls.FieldSpec(),
			auth.FieldSpec(),
		).ChildDefaultAndTypesFromStruct(input.NewNATSConfig()),
		Categories: []string{
			"Services",
		},
	})
	if err != nil {
		panic(err)
	}
}

func newNATSInput(conf input.Config, mgr bundle.NewManagement, log log.Modular, stats metrics.Type) (input.Streamed, error) {
	n, err := newNATSReader(conf.NATS, log)
	if err != nil {
		return nil, err
	}
	return input.NewAsyncReader("nats", true, input.NewAsyncPreserver(n), log, stats)
}

type natsReader struct {
	urls string
	conf input.NATSConfig
	log  log.Modular

	cMut sync.Mutex

	natsConn      *nats.Conn
	natsSub       *nats.Subscription
	natsChan      chan *nats.Msg
	interruptChan chan struct{}
	tlsConf       *tls.Config
}

func newNATSReader(conf input.NATSConfig, log log.Modular) (*natsReader, error) {
	n := natsReader{
		conf:          conf,
		log:           log,
		interruptChan: make(chan struct{}),
	}
	n.urls = strings.Join(conf.URLs, ",")
	if conf.PrefetchCount < 0 {
		return nil, errors.New("prefetch count must be greater than or equal to zero")
	}
	var err error
	if conf.TLS.Enabled {
		if n.tlsConf, err = conf.TLS.Get(); err != nil {
			return nil, err
		}
	}

	return &n, nil
}

func (n *natsReader) ConnectWithContext(ctx context.Context) error {
	n.cMut.Lock()
	defer n.cMut.Unlock()

	if n.natsConn != nil {
		return nil
	}

	var natsConn *nats.Conn
	var natsSub *nats.Subscription
	var err error
	var opts []nats.Option

	if n.tlsConf != nil {
		opts = append(opts, nats.Secure(n.tlsConf))
	}

	opts = append(opts, auth.GetOptions(n.conf.Auth)...)

	if natsConn, err = nats.Connect(n.urls, opts...); err != nil {
		return err
	}
	natsChan := make(chan *nats.Msg, n.conf.PrefetchCount)

	if len(n.conf.QueueID) > 0 {
		natsSub, err = natsConn.ChanQueueSubscribe(n.conf.Subject, n.conf.QueueID, natsChan)
	} else {
		natsSub, err = natsConn.ChanSubscribe(n.conf.Subject, natsChan)
	}

	if err != nil {
		return err
	}

	n.log.Infof("Receiving NATS messages from subject: %v\n", n.conf.Subject)

	n.natsConn = natsConn
	n.natsSub = natsSub
	n.natsChan = natsChan
	return nil
}

func (n *natsReader) disconnect() {
	n.cMut.Lock()
	defer n.cMut.Unlock()

	if n.natsSub != nil {
		_ = n.natsSub.Unsubscribe()
		n.natsSub = nil
	}
	if n.natsConn != nil {
		n.natsConn.Close()
		n.natsConn = nil
	}
	n.natsChan = nil
}

func (n *natsReader) ReadWithContext(ctx context.Context) (*message.Batch, input.AsyncAckFn, error) {
	n.cMut.Lock()
	natsChan := n.natsChan
	natsConn := n.natsConn
	n.cMut.Unlock()

	var msg *nats.Msg
	var open bool
	select {
	case msg, open = <-natsChan:
	case <-ctx.Done():
		return nil, nil, component.ErrTimeout
	case _, open = <-n.interruptChan:
	}
	if !open {
		n.disconnect()
		return nil, nil, component.ErrNotConnected
	}

	bmsg := message.QuickBatch([][]byte{msg.Data})
	part := bmsg.Get(0)
	part.MetaSet("nats_subject", msg.Subject)
	// process message headers if server supports the feature
	if natsConn.HeadersSupported() {
		for key := range msg.Header {
			value := msg.Header.Get(key)
			part.MetaSet(key, value)
		}
	}

	return bmsg, func(ctx context.Context, res error) error {
		var ackErr error
		if res != nil {
			ackErr = msg.Nak()
		} else {
			ackErr = msg.Ack()
		}
		if errors.Is(ackErr, nats.ErrMsgNoReply) {
			ackErr = nil
		}
		return ackErr
	}, nil
}

func (n *natsReader) CloseAsync() {
	close(n.interruptChan)
}

func (n *natsReader) WaitForClose(timeout time.Duration) error {
	return nil
}
