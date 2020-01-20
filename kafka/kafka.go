package kafka

import (
	"context"
	"io"
	"time"

	"github.com/Shopify/sarama"
	"github.com/hashicorp/go-multierror"
	"github.com/uw-labs/substrate"
	"github.com/uw-labs/substrate/internal/unwrap"
	"github.com/uw-labs/sync/rungroup"
)

var (
	_ substrate.AsyncMessageSink   = (*asyncMessageSink)(nil)
	_ substrate.AsyncMessageSource = (*asyncMessageSource)(nil)
)

const (
	// OffsetOldest indicates the oldest appropriate message available on the broker.
	OffsetOldest int64 = sarama.OffsetOldest
	// OffsetNewest indicates the next appropriate message available on the broker.
	OffsetNewest int64 = sarama.OffsetNewest

	defaultMetadataRefreshFrequency = 10 * time.Minute
)

type AsyncMessageSinkConfig struct {
	Brokers         []string
	Topic           string
	MaxMessageBytes int
	KeyFunc         func(substrate.Message) []byte
	Version         string
}

func NewAsyncMessageSink(config AsyncMessageSinkConfig) (substrate.AsyncMessageSink, error) {

	conf, err := config.buildSaramaProducerConfig()
	if err != nil {
		return nil, err
	}

	client, err := sarama.NewClient(config.Brokers, conf)
	if err != nil {
		return nil, err
	}

	sink := asyncMessageSink{
		client:  client,
		Topic:   config.Topic,
		KeyFunc: config.KeyFunc,
	}
	return &sink, nil
}

type asyncMessageSink struct {
	client  sarama.Client
	Topic   string
	KeyFunc func(substrate.Message) []byte
}

func (ams *asyncMessageSink) PublishMessages(ctx context.Context, acks chan<- substrate.Message, messages <-chan substrate.Message) (rerr error) {

	producer, err := sarama.NewAsyncProducerFromClient(ams.client)
	if err != nil {
		return err
	}

	err = ams.doPublishMessages(ctx, producer, acks, messages)
	if err != nil {
		_ = producer.Close()
		return err
	}
	return producer.Close()
}

func (ams *asyncMessageSink) doPublishMessages(ctx context.Context, producer sarama.AsyncProducer, acks chan<- substrate.Message, messages <-chan substrate.Message) (rerr error) {

	input := producer.Input()
	errs := producer.Errors()
	successes := producer.Successes()

	go func() {
		for suc := range successes {
			acks <- suc.Metadata.(substrate.Message)
		}
	}()
	for {
		select {
		case m := <-messages:
			message := &sarama.ProducerMessage{
				Topic: ams.Topic,
			}

			message.Value = sarama.ByteEncoder(m.Data())

			if ams.KeyFunc != nil {
				// Provide original user message to the partition key function.
				unwrappedMsg := unwrap.Unwrap(m)
				message.Key = sarama.ByteEncoder(ams.KeyFunc(unwrappedMsg))
			}

			message.Metadata = m
			input <- message
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return err
		}
	}
}

func (ams *asyncMessageSink) Status() (*substrate.Status, error) {
	return status(ams.client, ams.Topic)
}

func (ams *AsyncMessageSinkConfig) buildSaramaProducerConfig() (*sarama.Config, error) {
	conf := sarama.NewConfig()
	conf.Producer.RequiredAcks = sarama.WaitForAll // make configurable
	conf.Producer.Return.Successes = true
	conf.Producer.Return.Errors = true
	conf.Producer.Retry.Max = 3
	conf.Producer.Timeout = time.Duration(60) * time.Second

	if ams.MaxMessageBytes != 0 {
		if ams.MaxMessageBytes > int(sarama.MaxRequestSize) {
			sarama.MaxRequestSize = int32(ams.MaxMessageBytes)
		}
		conf.Producer.MaxMessageBytes = int(ams.MaxMessageBytes)
	}

	if ams.KeyFunc != nil {
		conf.Producer.Partitioner = sarama.NewHashPartitioner
	} else {
		conf.Producer.Partitioner = sarama.NewRoundRobinPartitioner
	}

	if ams.Version != "" {
		version, err := sarama.ParseKafkaVersion(ams.Version)
		if err != nil {
			return nil, err
		}
		conf.Version = version
	}

	return conf, nil
}

// Close implements the Close method of the substrate.AsyncMessageSink
// interface.
func (ams *asyncMessageSink) Close() error {
	return ams.client.Close()
}

// AsyncMessageSource represents a kafka message source and implements the
// substrate.AsyncMessageSource interface.
type AsyncMessageSourceConfig struct {
	ConsumerGroup            string
	Topic                    string
	Brokers                  []string
	Offset                   int64
	MetadataRefreshFrequency time.Duration
	OffsetsRetention         time.Duration
	Version                  string
}

func (ams *AsyncMessageSourceConfig) buildSaramaConsumerConfig() (*sarama.Config, error) {
	offset := OffsetNewest
	if ams.Offset != 0 {
		offset = ams.Offset
	}
	mrf := defaultMetadataRefreshFrequency
	if ams.MetadataRefreshFrequency > 0 {
		mrf = ams.MetadataRefreshFrequency
	}

	config := sarama.NewConfig()
	config.Consumer.Return.Errors = true
	config.Consumer.Offsets.Initial = offset
	config.Metadata.RefreshFrequency = mrf
	config.Consumer.Offsets.Retention = ams.OffsetsRetention

	if ams.Version != "" {
		version, err := sarama.ParseKafkaVersion(ams.Version)
		if err != nil {
			return nil, err
		}
		config.Version = version
	}

	return config, nil
}

func NewAsyncMessageSource(c AsyncMessageSourceConfig) (substrate.AsyncMessageSource, error) {
	config, err := c.buildSaramaConsumerConfig()
	if err != nil {
		return nil, err
	}

	client, err := sarama.NewClient(c.Brokers, config)
	if err != nil {
		return nil, err
	}
	consumerGroup, err := sarama.NewConsumerGroupFromClient(c.ConsumerGroup, client)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	return &asyncMessageSource{
		client:        client,
		consumerGroup: consumerGroup,
		topic:         c.Topic,
	}, nil
}

type asyncMessageSource struct {
	client        sarama.Client
	consumerGroup sarama.ConsumerGroup
	topic         string
}

type consumerMessage struct {
	sess sarama.ConsumerGroupSession
	cm   *sarama.ConsumerMessage

	offset *struct {
		topic     string
		partition int32
		offset    int64
	}
}

func (cm *consumerMessage) Data() []byte {
	if cm.cm == nil {
		panic("attempt to use payload after discarding.")
	}
	return cm.cm.Value
}

func (cm *consumerMessage) DiscardPayload() {
	if cm.offset != nil {
		// already discarded
		return
	}
	cm.offset = &struct {
		topic     string
		partition int32
		offset    int64
	}{
		cm.cm.Topic,
		cm.cm.Partition,
		cm.cm.Offset,
	}
	cm.cm = nil
}

func (cm *consumerMessage) ack() {
	if cm.cm != nil {
		cm.sess.MarkMessage(cm.cm, "")
	} else {
		off := cm.offset
		cm.sess.MarkOffset(off.topic, off.partition, off.offset, "")
	}
}

type consumerGroupHandler struct {
	ctx context.Context

	messages chan<- substrate.Message
	toAck    chan<- *consumerMessage
}

// Setup is run at the beginning of a new session, before ConsumeClaim.
func (_ *consumerGroupHandler) Setup(_ sarama.ConsumerGroupSession) error { return nil }

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
// but before the offsets are committed for the very last time.
func (c *consumerGroupHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
// Once the Messages() channel is closed, the Handler must finish its processing
// loop and exit.
func (c *consumerGroupHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	// This function can be called concurrently for multiple claims, so the code
	// below, absent locking etc may seem wrong, but it's actually fine.
	// Different partition claims can be processed concurrently, but we funnel
	// them all into c.messages.  The caller puts all acks into c.acks and it
	// doesn't matter which one of us processes the offset marking because they
	// all work with the same session.
	for {
		select {
		case <-c.ctx.Done():
			return nil
		case m, ok := <-claim.Messages():
			if !ok {
				return nil
			}
			cm := &consumerMessage{cm: m, sess: sess}
			select {
			case c.toAck <- cm:
			case <-c.ctx.Done():
				return nil
			}
			select {
			case c.messages <- cm:
			case <-c.ctx.Done():
				return nil
			}
		}
	}
}

func (ams *asyncMessageSource) processAcks(ctx context.Context, messages <-chan *consumerMessage, acks <-chan substrate.Message) error {
	var forAcking []*consumerMessage

	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-messages:
			forAcking = append(forAcking, m)
		case ack := <-acks:
			switch {
			case len(forAcking) == 0:
				return substrate.InvalidAckError{
					Acked:    ack,
					Expected: nil,
				}
			case ack != forAcking[0]:
				return substrate.InvalidAckError{
					Acked:    ack,
					Expected: forAcking[0],
				}
			default:
				forAcking[0].ack()
				forAcking = forAcking[1:]
			}
		}
	}
}

func (ams *asyncMessageSource) ConsumeMessages(ctx context.Context, messages chan<- substrate.Message, acks <-chan substrate.Message) error {
	rg, ctx := rungroup.New(ctx)
	toAck := make(chan *consumerMessage)

	rg.Go(func() error {
		return ams.processAcks(ctx, toAck, acks)
	})
	rg.Go(func() error {
		return ams.consumerGroup.Consume(ctx, []string{ams.topic}, &consumerGroupHandler{
			ctx:      ctx,
			messages: messages,
			toAck:    toAck,
		})
	})

	return rg.Wait()
}

func (ams *asyncMessageSource) Status() (*substrate.Status, error) {
	return status(ams.client, ams.topic)
}

func (ams *asyncMessageSource) Close() (err error) {
	for _, closer := range []io.Closer{ams.consumerGroup, ams.client} {
		err = multierror.Append(err, closer.Close()).ErrorOrNil()
	}
	return err
}
