# Armory CLI

Command-line client for the CodeArmory platform. All commands talk to [Conductor](../conductor/README.md), which verifies the caller's identity and routes requests to the appropriate backend service.

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

**Theme** (highest to lowest):
1. `--theme` flag
2. `CODEARMORY_THEME` environment variable
3. `~/.config/codearmory/config.json` (set via `armory theme set`)

Available themes: `cyber` (default), `tokyo-night`, `light-cyber` (a bright,
high-key variant of cyber), `dracula`, `nord`, `gruvbox`, `catppuccin`,
`solarized`. Unknown values fall back to `cyber`. Run `armory settings` for an
interactive picker that previews themes live.

```bash
# Point at a non-default conductor
export CODEARMORY_URL=https://conductor.example.com

# Or pass per-command
armory --url https://conductor.example.com auth status

# Pick a color theme for the interactive TUIs
armory theme set tokyo-night          # persist to config
armory --theme light-cyber ci         # one-off override
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

### `state`

Manages Terraform state at `/state/{username}/{workspace}`.

| Command | Description |
|---------|-------------|
| `state get <username> <workspace>` | Fetch state |
| `state post <username> <workspace>` | Store state |
| `state delete <username> <workspace>` | Delete state |
| `state lock <username> <workspace> [--data <json\|@file>]` | Acquire workspace lock |
| `state unlock <username> <workspace> [--data <json\|@file>]` | Release workspace lock |

The `--data` flag on `lock` and `unlock` accepts a JSON string or a `@filename` to read from a file. The lock ID in the data must match the ID of the current lock when unlocking.

### `ci`

Manage CI/CD steps, pipelines, and runs. Steps are reusable building blocks; pipelines compose steps using a DSL (`step1->step2->[parallel_a,parallel_b]->step3`); runs are triggered executions of a pipeline.

**Steps**

| Command | Flags | Description |
|---------|-------|-------------|
| `ci create step <name>` | `--action`, `--with <json>`, `--image`, `--run`, `--env KEY=VAL` (repeatable), `--timeout`, `--description`, `-f <file>` | Create a reusable step |
| `ci list steps` | | List all steps |
| `ci get step <id>` | | Get a step |
| `ci update step <id>` | `--action`, `--with <json>`, `--image`, `--run`, `--env KEY=VAL` (repeatable), `--timeout`, `--description`, `-f <file>` | Update a step |
| `ci delete step <id>` | | Delete a step |

**Pipelines**

| Command | Flags | Description |
|---------|-------|-------------|
| `ci create pipeline <repo> <branch> [dsl]` | `-f <file>` | Create a pipeline from a DSL string or JSON file |
| `ci list pipelines` | | List all pipelines |
| `ci get pipeline <id>` | | Get a pipeline with full step definitions |
| `ci update pipeline <id> [dsl]` | `--name`, `--description`, `-f <file>` | Replace a pipeline's step list |
| `ci delete pipeline <id>` | | Delete a pipeline |

**Runs**

| Command | Flags | Description |
|---------|-------|-------------|
| `ci run pipeline <id>` | `--input KEY=VAL` (repeatable, `-i`) | Trigger a pipeline run |
| `ci list runs` | `--pipeline <id>` | List runs, optionally filtered by pipeline |
| `ci get run <id>` | | Get a run with step details |
| `ci cancel run <id>` | | Cancel a pending or running run |

**Actions**

| Command | Description |
|---------|-------------|
| `ci list actions` | List available workflow actions from the service catalog |

For `forge/run` steps, `--image` and `--run` are convenience flags that build the required `with` JSON automatically. For all other actions, `--with <json>` is required.

### `hooks`

Manage webhook pipeline rules and event history.

**Rules**

| Command | Flags | Description |
|---------|-------|-------------|
| `hooks rules create` | `--name`, `--repo`, `--events` (repeatable), `--ref-filter`, `--workflow <id>`, `--secret`, `--input KEY=VAL` (repeatable, `-i`) | Create a webhook pipeline rule |
| `hooks rules list` | | List rules |
| `hooks rules get <id>` | | Get a rule |
| `hooks rules update <id>` | `--name`, `--repo`, `--events` (repeatable), `--ref-filter`, `--workflow <id>`, `--secret`, `--clear-secret`, `--input KEY=VAL` (repeatable, `-i`) | Update a rule (all fields replaced) |
| `hooks rules delete <id>` | | Delete a rule |

On `rules update`, omit `--secret` to leave the existing secret unchanged, pass `--clear-secret` to remove it, or pass `--secret <value>` to replace it.

**Events**

| Command | Flags | Description |
|---------|-------|-------------|
| `hooks events list` | `--repo` | List received webhook events, optionally filtered by repo |
| `hooks events get <id>` | | Get an event with its trigger details |

### `tickets`

Manage tickets and comments.

**Tickets**

| Command | Flags | Description |
|---------|-------|-------------|
| `tickets create` | `--title` (required), `--description`, `--priority` (low/medium/high/critical), `--assignee <user-id>`, `--workflow <id>`, `--run <id>`, `--forge-execution <id>` | Create a ticket |
| `tickets list` | `--status` (open/in_progress/resolved/closed), `--priority`, `--assignee <user-id>` | List tickets |
| `tickets get <id>` | | Get a ticket with its comments |
| `tickets update <id>` | `--title`, `--description`, `--status`, `--priority`, `--assignee <user-id>`, `--workflow <id>`, `--run <id>`, `--forge-execution <id>` | Update a ticket (title fetched automatically if omitted) |
| `tickets delete <id>` | | Delete a ticket |

**Comments**

| Command | Flags | Description |
|---------|-------|-------------|
| `tickets comment add <ticket-id>` | `--body` (required) | Add a comment to a ticket |
| `tickets comment delete <ticket-id> <comment-id>` | | Delete a comment |

### `theme`

Choose the color theme for the interactive TUIs (`armory` home screen, `ci`,
`hooks`, `tickets`, forge/audit views). See [Configuration](#configuration) for
the resolution order.

| Command | Flags | Description |
|---------|-------|-------------|
| `theme list` | | List available themes (active one marked) |
| `theme show` | | Print the active theme name |
| `theme set <name>` | | Persist a theme to config (`cyber`, `tokyo-night`, `light-cyber`, `dracula`, `nord`, `gruvbox`, `catppuccin`, `solarized`) |

### `settings`

Interactive screen for client-side configuration — theme (with live preview)
and conductor URL — saved to `~/.config/codearmory/config.json`. Also reachable
from the **Settings** entry on the `armory` home screen.

| Command | Flags | Description |
|---------|-------|-------------|
| `settings` | | Open the settings TUI (←/→ cycle theme, enter save, esc cancel) |
