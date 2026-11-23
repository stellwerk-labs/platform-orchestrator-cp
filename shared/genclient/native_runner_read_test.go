package genclient

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNativeRunnerReadPreservesAbsentInventoryAndOptionalLists(t *testing.T) {
	for name, input := range map[string]string{
		"absent":      `{"type":"native-agent","key":"public-key","max_parallel_jobs":2}`,
		"empty lists": `{"type":"native-agent","key":"public-key","max_parallel_jobs":2,"capabilities":[],"supported_protocols":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var configuration NativeAgentRunnerConfiguration
			if err := json.Unmarshal([]byte(input), &configuration); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(configuration)
			if err != nil {
				t.Fatal(err)
			}
			var before, after map[string]json.RawMessage
			beforeErr := json.Unmarshal([]byte(input), &before)
			afterErr := json.Unmarshal(encoded, &after)
			if beforeErr != nil || afterErr != nil {
				t.Fatalf("decode round-trip objects: input=%v output=%v", beforeErr, afterErr)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("native READ changed absence or explicit empty lists: %s", encoded)
			}
		})
	}
}
