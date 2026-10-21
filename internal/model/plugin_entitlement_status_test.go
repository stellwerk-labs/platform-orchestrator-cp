package model

import "testing"

func TestPluginEntitlementFallback(t *testing.T) {
	states := []string{"", "not_entitled", "valid", "grace", "expired", "revoked", "untrusted", "not_yet_valid"}
	for _, own := range states {
		for _, wide := range states {
			t.Run(own+"/"+wide, func(t *testing.T) {
				want := (own == "expired" || own == "revoked") && (wide == "valid" || wide == "grace")
				if got := PluginEntitlementFallback(own, wide); got != want {
					t.Fatalf("fallback(%q, %q) = %v, want %v", own, wide, got, want)
				}
			})
		}
	}
}
