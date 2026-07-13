package orchestratoriam

//go:generate go tool mockgen -destination mocks/client_mock.go -package mockorchestratoriam github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient ClientWithResponsesInterface
