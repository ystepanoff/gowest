# gowest developer tasks. Run `make help` for a summary.

.PHONY: help test race vet bench-compile bench check autobahn autobahn-down clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

test: ## Run the test suite
	go test ./...

race: ## Run the test suite under the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

bench-compile: ## Compile (but do not run) both benchmark suites
	go test -run='^$$' -bench='^$$' ./...
	cd benchmarks && go vet ./... && go test -run='^$$' -bench='^$$' ./...

bench: ## Run the cross-library echo benchmarks
	cd benchmarks && go test -run='^$$' -bench=BenchmarkEcho -benchmem -count=6 .

check: vet test race bench-compile ## Run everything CI runs

autobahn: ## Run the Autobahn conformance suite (requires Docker)
	cd autobahn && docker compose up --build --abort-on-container-exit
	@echo "Report written to autobahn/report/index.html"

autobahn-down: ## Tear down the Autobahn containers
	cd autobahn && docker compose down

clean: ## Remove generated artefacts
	rm -rf autobahn/report
	go clean ./...
