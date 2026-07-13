compose.yaml: BUILD_ARGS := {"context":"../"}
compose.yaml: ../score.yaml Makefile compose-mixins.yaml score.provisioners.yaml
	score-compose check-version '>=0.16.0'
	score-compose init --no-sample
	cp score.provisioners.yaml .score-compose/
	score-compose generate ../score.yaml --build 'platform-orchestrator-cp=$(BUILD_ARGS)'
	score-compose generate ./score.dp.yaml
	score-compose generate ./score.iam.yaml
	score-compose generate --publish 5432:postgres.default#platform-orchestrator-cp.postgres.host:5432 --publish 8081:platform-orchestrator-cp:8080 --publish 8083:platform-orchestrator-iam:8080
	yq eval-all '. as $$item ireduce ({}; . * $$item)' --inplace compose.yaml compose-mixins.yaml

.PHONY: build
build: compose.yaml
	docker compose build --pull

.PHONY: test
test: compose.yaml
	docker compose up --no-build -d platform-orchestrator-cp-platform-orchestrator-cp platform-orchestrator-dp-platform-orchestrator-dp --wait
	SERVER_URL=http://$(shell score-compose resources get-outputs dns.default\#api-dns | jq -r .host):8080 \
	INTERNAL_SERVER_URL=http://localhost:8081 \
	INTERNAL_IAM_URL=http://localhost:8083 \
	VAULT_URL=http://localhost:8200 VAULT_TOKEN=mytoken \
	TEST_USER_IDENTITY_RECIPIENT=age10hmkvsqfxr5ha05ay6pqj9zdtal7pw7x2r3t4n00rzucc3en3u9q3cycqm \
	go tool gotestsum --format testname ./ -test.timeout 10m -test.parallel=2 -test.v $(TESTARGS)

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
		--format 'Command: docker run -it --network integration-tests_default --rm postgres:16-alpine psql -h {{ .host }} -U {{ .username }} --dbname {{ .name }}'
	@score-compose resources get-outputs 'postgres.default#platform-orchestrator-cp.postgres' --format 'Password: {{ .password }}'
