package worker

import (
	"context"
	"regexp"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/worker/handlers"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/worker/handlers/branchhandler"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/worker/middleware"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/stellwerk-labs/golib/hrabbitmq"
	delayqueues "github.com/stellwerk-labs/golib/hrabbitmq/delayqueues/v2"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/genclient"
	iamclient "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/wagslane/go-rabbitmq"
	"go.uber.org/zap"
)

type Worker struct {
	RabbitConn      *rabbitmq.Conn
	RabbitPublisher hrabbitmq.Publisher
	Cache           *expirable.LRU[string, int32]
	Logger          *zap.Logger

	DB model.Databaser

	IamClient iamclient.ClientWithResponsesInterface
	DpClient  orchestratordp.ClientWithResponsesInterface
}

func (w *Worker) BuildMainConsumer() (*hrabbitmq.ConsumerWithHandlerWaiter, error) {
	var inner handlers.Handler = &branchhandler.Handler{
		{PrefixPattern: regexp.MustCompile(""), Handler: handlers.HandlerFunc(func(ctx context.Context, logger *zap.Logger, d *rabbitmq.Delivery) error {
			logger.Info("dropping unsupported message")
			return nil
		})},
	}

	// This middleware handles timeouts, panic recovery, graceful retries, and logging
	inner = middleware.WrapWithObserver(inner, "main-consumer", w.RabbitPublisher, w.Cache)

	return hrabbitmq.NewConsumerWithHandlerWaiter(
		w.RabbitConn,
		func(d rabbitmq.Delivery) (action rabbitmq.Action) {
			if err := inner.Handle(context.TODO(), w.Logger, &d); err != nil {
				return rabbitmq.NackDiscard
			}
			return rabbitmq.Ack
		},
		"platform-orchestrator-cp-main",
		rabbitmq.WithConsumerOptionsLogger(hrabbitmq.NewLogger(w.Logger)),
		rabbitmq.WithConsumerOptionsConsumerAutoAck(false),
		rabbitmq.WithConsumerOptionsConcurrency(MainConsumerConcurrency),
		rabbitmq.WithConsumerOptionsQueueDurable,
		rabbitmq.WithConsumerOptionsQueueArgs(rabbitmq.Table{
			"x-queue-type":              "quorum",
			"x-message-ttl":             MainConsumerMessageTtl.Milliseconds(),
			"x-dead-letter-exchange":    delayqueues.DefaultExchange,
			"x-dead-letter-routing-key": delayqueues.DeadLetterRoutingKey,
			// ensure we dead letter things correctly
			"x-dead-letter-strategy": "at-least-once",
			// ensure we reject publish if queue is full
			"x-overflow": "reject-publish",
		}),
		rabbitmq.WithConsumerOptionsExchangeName(events.DefaultExchange),
	)
}
