package events

import (
	"encoding/json"
	"time"

	"github.com/stellwerk-labs/golib/hstandardoutbox"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genevents"
)

type CloudEvent[e any] struct {
	SpecVersion CloudEventSpecVersion1 `json:"specversion"`
	Type        genevents.EventType    `json:"type"`
	Time        time.Time              `json:"time"`
	Data        e                      `json:"data"`
}

type CloudEventSpecVersion1 struct{}

func (c CloudEventSpecVersion1) MarshalJSON() ([]byte, error) {
	return []byte(`"1.0"`), nil
}

func (c CloudEventSpecVersion1) UnmarshalJSON(data []byte) error {
	return nil
}

func AsMessage(e CloudEvent[any]) *hstandardoutbox.PendingEventMessage {
	data, _ := json.Marshal(e)
	return &hstandardoutbox.PendingEventMessage{
		Subject:    string(e.Type),
		Payload:    data,
		Expiration: 0,
	}
}

func AsMessages(e ...CloudEvent[any]) []*hstandardoutbox.PendingEventMessage {
	out := make([]*hstandardoutbox.PendingEventMessage, len(e))
	for i, ee := range e {
		out[i] = AsMessage(ee)
	}
	return out
}
