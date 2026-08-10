package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genevents"
)

func TestAsMessagePreservesCloudEventTypeAsSubject(t *testing.T) {
	event := CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorProjectCreated,
		Time: time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC),
		Data: map[string]any{"id": "project-1"},
	}
	pending := AsMessage(event)
	if pending.Subject != "io.platform-orchestrator.project.created" {
		t.Fatalf("subject = %q", pending.Subject)
	}
	var encoded CloudEvent[json.RawMessage]
	if err := json.Unmarshal(pending.Payload, &encoded); err != nil {
		t.Fatal(err)
	}
	if encoded.Type != event.Type || !encoded.Time.Equal(event.Time) {
		t.Fatalf("encoded CloudEvent = %#v", encoded)
	}
}
