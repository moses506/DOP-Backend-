-include backend/.env
export

.PHONY: setup db api web test test-api test-web admin import-clients import-suppliers build lint

## First-time setup: Go modules, npm packages, database
setup:
	cd backend && go mod tidy
	cd frontend && npm install
	docker compose up -d --wait db mail

db:
	docker compose up -d --wait db mail

## Run the API on :8080 (applies migrations on start)
api:
	cd backend && go run ./cmd/server

## Run the web app on :5173 (proxies /api to :8080)
web:
	cd frontend && npm run dev

## Create a user: make admin EMAIL=you@dos.co.zm NAME="Your Name" POSITION="Developer" DEPT=IT ROLE=admin
admin:
	cd backend && go run ./cmd/admin create-user -email "$(EMAIL)" -name "$(NAME)" \
		-position "$(POSITION)" -department "$(DEPT)" -role "$(or $(ROLE),admin)"

## Import clients cleaned from the sales master list (DRY=1 to preview only):
##   make import-clients AS=you@dos.co.zm [FILE=scripts/import/data/clients-import.json] [DRY=1]
import-clients:
	cd backend && go run ./cmd/admin import-clients -as "$(AS)" \
		-file "$(or $(FILE),scripts/import/data/clients-import.json)" $(if $(DRY),-dry-run,)

## Load the suppliers from the sourcing log book (safe to re-run: existing ones are skipped):
##   make import-suppliers AS=you@dos.co.zm [DRY=1]
import-suppliers:
	cd backend && go run ./cmd/admin import-suppliers -as "$(AS)" \
		-file "$(or $(FILE),scripts/import/suppliers-import.json)" $(if $(DRY),-dry-run,)

test: test-api test-web

## Unit tests always run; integration tests run when TEST_DATABASE_URL is set
test-api:
	cd backend && go vet ./... && go test -race ./...

test-web:
	cd frontend && npm run typecheck

lint:
	cd backend && gofmt -l . && go vet ./...

build:
	docker build -t dos-bpas .
