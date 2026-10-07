DP_IMAGE ?= ghcr.io/stellwerk-labs/platform-orchestrator-dp:v2.0.0@sha256:275947e6176d45df02979d7d600e7ebb096561e46cd577049761e24e0a8ec9c3
IAM_IMAGE ?= ghcr.io/stellwerk-labs/platform-orchestrator-iam@sha256:0fb4b3645ae2d7ccf366921c8d9490a6862cce111bdcb683ef6afcc852b40331
# nats:2.12.15-alpine (multi-platform index), the disposable JetStream of test-integration-postgres.
NATS_IMAGE ?= nats@sha256:b270f5e2428354c0335612694d7dd2fb588148e567a5757fdff325ef9c9332e6

compose.yaml: BUILD_ARGS := {"context":"../"}
compose.yaml: ../score.yaml Makefile compose-mixins.yaml score.provisioners.yaml score.dp.yaml score.iam.yaml
	score-compose check-version '>=0.16.0'
	score-compose init --no-sample
	cp score.provisioners.yaml .score-compose/
	cp score.dp.yaml score.iam.yaml .score-compose/
	yq -i '.containers.platform-orchestrator-dp.image = "$(DP_IMAGE)"' .score-compose/score.dp.yaml
	yq -i '.containers.platform-orchestrator-iam.image = "$(IAM_IMAGE)"' .score-compose/score.iam.yaml
	score-compose generate ../score.yaml --build 'platform-orchestrator-cp=$(BUILD_ARGS)'
	score-compose generate .score-compose/score.dp.yaml
	score-compose generate .score-compose/score.iam.yaml
	score-compose generate --publish 5432:postgres.default#platform-orchestrator-cp.postgres.host:5432 --publish 8081:platform-orchestrator-cp:8080 --publish 8082:platform-orchestrator-dp:8080 --publish 8083:platform-orchestrator-iam:8080
	yq eval-all '. as $$item ireduce ({}; . * $$item)' --inplace compose.yaml compose-mixins.yaml

.PHONY: build
build: compose.yaml
	docker compose build --pull

.PHONY: test
test: compose.yaml test-startup
	@DB_CONNECTION_STRING="$$(score-compose resources get-outputs 'postgres.default#platform-orchestrator-cp.postgres' | jq -r '"postgres://\(.username):\(.password)@127.0.0.1:5432/\(.name)?sslmode=disable"')" \
	SERVER_URL=http://$(shell score-compose resources get-outputs dns.default\#api-dns | jq -r .host):8080 \
	INTERNAL_SERVER_URL=http://localhost:8081 \
	INTERNAL_IAM_URL=http://localhost:8083 \
	VAULT_URL=http://localhost:8200 VAULT_TOKEN=mytoken \
	TEST_USER_IDENTITY_RECIPIENT=age10hmkvsqfxr5ha05ay6pqj9zdtal7pw7x2r3t4n00rzucc3en3u9q3cycqm \
	go tool gotestsum --format testname ./ -test.timeout 10m -test.parallel=2 -test.v $(TESTARGS)

## Verify IAM can bootstrap an empty event transport before CP starts.
.PHONY: test-iam-startup
test-iam-startup: compose.yaml
	docker compose up --no-build -d platform-orchestrator-iam-platform-orchestrator-iam --wait --wait-timeout 60
	timeout 60s sh -c 'until curl --fail --silent --output /dev/null http://127.0.0.1:8083/health; do sleep 1; done'

## Verify the complete API fixture before running tests against it.
.PHONY: test-startup
test-startup: test-iam-startup
	docker compose up --no-build -d platform-orchestrator-cp-platform-orchestrator-cp runner-gateway --wait --wait-timeout 90
	timeout 60s sh -c 'until curl --fail --silent --output /dev/null http://127.0.0.1:8081/health && curl --fail --silent --output /dev/null http://127.0.0.1:8082/health; do sleep 1; done'

## Run -tags integration Go package tests sequentially against the compose stack's PostgreSQL.
## PLUGIN_TEST_DATABASE_URL is keyword=value, because the tests append a search_path to it.
## Starts a dedicated disposable NATS on 127.0.0.1:14222 for the events package and removes it after.
.PHONY: test-integration-postgres
test-integration-postgres: compose.yaml test-startup
	@set -e; \
	nats_container=cp-integ-nats-events-$$$$; \
	trap 'docker stop "$$nats_container" >/dev/null 2>&1 || true' EXIT; \
	docker run -d --rm --name "$$nats_container" -p 127.0.0.1:14222:4222 -p 127.0.0.1:18222:8222 \
	  $(NATS_IMAGE) -js -m 8222 >/dev/null; \
	timeout 30s sh -c 'until curl --fail --silent --output /dev/null http://127.0.0.1:18222/healthz; do sleep 1; done'; \
	PLUGIN_TEST_DATABASE_URL="$$(score-compose resources get-outputs 'postgres.default#platform-orchestrator-cp.postgres' | \
	  jq -r '"host=127.0.0.1 port=5432 user=\(.username) password=\(.password) dbname=\(.name) sslmode=disable"')"; \
	PLUGIN_TEST_DATABASE_URL="$$PLUGIN_TEST_DATABASE_URL" \
	PLUGIN_EVENTS_TEST_NATS_URL=nats://127.0.0.1:14222 \
	PLUGIN_EVENTS_TEST_DISPOSABLE=1 \
	go test -tags=integration -count=1 -p 1 -timeout 5m \
	  ../internal/model/... ../internal/api/... ../internal/pluginmanager/... ../cmd/plugin-entitlement-dev/... ../internal/events/...

.PHONY: clean
clean:
	if [ -f compose.yaml ]; then docker compose down -v --remove-orphans; fi
	rm -rfv .score-compose compose.yaml

.PHONY: test-integration-logs
test-integration-logs:
	docker compose logs

.PHONY: connect-to-integ-db
connect-to-integ-db:
	@score-compose resources get-outputs 'postgres.default#platform-orchestrator-cp.postgres' \
		--format 'Command: docker run -it --network integration-tests_default --rm postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea psql -h {{ .host }} -U {{ .username }} --dbname {{ .name }}'
	@score-compose resources get-outputs 'postgres.default#platform-orchestrator-cp.postgres' --format 'Password: {{ .password }}'
