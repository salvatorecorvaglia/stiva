# Contributing to Stiva 🖼️

Thank you for your interest in contributing to **Stiva**! We welcome contributions, bug reports, feature requests, and security improvements from the community.

---

## 💡 How Can I Contribute?

### Reporting Bugs

If you find a bug in Stiva, please open a GitHub Issue with the following details:
1. **Clear title** and description of the issue.
2. **Steps to reproduce** the bug.
3. **Expected vs. actual behavior**.
4. **Environment details** (Go version, OS, Docker version, client S3 SDK used).
5. Relevant logs or error messages (with sensitive keys redacted).

### Suggesting Enhancements

We are always looking for ways to make Stiva better! If you have a feature request or enhancement idea:
1. Search the open issues to see if the feature has already been suggested.
2. Open a new issue detailing your proposal, explaining the use case and how it benefits the project.
3. Keep in mind Stiva's goal of being a lightweight, high-performance, and self-hosted S3-compatible storage engine.

---

## 💻 Setting Up Your Development Environment

### Prerequisites

- **Go 1.26 or higher** (Required to compile and run the project)
- **Docker & Docker Compose** (Optional, for building/testing containerized builds)
- **golangci-lint** (For running lint checks locally)
- **AWS CLI / Boto3** (Optional, for S3 API compatibility verification)

### Running Locally

1. **Fork and Clone the Repository**
   Fork the repository on GitHub and clone it to your local machine:
   ```bash
   git clone https://github.com/<your-username>/stiva.git
   cd stiva
   ```

2. **Configure Environment Variables**
   Create a local configuration file by copying the template file:
   ```bash
   cp .env.example .env
   ```
   Modify the `.env` variables if you need to run ports on different addresses or change the default storage directory (`STIVA_DATA_DIR`).

3. **Build the Server**
   To verify that everything compiles correctly, build the binary:
   ```bash
   go build -o stiva ./cmd/stiva
   ```

4. **Run the Server**
   Load your environment variables and start Stiva:
   ```bash
   export $(grep -v '^#' .env | xargs) && ./stiva
   ```
   *Alternatively, run and compile in one step:*
   ```bash
   export $(grep -v '^#' .env | xargs) && go run ./cmd/stiva
   ```
   The S3 API will be available at `http://localhost:9000` and the web console at `http://localhost:9001`.

### Running Tests

Stiva has unit, integration, and fuzz tests living alongside the code they cover, as in-package `_test.go` files under `internal/` (`internal/auth`, `internal/config`, `internal/console`, `internal/httpx`, `internal/s3api`, `internal/server`, `internal/storage`). Keeping them in-package means internal helpers can be tested directly, without exporting them purely for tests. Make sure all tests pass before submitting a pull request:
```bash
# Run all tests in the workspace
go test -v ./...

# Run tests with race detector (used in CI)
go test -race ./...

# Run tests with coverage report
go test -race -coverprofile=coverage.out -covermode=atomic ./...

# View coverage in HTML format
go tool cover -html=coverage.out -o coverage.html
```

### Linting

Before pushing your changes, run `golangci-lint` to check code quality. We enforce strict linting rules on every pull request:
```bash
# Install golangci-lint (if not already installed)
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Run the linter
golangci-lint run
```
Lint rules are defined in [`.golangci.yml`](.golangci.yml).

---

## 🚀 Submitting Your Changes

### Branch Naming

Use clear branch names prefixing the type of changes you are making:
- `feature/` for new features (e.g., `feature/lifecycle-rules`)
- `bugfix/` for bug fixes (e.g., `bugfix/multipart-part-size`)
- `docs/` for documentation updates (e.g., `docs/add-contributing-guide`)
- `refactor/` for code refactoring
- `test/` for testing additions

### Commit Message Guidelines

We prefer clean and descriptive commit messages following the [Conventional Commits](https://www.conventionalcommits.org/) specification:
```
<type>(<scope>): <short description>

[Optional longer body describing details]
```
Common types include:
- `feat`: A new feature (e.g., `feat(s3api): add support for S3 Lifecycle policies`)
- `fix`: A bug fix (e.g., `fix(storage): resolve memory leak in multipart upload`)
- `docs`: Documentation updates (e.g., `docs: update setup instructions in README`)
- `test`: Add or modify tests (e.g., `test: add tests for path traversal protection`)
- `refactor`: Code changes that neither fix a bug nor add a feature (e.g., `refactor: simplify metadata store locking`)
- `perf`: Performance optimizations (e.g., `perf: optimize streaming I/O buffer allocation`)
- `ci`: CI pipeline updates (e.g., `ci: update golangci-lint version`)

> **Why this matters**: The release workflow uses [GoReleaser](https://goreleaser.com/) to auto-generate changelogs grouped by commit type (`feat`, `fix`, `docs`, `test`, `refactor`, `perf`) and publish binary releases to GitHub, as well as pushing multi-architecture Docker images to Docker Hub. Commits prefixed with `chore:`, `ci:`, or `style:` are excluded from release notes.

### Pull Request Process

1. Fork the repository and create your branch:
   ```bash
   git checkout -b feature/your-feature-name
   ```
2. Implement your changes, ensuring you write relevant unit/integration tests.
3. Push your branch to your fork:
   ```bash
   git push origin feature/your-feature-name
   ```
4. Open a Pull Request against the `main` branch of the `salvatorecorvaglia/stiva` repository.
5. Fill out the pull request template completely, detailing:
   - What problem is solved by the PR.
   - The approach taken.
   - Verification and manual testing steps.
6. Address any feedback during code review and update the branch as needed.

---

Happy coding! 🖼️
