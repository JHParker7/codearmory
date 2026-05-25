# Armory CLI

Command-line client for the CodeArmory platform. All commands talk to [Conductor](../conductor/README.md), which routes requests to Gatekeeper and Blueprints.

## Installation

### Download a release binary

Pre-built binaries are attached to every [GitHub release](../../../releases):

| Platform | File |
|----------|------|
| Linux x86-64 | `armory_linux_amd64` |
| Linux ARM64 | `armory_linux_arm64` |
| macOS (Intel) | `armory_darwin_amd64` |
| macOS (Apple Silicon) | `armory_darwin_arm64` |
| Windows x86-64 | `armory_windows_amd64.exe` |

```bash
# Example — Linux x86-64
curl -L -o armory https://github.com/code-armory-app/codearmory/releases/latest/download/armory_linux_amd64
chmod +x armory
sudo mv armory /usr/local/bin/
```

### Build from source

```bash
cd src/cli
go build -o armory .
```

## Configuration

The CLI resolves the Conductor URL and auth token in priority order:

**URL** (highest to lowest):
1. `--url` flag
2. `CODEARMORY_URL` environment variable
3. `~/.config/codearmory/config.json`

**Token** (highest to lowest):
1. `--token` flag
2. `CODEARMORY_TOKEN` environment variable
3. OS keychain (`codearmory` / `token`)
4. `~/.config/codearmory/config.json` (plaintext fallback)

```bash
# Point at a non-default conductor
export CODEARMORY_URL=https://conductor.example.com

# Or pass per-command
armory --url https://conductor.example.com auth status
```

## Quick Start

```bash
# Create an account and log in
armory auth signup --email alice@example.com --username alice --password hunter2
armory auth login --email alice@example.com --password hunter2

# Check auth state
armory auth status

# Create an org and invite a colleague
armory orgs create --name acme
armory orgs invite <org-id> --email bob@example.com

# Manage Terraform state workspaces
armory state get alice dev
armory state lock alice dev
armory state unlock alice dev --data '{"ID":"abc123"}'
```

## Command Reference

### `auth`

| Command | Description |
|---------|-------------|
| `auth login` | Authenticate and save token |
| `auth signup` | Register a new account |
| `auth logout` | Clear saved token from keychain and config |
| `auth status` | Show current auth configuration |

### `users`

| Command | Description |
|---------|-------------|
| `users list` | List all users |
| `users get <id>` | Get a user by ID |
| `users update <id>` | Update a user |
| `users delete <id>` | Delete a user |

### `orgs`

| Command | Description |
|---------|-------------|
| `orgs list` | List all organizations |
| `orgs get <id>` | Get an organization by ID |
| `orgs create` | Create an organization |
| `orgs update <id>` | Update an organization |
| `orgs invite <id>` | Send an invite for an organization |
| `orgs delete <id>` | Delete an organization |

### `teams`

| Command | Description |
|---------|-------------|
| `teams list` | List all teams |
| `teams get <id>` | Get a team by ID |
| `teams create` | Create a team |
| `teams update <id>` | Update a team |
| `teams invite <id>` | Send an invite for a team |
| `teams delete <id>` | Delete a team |

### `roles`

| Command | Description |
|---------|-------------|
| `roles get <id>` | Get a role by ID |
| `roles create` | Create a role |
| `roles update <id>` | Update a role |
| `roles delete <id>` | Delete a role |

### `permissions`

| Command | Description |
|---------|-------------|
| `permissions get <id>` | Get a permission by ID |
| `permissions create` | Create a permission |
| `permissions update <id>` | Update a permission |
| `permissions delete <id>` | Delete a permission |

### `sessions`

| Command | Description |
|---------|-------------|
| `sessions get <id>` | Get a session by ID |
| `sessions delete <id>` | Revoke a session |

### `invites`

| Command | Description |
|---------|-------------|
| `invites list` | List all invites |
| `invites get <id>` | Get an invite by ID |
| `invites accept <id>` | Accept an invite |
| `invites decline <id>` | Decline an invite |
| `invites delete <id>` | Revoke an invite |

### `state` (user-scoped)

Manages Terraform state at `/state/{username}/{workspace}`.

| Command | Description |
|---------|-------------|
| `state get <username> <workspace>` | Fetch state |
| `state post <username> <workspace>` | Store state |
| `state delete <username> <workspace>` | Delete state |
| `state lock <username> <workspace> [--data <json\|@file>]` | Acquire workspace lock |
| `state unlock <username> <workspace> [--data <json\|@file>]` | Release workspace lock |

### `org-state` (org-scoped)

Manages Terraform state at `/{org}/state/{team}/{workspace}`.

| Command | Description |
|---------|-------------|
| `org-state get <org> <team> <workspace>` | Fetch state |
| `org-state post <org> <team> <workspace>` | Store state |
| `org-state delete <org> <team> <workspace>` | Delete state |
| `org-state lock <org> <team> <workspace> [--data <json\|@file>]` | Acquire workspace lock |
| `org-state unlock <org> <team> <workspace> [--data <json\|@file>]` | Release workspace lock |

The `--data` flag on `lock` and `unlock` accepts a JSON string or a `@filename` to read from a file. The lock ID in the data must match the ID of the current lock when unlocking.
