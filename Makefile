# GoFiber CRM Backend Makefile

.PHONY: help dev run build test test-cover test-integration test-api clean lint deps docker-build docker-run fmt check install-tools

# Default target
help:
	@echo "Available commands:"
	@echo "  dev              - Run with hot reload (requires air)"
	@echo "  run              - Run the application"
	@echo "  run-simple       - Run without building binary"
	@echo "  build            - Build the application"
	@echo "  test             - Run unit tests"
	@echo "  test-cover       - Run tests with coverage report"
	@echo "  test-integration - Run integration tests"
	@echo "  test-api         - Run API tests using bash script"
	@echo "  clean            - Clean build artifacts"
	@echo "  lint             - Run linting"
	@echo "  fmt              - Format code"
	@echo "  deps             - Install/update dependencies"
	@echo "  check            - Run all checks (fmt, lint, test, build)"
	@echo "  install-tools    - Install development tools"
	@echo "  swagger          - Generate Swagger documentation"
	@echo "  seed             - Seed database with test data"
	@echo "  seed-clean       - Remove all test data from database"

# Build the application
build:
	@echo "Building application..."
	@mkdir -p bin
	go build -o bin/server cmd/server/main.go
	@echo "Build complete: bin/server"

# Run the application
run: build
	@echo "Starting application..."
	./bin/server

# Run without building binary first
run-simple:
	@echo "Starting application (simple mode)..."
	go run cmd/server/main.go

# Development with hot reload
dev:
	@echo "Starting development server with hot reload..."
	@if command -v air > /dev/null 2>&1; then \
		air; \
	else \
		echo "Air not found. Installing..."; \
		go install github.com/air-verse/air@latest; \
		air; \
	fi

# Run unit tests
test:
	@echo "Running unit tests..."
	@echo "Note: If you encounter 'snap-confine has elevated permissions' error,"
	@echo "this is a system-level snap security issue, not a code problem."
	@echo "Alternative testing methods:"
	@echo "  - Use 'make test-api' for comprehensive API testing"
	@echo "  - Import the Postman collection from postman/ directory"
	@echo "  - Use manual testing with curl (see docs/TESTING.md)"
	@echo "  - Install Go via apt instead of snap to resolve the issue"
	@echo ""
	go test -v ./internal/...

# Run tests with coverage
test-cover:
	@echo "Running tests with coverage..."
	go test -v -coverprofile=coverage.out ./internal/...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Run integration tests
test-integration:
	@echo "Running integration tests..."
	go test -v ./tests/...

# Run API tests using bash script
test-api:
	@echo "Running API tests..."
	@if [ ! -f scripts/test_api.sh ]; then \
		echo "Test script not found: scripts/test_api.sh"; \
		exit 1; \
	fi
	@chmod +x scripts/test_api.sh
	@echo "Make sure the server is running on localhost:8080"
	@echo "You can start it with: make run"
	@read -p "Press Enter to continue when server is ready..."
	@./scripts/test_api.sh

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	rm -rf tmp/
	rm -f coverage.out coverage.html
	@echo "Clean complete"

# Run linting
lint:
	@echo "Running linting..."
	@if command -v golangci-lint > /dev/null 2>&1; then \
		golangci-lint run; \
	elif command -v golint > /dev/null 2>&1; then \
		golint ./...; \
	else \
		echo "No linter found. Installing golangci-lint..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
		golangci-lint run; \
	fi

# Format code
fmt:
	@echo "Formatting code..."
	go fmt ./...
	@echo "Code formatted"

# Install/update dependencies
deps:
	@echo "Installing/updating dependencies..."
	go mod tidy
	go mod download
	@echo "Dependencies updated"

# Generate Swagger documentation
swagger:
	@echo "Generating Swagger documentation..."
	@if command -v swag > /dev/null 2>&1; then \
		swag init -g cmd/server/main.go; \
		echo "Swagger documentation generated"; \
	else \
		echo "swag not found. Installing..."; \
		go install github.com/swaggo/swag/cmd/swag@latest; \
		swag init -g cmd/server/main.go; \
		echo "Swagger documentation generated"; \
	fi

# Install development tools
install-tools:
	@echo "Installing development tools..."
	go install github.com/air-verse/air@latest
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install github.com/swaggo/swag/cmd/swag@latest
	@echo "Development tools installed"

# Run all checks (format, lint, test, build)
check: fmt lint test build
	@echo "All checks passed!"

# Docker build
docker-build:
	@echo "Building Docker image..."
	docker build -t gofiber-crm-backend .

# Docker run
docker-run:
	@echo "Running with Docker..."
	docker-compose up --build

# Database migrations (if you have migration tools)
migrate-up:
	@echo "Running database migrations..."
	@if [ -f "scripts/migrate.go" ]; then \
		go run scripts/migrate.go up; \
	else \
		echo "Migration script not found. Using model auto-migration."; \
		go run cmd/migrate/main.go; \
	fi

migrate-down:
	@echo "Rolling back database migrations..."
	@if [ -f "scripts/migrate.go" ]; then \
		go run scripts/migrate.go down; \
	else \
		echo "Migration rollback script not found"; \
	fi

# Seed database with test data
seed:
	@echo "Seeding database with test data..."
	@./scripts/seed.sh

# Clean test data from database
seed-clean:
	@echo "Removing test data from database..."
	@./scripts/seed.sh clean
