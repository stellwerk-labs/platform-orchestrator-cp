package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
)

// LoadDeploymentAdmissionIdentities binds each organization to its authenticated
// DP service principal. It contains no credential and is independent of plugins.
func LoadDeploymentAdmissionIdentities(reader io.Reader) (map[string]uuid.UUID, error) {
	encoded, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil || len(encoded) > 1<<20 {
		return nil, fmt.Errorf("deployment admission identities exceed the bounded configuration size or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, fmt.Errorf("deployment admission identities must be an organization-to-service-UUID object")
	}
	result := map[string]uuid.UUID{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid Deployment admission organization entry")
		}
		org, ok := key.(string)
		if !ok || org == "" || len(org) > 200 || strings.ContainsAny(org, "/\r\n\t ") || len(result) >= 10000 {
			return nil, fmt.Errorf("invalid Deployment admission organization identity")
		}
		if _, exists := result[org]; exists {
			return nil, fmt.Errorf("duplicate Deployment admission organization identity")
		}
		var id string
		if err := decoder.Decode(&id); err != nil {
			return nil, fmt.Errorf("deployment admission service identity must be a UUID")
		}
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || parsed.String() != id || !userid.IsServiceUser(parsed) {
			return nil, fmt.Errorf("deployment admission requires a distinct canonical service UUID")
		}
		result[org] = parsed
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("incomplete Deployment admission identities")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after Deployment admission identities")
	}
	return result, nil
}
