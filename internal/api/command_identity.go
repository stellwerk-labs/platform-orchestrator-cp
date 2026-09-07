package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// commandIdentity binds an idempotency key to the exact typed operation and
// payload. It is shared by Core and add-on command handlers.
func commandIdentity(name string, payload any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("encode typed command identity: %v", err))
	}
	return fmt.Sprintf("%s:sha256:%x", name, sha256.Sum256(encoded))
}
