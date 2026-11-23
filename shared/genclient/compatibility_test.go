package genclient_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
)

func assertActionCompatibility[T ~string](t *testing.T, legacy, current T, wire string) {
	t.Helper()
	if legacy != current || string(legacy) != wire {
		t.Fatalf("action changed: legacy=%q current=%q wire=%q", legacy, current, wire)
	}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `"`+wire+`"` {
		t.Fatalf("wire value changed: %s", encoded)
	}
}

func TestPublishedActionNamesRemainSourceAndWireCompatible(t *testing.T) {
	for _, action := range []struct {
		legacy, current genclient.PluginOperationAuthorizationRequestAction
		wire            string
	}{
		{genclient.Activate, genclient.PluginOperationAuthorizationRequestActionActivate, "activate"},
		{genclient.Prepare, genclient.PluginOperationAuthorizationRequestActionPrepare, "prepare"},
		{genclient.Release, genclient.PluginOperationAuthorizationRequestActionRelease, "release"},
		{genclient.Suspend, genclient.PluginOperationAuthorizationRequestActionSuspend, "suspend"},
	} {
		t.Run("authorization/"+action.wire, func(t *testing.T) {
			assertActionCompatibility(t, action.legacy, action.current, action.wire)
			if !action.legacy.Valid() {
				t.Fatal("published authorization action is no longer valid")
			}
		})
	}
	for _, action := range []struct {
		legacy, current genclient.PluginPinOperationReceiptAction
		wire            string
	}{
		{genclient.Begin, genclient.PluginPinOperationReceiptActionBegin, "begin"},
		{genclient.Cancel, genclient.PluginPinOperationReceiptActionCancel, "cancel"},
		{genclient.Reconcile, genclient.PluginPinOperationReceiptActionReconcile, "reconcile"},
		{genclient.Restore, genclient.PluginPinOperationReceiptActionRestore, "restore"},
	} {
		t.Run("pin/"+action.wire, func(t *testing.T) {
			assertActionCompatibility(t, action.legacy, action.current, action.wire)
			if !action.legacy.Valid() {
				t.Fatal("published Pin action is no longer valid")
			}
		})
	}
	if reflect.TypeOf(genclient.Prepare) != reflect.TypeOf(genclient.PluginOperationAuthorizationRequestActionPrepare) ||
		reflect.TypeOf(genclient.Begin) != reflect.TypeOf(genclient.PluginPinOperationReceiptActionBegin) {
		t.Fatal("legacy constants lost their enum types")
	}
}

func TestPublishedContributionModeNamesRemainSourceAndWireCompatible(t *testing.T) {
	if reflect.TypeOf(genclient.Full) != reflect.TypeOf(genclient.PluginContributionModeFull) ||
		reflect.TypeOf(genclient.Hidden) != reflect.TypeOf(genclient.PluginContributionModeHidden) ||
		reflect.TypeOf(genclient.ReadOnly) != reflect.TypeOf(genclient.PluginContributionModeReadOnly) ||
		reflect.TypeOf(genclient.SafetyOnly) != reflect.TypeOf(genclient.PluginContributionModeSafetyOnly) {
		t.Fatal("legacy contribution constants lost their enum types")
	}
	for _, mode := range []struct {
		legacy, current genclient.PluginContributionMode
		wire            string
	}{
		{genclient.Full, genclient.PluginContributionModeFull, "full"},
		{genclient.Hidden, genclient.PluginContributionModeHidden, "hidden"},
		{genclient.ReadOnly, genclient.PluginContributionModeReadOnly, "read_only"},
		{genclient.SafetyOnly, genclient.PluginContributionModeSafetyOnly, "safety_only"},
	} {
		t.Run(mode.wire, func(t *testing.T) {
			assertActionCompatibility(t, mode.legacy, mode.current, mode.wire)
			if !mode.legacy.Valid() {
				t.Fatal("published contribution mode is no longer valid")
			}
		})
	}
}

func TestPublishedScopeKeyRemainsAvailable(t *testing.T) {
	if genclient.UserIdHeaderScopes != "userIdHeader.Scopes" {
		t.Fatal("published scope key changed")
	}
}
