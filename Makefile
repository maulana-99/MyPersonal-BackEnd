# MyPersonal backend — task aliases.
# Run `make` (or `make help`) to see everything.
#
# Secrets come from the repo-root .env, which reaches this directory through the
# `MyPersonal-BackEnd/.env -> ../.env` symlink. `go run` picks that up itself via
# godotenv, but psql and the seed script need it here, so it is parsed once below.
#
#   make run        API + reminder scheduler + Asynq worker   (needs Redis)
#   make run-api    API only                                  (no Redis needed)
#   make test       integration tests against the test database
#   make verify     the full gate: fmt + build + vet + test

SHELL := /bin/bash

ENV_FILE := .env
PORT     ?= 8080

# Parsed from .env so no export is required in the calling shell.
DATABASE_URL ?= $(shell sed -n 's|^DATABASE_URL=||p' $(ENV_FILE))
PGPASSWORD   ?= $(shell sed -n 's|^POSTGRES_PASSWORD=||p' $(ENV_FILE))
export PGPASSWORD

# Integration tests need a SEPARATE database. This matters: with
# TEST_DATABASE_URL unset, TestMain skips every DB test and `go test` still
# reports "ok" — a silent green. Deriving it here removes that trap.
TEST_DATABASE_URL ?= $(subst /daily_planner?,/daily_planner_test?,$(DATABASE_URL))
PG_MAINT_URL      := $(subst /daily_planner?,/postgres?,$(DATABASE_URL))

APP_BASE_URL ?= http://127.0.0.1:$(PORT)/api/v1

BIN := bin/server

# `make` with no argument documents the aliases rather than guessing a target.
.DEFAULT_GOAL := help

.PHONY: run run-api build test test-db migrate seed verify fmt vet clean help

run: ## API + reminder scheduler + Asynq worker (needs Redis)
	PORT=$(PORT) go run ./cmd/server

run-api: ## API only — no Redis, no worker (what the e2e suite uses)
	ENABLE_WORKER=false PORT=$(PORT) go run ./cmd/server

build: ## Compile the server to bin/server
	go build -o $(BIN) ./cmd/server

test: test-db ## Integration tests against the test database
	@test -n "$(TEST_DATABASE_URL)" \
		|| { echo "TEST_DATABASE_URL is empty — is $(ENV_FILE) missing DATABASE_URL?"; exit 1; }
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -count=1 ./...

test-db: ## Create the test database if it does not exist
	@psql '$(PG_MAINT_URL)' -tAc \
		"SELECT 1 FROM pg_database WHERE datname = 'daily_planner_test'" | grep -q 1 \
		|| psql '$(PG_MAINT_URL)' -c 'CREATE DATABASE daily_planner_test'

migrate: ## Apply migrations/*.sql, in order, to the dev database
	@for f in migrations/*.sql; do \
		printf '  → %s\n' "$$f"; \
		psql '$(DATABASE_URL)' -v ON_ERROR_STOP=1 -q -f "$$f" || exit 1; \
	done
	@echo "migrations applied"

seed: ## Seed the demo persona (needs `make run` in another terminal)
	@set -a && . ./$(ENV_FILE) && set +a && \
		APP_BASE_URL='$(APP_BASE_URL)' python3 ../scripts/seed_dummy.py $(ARGS)

verify: ## The full pre-commit gate: fmt check + build + vet + test
	@out=$$(gofmt -l .); test -z "$$out" \
		|| { printf 'gofmt needed:\n%s\n' "$$out"; exit 1; }
	go build ./...
	go vet ./...
	@$(MAKE) --no-print-directory test

fmt: ## Format the tree
	gofmt -w .

vet: ## go vet ./...
	go vet ./...

clean: ## Remove build output
	rm -rf bin

help: ## List the available targets
	@printf '\nMyPersonal backend — targets:\n\n'
	@grep -hE '^[a-z][a-z-]*:.*## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
	@printf '\n'
