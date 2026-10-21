//go:build integration

package events

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPluginPublicationUsesExistingJetStreamAndRetainsIdentity(t *testing.T) {
	address := os.Getenv("PLUGIN_EVENTS_TEST_NATS_URL")
	u, err := url.Parse(address)
	require.NoError(t, err)
	require.Equal(t, "nats", u.Scheme, "set PLUGIN_EVENTS_TEST_NATS_URL to a disposable loopback JetStream")
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname())
	require.NotEmpty(t, u.Port())
	require.Equal(t, "1", os.Getenv("PLUGIN_EVENTS_TEST_DISPOSABLE"), "requires a dedicated disposable broker")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	connection, err := hnats.Connect(hnats.ConnectionConfig{URLs: []string{address}, Name: "plugin-event-publication-test"}, zap.NewNop())
	require.NoError(t, err)
	defer connection.Close()
	js, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	stream, err := hnats.EnsureStream(ctx, js, hnats.EventsStreamConfig(1))
	require.NoError(t, err)
	initial, err := stream.Info(ctx)
	require.NoError(t, err)
	require.Zero(t, initial.State.Msgs, "requires an empty disposable broker")
	require.Equal(t, []string{hmessaging.EventsSubjectPattern}, initial.Config.Subjects)
	transport := hnats.NewPublisher(js, hmessaging.EventsStreamName, zap.NewNop())
	publisher := NewPublisher(transport)
	payload := []byte(`{"specversion":"1.0","id":"fixture-event","type":"plugin.dev.example.extension.changed","data":{"status":"done"}}`)
	message := hmessaging.Message{ID: "fixture-outbox-id", Subject: "plugin.dev.example.extension.changed", Data: payload, CreatedAt: time.Now().UTC()}
	require.NoError(t, publisher.Publish(ctx, message))
	require.NoError(t, publisher.Publish(ctx, message), "retry after an uncertain acknowledgement keeps the same ID")
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.State.Msgs, "JetStream deduplicates the retained outbox identity")
	stored, err := stream.GetMsg(ctx, info.State.FirstSeq)
	require.NoError(t, err)
	require.Equal(t, "io.platform-orchestrator.plugin.dev.example.extension.changed", stored.Subject)
	require.Equal(t, payload, stored.Data)
	require.Equal(t, message.ID, stored.Header.Get("Nats-Msg-Id"))
	message.ID = "fixture-core-event"
	message.Subject = "io.platform-orchestrator.project.created"
	message.Data = []byte(`{"type":"io.platform-orchestrator.project.created"}`)
	require.NoError(t, publisher.Publish(ctx, message))
	info, err = stream.Info(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, info.State.Msgs)
	stored, err = stream.GetMsg(ctx, info.State.LastSeq)
	require.NoError(t, err)
	require.Equal(t, message.Subject, stored.Subject)
	require.Equal(t, message.Data, stored.Data)
}
