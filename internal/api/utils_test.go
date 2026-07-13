package api

import (
	"context"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hrabbitmq"
	"github.com/stellwerk-labs/golib/hrabbitmq/reliableoutbox"
	"github.com/stellwerk-labs/golib/hstandardreliableoutbox"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	mockorchestratordp "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratordp/mocks"
	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	mockvault "github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault/mocks"
)

func MockServer(t *testing.T) (*echo.Echo, *Server, func()) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	e, _ := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{
		AppName:          "test",
		Logger:           zaptest.NewLogger(t),
		OpenAPIRawSchema: MustDecodeOpenApiSpec(),
	})
	e.Use(middleware.EchoCtxMiddleware)
	db := mockmodel.NewMockDatabaser(ctrl)
	tx := mockmodel.NewMockTxWithCommit(ctrl)
	db.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil).AnyTimes()
	tx.EXPECT().Rollback().Return(nil).AnyTimes()
	tx.EXPECT().Commit().Return(nil).AnyTimes()

	store := new(reliableoutbox.InMemoryStorage[*hstandardreliableoutbox.PendingEventMessage])
	db.EXPECT().InsertPendingEventMessages(gomock.Any(), gomock.Not(nil), gomock.Any()).DoAndReturn(func(_ context.Context, _ model.Tx, messages []*hstandardreliableoutbox.PendingEventMessage) ([]*hstandardreliableoutbox.PendingEventMessage, error) {
		for i, message := range messages {
			message.Id = int64(i)
		}
		store.Put(messages)
		return messages, nil
	}).AnyTimes()
	db.EXPECT().AsReliableOutboxStore().Return(store).AnyTimes()
	vlt := mockvault.NewMockVaultClientInterface(ctrl)
	orchestratoriam := mockorchestratoriam.NewMockClientWithResponsesInterface(ctrl)
	orchestratordp := mockorchestratordp.NewMockClientWithResponsesInterface(ctrl)
	s := &Server{
		Logger: zaptest.NewLogger(t), Database: db, Vault: vlt,
		RabbitMqPublisher: new(hrabbitmq.NoOpPublisher),
		IamClient:         orchestratoriam,
		DpClient:          orchestratordp,
	}
	s.MapRoutes(e)
	return e, s, func() {
		ctrl.Finish()
	}
}
