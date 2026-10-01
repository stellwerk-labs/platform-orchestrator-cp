package events

import (
	"context"
	"strings"

	"github.com/stellwerk-labs/golib/hmessaging"
)

// NewPublisher routes plugin CloudEvents through the existing platform stream.
// Mapping at delivery also covers durable outbox entries already awaiting retry,
// without rewriting their immutable event type, payload or deduplication ID.
func NewPublisher(transport hmessaging.Publisher) hmessaging.Publisher {
	return hmessaging.PublisherFunc(func(ctx context.Context, message hmessaging.Message) error {
		return transport.Publish(ctx, publicationMessage(message))
	})
}

func publicationMessage(message hmessaging.Message) hmessaging.Message {
	if strings.HasPrefix(message.Subject, "plugin.") {
		message.Subject = strings.TrimSuffix(hmessaging.EventsSubjectPattern, ">") + message.Subject
	}
	return message
}
