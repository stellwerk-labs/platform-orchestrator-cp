package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hconfig"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/golib/hrabbitmq"
	delayqueues "github.com/stellwerk-labs/golib/hrabbitmq/delayqueues/v2"
	"github.com/stellwerk-labs/golib/hrabbitmq/reliableoutbox"
	"github.com/stellwerk-labs/golib/hretrier"
	"github.com/stellwerk-labs/golib/hstandardreliableoutbox"
	"github.com/stellwerk-labs/golib/hvaultapi"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/genclient"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/wagslane/go-rabbitmq"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api"
	apimiddleware "github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/worker"

	vaultapi "github.com/hashicorp/vault/api"
	htelemetry "github.com/stellwerk-labs/golib/htelemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	rabbitmqCacheSize = 10000
	rabbitmqCacheTtl  = 30 * time.Minute
)

var buildInfo *debug.BuildInfo

func init() {
	buildInfo, _ = debug.ReadBuildInfo()
}

func main() {
	logw, err := hlogger.NewHLogger("INFO", false, "json")
	if err != nil {
		log.Fatalf("Error building logger: %v (%s %s)", err, path.Base(buildInfo.Main.Path), buildInfo.Main.Version)
	}
	defer hlogger.OnExit(logw.Logger)
	zap.ReplaceGlobals(logw.Logger)
	zap.L().Info("Starting", zap.String("app", path.Base(buildInfo.Main.Path)), zap.String("version", buildInfo.Main.Version))

	cfg := new(config.Configuration)
	if err := hconfig.LoadConfigWithoutRetag(cfg); err != nil {
		zap.L().Fatal("failed to load config", zap.Error(err))
	}

	if err := logw.ChangeLevel(cfg.LogLevel); err != nil {
		zap.L().Fatal("error setting log level", zap.Error(err))
	}

	ctx := context.Background()
	if cfg.OTELEnabled {
		_, shutdown, err := htelemetry.StartOTel(ctx, htelemetry.OTelConfig{
			ServiceName:    path.Base(buildInfo.Main.Path),
			ServiceVersion: buildInfo.Main.Version,
			Logger:         zap.L(),

			// Custom TracerProvider options (e.g., sampling)
			TracerProviderOptions: []sdktrace.TracerProviderOption{
				sdktrace.WithSampler(sdktrace.TraceIDRatioBased(1)),
			},
			RuntimeMetrics:         ref.Ref(true),
			RuntimeMetricsInterval: 5 * time.Second,
		})

		if err != nil {
			zap.L().Fatal("failed to start otel tracing", zap.Error(err))
		}
		defer func() {
			if err := shutdown(ctx); err != nil {
				zap.L().Error("failed to shutdown otel tracing", zap.Error(err))
			}
		}()

	}

	http.DefaultClient = hretrier.WrapHttpClientWithStandardRetries(http.DefaultClient)

	dpClient, err := orchestratordp.NewClientWithResponses(
		cfg.DataPlaneUrl,
		orchestratordp.WithHTTPClient(http.DefaultClient),
		orchestratordp.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			return nil
		}),
	)
	if err != nil {
		zap.L().Fatal("Failed to initialize data plane client", zap.Error(err))
	}

	iamClient, err := orchestratoriam.NewClientWithResponses(
		cfg.IamUrl,
		orchestratoriam.WithHTTPClient(http.DefaultClient),
		orchestratoriam.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("From", userid.InternalSystemUuid.String())
			return nil
		}),
	)
	if err != nil {
		zap.L().Fatal("Failed to initialize iam client", zap.Error(err))
	}

	dbConnStr := fmt.Sprintf(
		"dbname=%s user=%s password=%s host=%s port=%s connect_timeout=5 sslmode=disable",
		cfg.DatabaseName, cfg.DatabaseUser, cfg.DatabasePassword, cfg.DatabaseHost, cfg.DatabasePort)
	db, err := model.NewDatabaser(ctx, zap.L(), dbConnStr)
	if err != nil {
		zap.L().Fatal("Failed to initialize database", zap.Error(err))
	}
	defer func() {
		zap.L().Info("Closing database")
		if err := db.Close(); err != nil {
			zap.L().Error("failed to close database", zap.Error(err))
		}
		zap.L().Info("Database closed")
	}()

	amqpConnectionString, err := cfg.GetAmqpConnectionString()
	if err != nil {
		zap.L().Fatal("Failed to get AMQP connection string", zap.Error(err))
	}
	conn, err := rabbitmq.NewConn(amqpConnectionString, rabbitmq.WithConnectionOptionsLogger(hrabbitmq.NewLogger(zap.L())))
	if err != nil {
		zap.L().Fatal("Failed to initialize rabbitmq connection", zap.Error(err))
	}
	defer func() {
		if err := conn.Close(); err != nil {
			zap.L().Error("Failed to close connection", zap.Error(err))
		}
	}()

	delayqueues.DefaultExchange = events.DefaultExchange
	hstandardreliableoutbox.MessageIdPrefix = "platform-orchestrator-cp-"

	publisher, err := rabbitmq.NewPublisher(
		conn, rabbitmq.WithPublisherOptionsLogger(hrabbitmq.NewLogger(zap.L())),
		rabbitmq.WithPublisherOptionsExchangeName(events.DefaultExchange),
		rabbitmq.WithPublisherOptionsExchangeDeclare,
		rabbitmq.WithPublisherOptionsExchangeDurable,
		rabbitmq.WithPublisherOptionsExchangeKind("topic"),
		rabbitmq.WithPublisherOptionsLogger(hrabbitmq.NewLogger(zap.L())),
	)
	if err != nil {
		zap.L().Fatal("Failed to initialize rabbitmq publisher", zap.Error(err))
	}
	defer publisher.Close()
	publisher.NotifyPublish(func(p rabbitmq.Confirmation) {
		zap.L().Debug("message publish confirmation received", zap.Bool("ack", p.Ack))
	})

	vaultHttpClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
	hvaultapiClient, err := hvaultapi.NewWithDefaults(cfg.VaultURL, cfg.VaultAuth, cfg.VaultRole, vaultHttpClient, zap.L(), func(config *vaultapi.Config) {
		config.MaxRetries = 2
	})
	if err != nil {
		zap.L().Fatal("Failed to initialize vault client", zap.Error(err))
	}
	hvaultapiClient.WaitUntilReady(ctx)
	go hvaultapiClient.PeriodicallyRenewToken(ctx)
	vaultApiClient := hvaultapiClient.Client()

	server := api.Server{
		Database:          db,
		Logger:            zap.L(),
		Vault:             vault.NewVaultClient(vaultApiClient, zap.L()),
		RabbitMqPublisher: publisher,
		DpClient:          dpClient,
		IamClient:         iamClient,
	}

	echoServer, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{
		AppName:          path.Base(buildInfo.Main.Path),
		Logger:           server.Logger,
		OpenAPIRawSchema: api.MustDecodeOpenApiSpec(),
		Tracing:          hecho.TracingOTel,
	})

	echoServer.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{
		StackSize: 1 << 10,
		LogErrorFunc: func(c echo.Context, err error, stack []byte) error {
			server.Logger.Error("unexpected error", zap.Error(err))
			return err
		},
	}))
	if err != nil {
		zap.L().Fatal("Failed to setup server with schema validation", zap.Error(err))
	}

	echoServer.Use(apimiddleware.EchoCtxMiddleware)
	echoServer.Use(middleware.RequestID())

	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()
	go func() {
		zap.L().Info("Starting scheduled flush of pending messages")
		reliableoutbox.ScheduledFlushPendingMessages(bgCtx, server.Database.AsReliableOutboxStore(), publisher, reliableoutbox.DefaultScheduledFlushPeriodFunc)
		zap.L().Info("Stopped scheduled flush of pending messages")
	}()

	server.MapRoutes(echoServer)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDelay)
		defer cancel()

		// Gracefully shutdown the server by waiting on existing requests (except websockets).
		zap.L().Info("Gracefully shutting down webserver")
		if err := echoServer.Shutdown(ctx); err != nil {
			zap.L().Error("failed to gracefully shutdown webserver", zap.Error(err))
			if err := echoServer.Close(); err != nil {
				zap.L().Error("Failed to terminate the echo server", zap.Error(err))
			}
		} else {
			zap.L().Info("webserver shutdown")
		}
	}()

	// This cache is used by hrabbitmq library for messages de-duplication
	queueCache := expirable.NewLRU[string, int32](rabbitmqCacheSize, nil, rabbitmqCacheTtl)

	// Set up the delay queues which expire the messages after the N seconds delays and then sends the message back to the common exchange
	if err := delayqueues.SetupStandardDelayConsumers(conn, zap.L().With(zap.String("consumer", "delay"))); err != nil {
		zap.L().Fatal("Failed to setup delay queues", zap.Error(err))
	}

	// Set up the dead letter queue and consumer which pushes the messages onto the delay queues and exponential backoff.
	dlc, err := delayqueues.SetupStandardDeadLetterConsumer(conn, zap.L().With(zap.String("consumer", "dead-letters")), publisher, queueCache)
	if err != nil {
		zap.L().Fatal("Failed to setup dead letter queue", zap.Error(err))
	}
	defer func() {
		zap.L().Info("Shutting down dead letter queue consumer")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		dlc.CloseWithContext(ctx)
		zap.L().Info("Dead letter queue consumer shutdown")
	}()

	wrk := &worker.Worker{
		RabbitConn:      conn,
		RabbitPublisher: publisher,
		Cache:           queueCache,
		Logger:          zap.L().Named("worker"),

		DB: db,

		IamClient: iamClient,
		DpClient:  dpClient,
	}
	wrkc, err := wrk.BuildMainConsumer()
	if err != nil {
		zap.L().Fatal("failed to setup main consumer", zap.Error(err))
	}
	defer func() {
		zap.L().Info("Shutting down main consumer")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		wrkc.CloseWithContext(ctx)
		zap.L().Info("Main consumer shutdown")
	}()

	errChan := make(chan error)

	// Start HTTP server.
	go func() {
		addr := fmt.Sprintf(":%d", cfg.Port)
		zap.L().Info("Starting server", zap.String("addr", addr))
		if err := echoServer.Start(addr); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				errChan <- errors.Wrap(err, "failed to start server")
			}
		}
	}()

	go func() {
		zap.L().Info("Starting dead letter queue consumer")
		if err := dlc.Run(); err != nil && !errors.Is(err, context.Canceled) {
			errChan <- errors.Wrap(err, "failed to run dead letter queue consumer")
		}
	}()

	go func() {
		zap.L().Info("Starting worker consumer")
		if err := wrkc.Run(); err != nil && !errors.Is(err, context.Canceled) {
			errChan <- errors.Wrap(err, "failed to run main queue consumer")
		}
	}()

	exit := make(chan os.Signal, 1) // we need to reserve to buffer size 1, so the notifier are not blocked
	signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-exit:
		zap.L().Info("Signal caught", zap.String("signal", sig.String()))
		time.Sleep(cfg.ShutdownDelay)
	case ec := <-errChan:
		zap.L().Error("Critical error received from background component", zap.Error(ec))
	}

	// drain the rest of the error channel
	go func() {
		for e := range errChan {
			zap.L().Error("Error received from background component", zap.Error(e))
		}
	}()
}
