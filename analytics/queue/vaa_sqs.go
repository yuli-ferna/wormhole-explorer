package queue

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	sqs_client "github.com/wormhole-foundation/wormhole-explorer/common/client/sqs"
)

// SQSOption represents a VAA queue in SQS option function.
type SQSOption func(*SQS)

// SQS represents a VAA queue in SQS.
type SQS struct {
	consumer *sqs_client.Consumer
	ch       chan ConsumerMessage
	chSize   int
	wg       sync.WaitGroup
	logger   *zap.Logger
}

// NewVaaSqs creates a VAA queue in SQS instances.
func NewVaaSqs(consumer *sqs_client.Consumer, logger *zap.Logger, opts ...SQSOption) *SQS {
	s := &SQS{
		consumer: consumer,
		chSize:   10,
		logger:   logger}
	for _, opt := range opts {
		opt(s)
	}
	s.ch = make(chan ConsumerMessage, s.chSize)
	return s
}

// WithChannelSize allows to specify an channel size when setting a value.
func WithChannelSize(size int) SQSOption {
	return func(d *SQS) {
		d.chSize = size
	}
}

// Consume returns the channel with the received messages from SQS queue.
func (q *SQS) Consume(ctx context.Context) <-chan ConsumerMessage {
	go func() {
		for {
			messages, err := q.consumer.GetMessages(ctx)
			if err != nil {
				q.logger.Error("Error getting messages from SQS", zap.Error(err))
				continue
			}
			expiredAt := time.Now().Add(q.consumer.GetVisibilityTimeout())
			for _, msg := range messages {
				// unmarshal body to sqsEvent
				var sqsEvent sqsEvent
				err := json.Unmarshal([]byte(*msg.Body), &sqsEvent)
				if err != nil {
					q.logger.Error("Error decoding message from SQS", zap.Error(err))
					continue
				}

				// unmarshal message to vaaEvent
				var vaaEvent VaaEvent
				err = json.Unmarshal([]byte(sqsEvent.Message), &vaaEvent)
				if err != nil {
					q.logger.Error("Error decoding vaaEvent message from SQSEvent", zap.Error(err))
					continue
				}

				q.wg.Add(1)
				q.ch <- &sqsConsumerMessage{
					id:        msg.ReceiptHandle,
					data:      &vaaEvent,
					wg:        &q.wg,
					logger:    q.logger,
					consumer:  q.consumer,
					expiredAt: expiredAt,
					ctx:       ctx,
				}
			}
			q.wg.Wait()
		}

	}()
	return q.ch
}

// Close closes all consumer resources.
func (q *SQS) Close() {
	close(q.ch)
}

type sqsConsumerMessage struct {
	data      *VaaEvent
	consumer  *sqs_client.Consumer
	wg        *sync.WaitGroup
	id        *string
	logger    *zap.Logger
	expiredAt time.Time
	ctx       context.Context
}

func (m *sqsConsumerMessage) Data() *VaaEvent {
	return m.data
}

func (m *sqsConsumerMessage) Done() {
	if err := m.consumer.DeleteMessage(m.ctx, m.id); err != nil {
		m.logger.Error("Error deleting message from SQS", zap.Error(err))
	}
	m.wg.Done()
}

func (m *sqsConsumerMessage) Failed() {
	m.wg.Done()
}

func (m *sqsConsumerMessage) IsExpired() bool {
	return m.expiredAt.Before(time.Now())
}