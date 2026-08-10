package orchestratordp

//go:generate go tool mockgen -destination mocks/client_mock.go -package mockorchestratordp github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient ClientWithResponsesInterface
