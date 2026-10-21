package events

import (
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stretchr/testify/require"
)

func TestPublicationMessageRoutesOnlyPluginNamespace(t *testing.T) {
	for _, test := range []struct{ subject, want string }{
		{"plugin.dev.example.extension.changed", "io.platform-orchestrator.plugin.dev.example.extension.changed"},
		{"io.platform-orchestrator.project.created", "io.platform-orchestrator.project.created"},
		{"io.platform-orchestrator.plugin.dev.example.extension.changed", "io.platform-orchestrator.plugin.dev.example.extension.changed"},
		{"plugins.dev.example.changed", "plugins.dev.example.changed"},
		{"plugin", "plugin"},
	} {
		t.Run(test.subject, func(t *testing.T) {
			message := hmessaging.Message{
				ID: "retained-outbox-id", Subject: test.subject,
				Data:      []byte(`{"type":"plugin.dev.example.extension.changed"}`),
				Header:    hmessaging.Header{"Traceparent": {"retained-trace"}},
				CreatedAt: time.Unix(1, 0), ExpiresAt: time.Unix(2, 0),
			}
			before := message.Clone()
			actual := publicationMessage(message)
			require.Equal(t, before, message)
			require.Equal(t, test.want, actual.Subject)
			actual.Subject = message.Subject
			require.Equal(t, before, actual)
		})
	}
}
