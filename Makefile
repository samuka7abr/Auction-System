# .env carries the credentials the compose interpolates. The Makefile reads the
# same file so that host-side targets talk to the same database, without any
# literal password living in a versioned file.
-include .env
export

AUCTIONS      ?= 1
ENDS_IN       ?= 5m
MIN_INCREMENT ?= 100
STARTING_BID  ?= 0
OUT           ?= bench/auctions.json
STRATEGY      ?= optimistic
POSTGRES_PORT ?= 5432

# postgres:5432 resolves inside the compose network; localhost:$(POSTGRES_PORT)
# from the host, which is where cmd/seed runs.
COMPOSE_DB_URL = postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres:5432/$(POSTGRES_DB)?sslmode=disable
HOST_DB_URL    = postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable

# RUN names the cell's artefact directory. Recursively expanded, so `date` only
# runs when a bench or check target actually asks for it.
RUN      ?= $(shell date -u +%Y%m%dT%H%M%S)
POLICY   ?= immediate
SCENARIO ?= smoke
# Loose enough that nothing closes mid-cell: an auction dying under load mixes
# contention with the closing edge in one number (RF03). Separate from ENDS_IN
# so that a plain `make seed` keeps its own default.
BENCH_ENDS_IN ?= 30m

SEED_SOURCES    = $(shell find cmd/seed internal -name '*.go') go.mod
CHECKER_SOURCES = $(shell find cmd/checker -name '*.go') go.mod

.PHONY: up down migrate migrate-down seed run logs test fmt lint bench check chaos matrix

up:
	docker compose up -d --build

down:
	docker compose down -v --remove-orphans

migrate:
	docker compose run --rm migrate -path=/migrations -database="$(COMPOSE_DB_URL)" up

migrate-down:
	docker compose run --rm migrate -path=/migrations -database="$(COMPOSE_DB_URL)" down -all

bin/seed: $(SEED_SOURCES)
	go build -o bin/seed ./cmd/seed

# Seeding must be negligible next to a benchmark cell, so the binary is built
# once and reused instead of going through `go run` on every call.
seed: bin/seed
	DATABASE_URL="$(HOST_DB_URL)" ./bin/seed \
		-auctions=$(AUCTIONS) \
		-ends-in=$(ENDS_IN) \
		-min-increment=$(MIN_INCREMENT) \
		-starting-bid=$(STARTING_BID) \
		$(if $(TRUNCATE),-truncate,) \
		-out=$(OUT)

run:
	STRATEGY=$(STRATEGY) docker compose up -d --build auctiond

bin/checker: $(CHECKER_SOURCES)
	go build -o bin/checker ./cmd/checker

# One cell, end to end, exiting with the checker's code so that the loop of
# etapa 5 can stop on it.
bench:
	RUN=$(RUN) AUCTIONS=$(AUCTIONS) POLICY=$(POLICY) SCENARIO=$(SCENARIO) \
	STRATEGY=$(STRATEGY) ENDS_IN=$(BENCH_ENDS_IN) MIN_INCREMENT=$(MIN_INCREMENT) \
	DATABASE_URL="$(HOST_DB_URL)" bench/run-cell.sh

# Re-verifies a cell already on disk, which is what makes a planted violation
# testable without re-running the load.
check: bin/checker
	DATABASE_URL="$(HOST_DB_URL)" ./bin/checker -run=$(RUN)

# The five chaos cells. One line, because the loop lives in the script and not
# here: make collapses any recipe failure into its own exit code, 2, which would
# erase the checker's difference between a violated invariant (1) and a cell that
# could not be verified (2). This target is for a person; automation calls
# chaos/run-all.sh directly (decisão 93).
chaos:
	chaos/run-all.sh

# The 36 cells and the control, ~1h30. One line, and for the same reason as
# `make chaos`: the loop lives in the script because make collapses every exit
# code into its own 2, which is exactly the difference the matrix needs to keep
# between a violated invariant (1), a cell it could not verify (2) and a
# breached threshold (99) — decisão 93. `make matrix RESUME=1` and
# `make matrix MATRIX=...` work through the export at the top of this file.
matrix:
	bench/run-matrix.sh

logs:
	docker compose logs -f

test:
	go test ./...

fmt:
	gofmt -w .

lint:
	go vet ./...
