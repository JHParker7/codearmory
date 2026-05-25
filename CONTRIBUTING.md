# Contributing to CodeArmory

Thanks for your interest in contributing. This document covers how to get set up, the branching model, and what we expect from pull requests.

## Prerequisites

- Go 1.25+
- Docker and Docker Compose
- Python 3.12+ (integration tests)
- [pre-commit](https://pre-commit.com/)

## Local Setup

```bash
# Install pre-commit hooks (commit-message linting + secret scanning)
pip install pre-commit
pre-commit install --hook-type commit-msg

# Start the full stack
cd infra/local
docker compose up --build
```

Services will be available at:

| Service    | Port |
|------------|------|
| Gatekeeper | 8080 |
| Blueprints | 8081 |
| Conductor  | 8082 |

## Running Tests

**Unit tests** — run inside each service directory:

```bash
cd src/systems/gatekeeper && go test .
cd src/systems/blueprints  && go test .
cd src/systems/conductor   && go test .
cd src/cli                 && go test .
```

**Integration tests** — requires the compose stack to be running:

```bash
pip install -r tests/gatekeeper/requirements.txt \
            -r tests/blueprints/requirements.txt  \
            -r tests/conductor/requirements.txt   \
            -r tests/cli/requirements.txt

pytest tests/gatekeeper tests/blueprints tests/conductor tests/cli -v
```

## Branching and Pull Requests

- Branch off `main` for every change.
- Keep PRs focused — one logical change per PR.
- Open a draft PR early if you want feedback before the work is complete.
- All PRs require at least one approving review and must pass CI before merging.

## Commit Messages

We use [Conventional Commits](https://www.conventionalcommits.org/). The pre-commit hook enforces this automatically.

```
<type>(<scope>): <short description>

Types: feat, fix, docs, style, refactor, test, ci, chore
```

Examples:

```
feat(gatekeeper): add TOTP support
fix(blueprints): handle empty state on first lock
docs: update CLI installation instructions
```

## Reporting Issues

Use the GitHub issue templates — bug reports and feature requests each have a form to fill in.

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Please read it before participating.
