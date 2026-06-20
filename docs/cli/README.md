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
armory --theme light-cyber pipelines  # one-off override
```

## Quick Start

```bash
# First-run wizard (conductor URL + login). Or run `armory settings` for the TUI.
armory setup

# Create an account and log in
armory auth signup --email alice@example.com --username alice --password hunter2
armory auth login --email alice@example.com --password hunter2

# Check auth state
armory auth status

# Run something in a sandbox and wait for the result
armory forge exec run --image python:3.12-slim --wait -- python -c 'print("hello")'

# Manage Terraform state workspaces
armory state get alice dev
armory state lock alice dev
armory state unlock alice dev --data '{"ID":"abc123"}'
```

## Interactive hub

Running `armory` with no command opens the **home hub** — a registry-driven TUI
listing every active module (Workflows, Tickets, Notifications, Forge, Hooks,
Chaos, Argo, Secrets, Repos, Containers, Settings). Most command groups below
also open their own TUI screen when run with no subcommand (e.g. `armory secrets`,
`armory forge`, `armory repos`). All screens auto-refresh every 5s; navigate with
the arrow keys, `esc` goes back/home, `ctrl+c` quits.

Platform-administration controls live under a separate **admin hub**, opened with
`armory admin` — see [Administration](#administration-armory-admin).

## Command Reference

### `auth`

| Command | Description |
|---------|-------------|
| `auth login` | Authenticate and save token |
| `auth signup` | Register a new account |
| `auth logout` | Clear saved token from keychain and config |
| `auth status` | Show current auth configuration |

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
| `state push <username> <workspace>` | Upload (store) state |
| `state delete <username> <workspace>` | Delete state |
| `state lock <username> <workspace> [--data <json\|@file>]` | Acquire workspace lock |
| `state unlock <username> <workspace> [--data <json\|@file>]` | Release workspace lock |

The `--data` flag on `lock` and `unlock` accepts a JSON string or a `@filename` to read from a file. The lock ID in the data must match the ID of the current lock when unlocking.

### `pipelines`

Manage CI/CD steps, pipelines, and runs. Steps are reusable building blocks; pipelines compose steps using a DSL (`step1->step2->[parallel_a,parallel_b]->step3`); runs are triggered executions of a pipeline. `armory pipelines` with no subcommand opens the Workflows TUI.

**Steps**

| Command | Flags | Description |
|---------|-------|-------------|
| `pipelines create step <name>` | `--action`, `--with <json>`, `--image`, `--run`, `--runner-class`, `--env KEY=VAL` (repeatable), `--timeout`, `--description`, `-f <file>` | Create a reusable step |
| `pipelines list steps` | | List all steps |
| `pipelines get step <id>` | | Get a step |
| `pipelines update step <id>` | `--action`, `--with <json>`, `--image`, `--run`, `--runner-class`, `--env KEY=VAL` (repeatable), `--timeout`, `--description`, `-f <file>` | Update a step |
| `pipelines delete step <id>` | | Delete a step |

**Pipelines**

| Command | Flags | Description |
|---------|-------|-------------|
| `pipelines create pipeline <repo> <branch> [dsl]` | `-f <file>` | Create a pipeline from a DSL string or JSON file |
| `pipelines list pipelines` | | List all pipelines |
| `pipelines get pipeline <id>` | | Get a pipeline with full step definitions |
| `pipelines update pipeline <id> [dsl]` | `--name`, `--description`, `-f <file>` | Replace a pipeline's step list |
| `pipelines delete pipeline <id>` | | Delete a pipeline |

**Runs**

| Command | Flags | Description |
|---------|-------|-------------|
| `pipelines run pipeline <id>` | `--input KEY=VAL` (repeatable, `-i`) | Trigger a pipeline run |
| `pipelines list runs` | `--pipeline <id>` | List runs, optionally filtered by pipeline |
| `pipelines get run <id>` | | Get a run with step details (incl. `memory_used_mb`/`memory_limit_mb` for forge-backed steps) |
| `pipelines cancel run <id>` | | Cancel a pending or running run |

**Actions**

| Command | Description |
|---------|-------------|
| `pipelines list actions` | List available workflow actions from the service catalog |

For `forge/run` steps, `--image`, `--run`, `--env`, and `--runner-class` are convenience flags that build the required `with` JSON automatically (`--runner-class` is optional; forge applies its default class when omitted). For all other actions, `--with <json>` is required.

### `forge`

Submit and manage sandboxed code executions. `armory forge` with no subcommand opens the Forge TUI.

| Command | Flags | Description |
|---------|-------|-------------|
| `forge exec run -- <cmd> [args...]` | `--image` (required), `--timeout <secs>`, `--env KEY=VAL` (repeatable), `--runner-class <name>`, `--wait` | Submit a sandboxed execution. `--wait` blocks until it finishes, prints output, and exits non-zero on failure. |
| `forge exec list` | `--status <pending\|running\|completed\|failed\|timed_out\|cancelled>` | List your executions |
| `forge exec get <id>` | | Get an execution incl. stdout/stderr and `memory_used_mb`/`memory_limit_mb` |
| `forge exec rerun <id>` | `--wait` | Resubmit with the same image, command, env, timeout, and runner class |
| `forge exec cancel <id>` | | Cancel a running or pending execution |

When `--wait` is set, forge prints a `memory: <used>/<limit>` line on completion. Runner-class and runtime-backend management is an admin control under [`admin forge-runtimes`](#admin-forge-runtimes).

### `hooks`

Manage webhook pipeline rules and event history. `armory hooks` opens the Hooks TUI.

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

Manage tickets and comments. `armory tickets` opens the Tickets board TUI.

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

**Field definitions**

Custom statuses, priorities, and timescales that the board and ticket forms use.

| Command | Description |
|---------|-------------|
| `tickets field-defs list` | List custom field definitions |
| `tickets field-defs create` | Create a custom field definition |
| `tickets field-defs update <id>` | Update a definition's label, color, or position |
| `tickets field-defs delete <id>` | Delete a field definition |

`armory tickets board` opens the kanban board directly (the same screen as the
no-argument `armory tickets`).

### `notifications`

Manage notification channels and send notifications. `armory notifications` opens the Notifications TUI.

**Channels**

| Command | Flags | Description |
|---------|-------|-------------|
| `notifications providers` | | List available provider types and their config fields |
| `notifications channels list` | | List channels |
| `notifications channels get <channel-id>` | | Get a channel |
| `notifications channels create` | `--name`, `--type <slack\|email\|webhook>`, `--config KEY=VALUE` (repeatable), `--enabled` | Create a channel |
| `notifications channels delete <channel-id>` | | Delete a channel |
| `notifications channels test <channel-id>` | `--subject`, `--body` | Send a test notification through a channel |

**Sending**

| Command | Flags | Description |
|---------|-------|-------------|
| `notifications send` | `--body` (required), `--subject`, `--channel <id>` (repeatable; default: all enabled) | Send a notification to one or more channels |
| `notifications list` | `--status <pending\|sent\|failed>` | List recent delivery records |

### `secrets`

Manage encrypted secrets and the org's external secret provider. `armory secrets` opens the Secrets TUI. Secret values are never returned by the API — `list` shows names only.

| Command | Flags | Description |
|---------|-------|-------------|
| `secrets create <name> <value>` | | Store a new secret |
| `secrets list` | | List secrets (names only) |
| `secrets update <id>` | `--value` (required) | Replace a secret's value |
| `secrets delete <id>` | | Delete a secret |
| `secrets provider get <org-id>` | | Show the org's configured secret provider |
| `secrets provider set <org-id> <provider>` | `--config <json>` | Configure an external provider (e.g. `vault`, `doppler`) for the org |
| `secrets provider delete <org-id>` | | Remove the external provider (resets to built-in) |

### `repos`

Manage Gitea/Forgejo repositories and pull requests. `armory repos` opens the Repos TUI.

**Account**

| Command | Flags | Description |
|---------|-------|-------------|
| `repos account get` | | Show your linked Gitea account |
| `repos account link` | `--token <pat>` (required) | Link your Gitea account with an API token |
| `repos account unlink` | | Unlink your Gitea account |

**Repositories**

| Command | Flags | Description |
|---------|-------|-------------|
| `repos list` | | List your repositories |
| `repos create` | `--name` (required), `--description`, `--private` | Create a repository |
| `repos get <owner>/<name>` | | Get a repository |
| `repos delete <owner>/<name>` | | Delete a repository |
| `repos branches <owner>/<name>` | | List branches |
| `repos tags <owner>/<name>` | | List tags |
| `repos commits <owner>/<name>` | | List recent commits |

**Pull requests**

| Command | Flags | Description |
|---------|-------|-------------|
| `repos pulls list <owner>/<name>` | | List pull requests |
| `repos pulls create <owner>/<name>` | `--title` (required), `--head` (required), `--base` (required), `--body` | Open a pull request |
| `repos pulls get <owner>/<name> <index>` | | Get a pull request by index |
| `repos pulls merge <owner>/<name> <index>` | | Merge a pull request |

### `containers`

Browse the OCI registry. `armory containers` opens the Containers TUI.

| Command | Description |
|---------|-------------|
| `containers list repos` | List image repositories |
| `containers list tags <namespace/image>` | List tags for an image |
| `containers get manifest <namespace/image> <reference>` | Get an image manifest by tag or digest |
| `containers delete manifest <namespace/image> <digest>` | Delete a manifest by digest (`sha256:…`) |

### `chaos`

Run chaos-engineering experiments against a connected cluster through an outpost. `armory chaos` opens the Chaos TUI.

| Command | Flags | Description |
|---------|-------|-------------|
| `chaos types` | | List available experiment types |
| `chaos list` | | List experiments |
| `chaos create` | `--outpost` (required), `--type` (required), `--namespace` (required), `--label` (required), `--kind` (default `deployment`), `--param KEY=VALUE` (repeatable) | Start an experiment |
| `chaos get <id>` | | Get an experiment |
| `chaos delete <id>` | | Delete an experiment |

### `argo`

Manage Argo CD applications and syncs through an outpost. `armory argo` opens the Argo TUI.

| Command | Flags | Description |
|---------|-------|-------------|
| `argo list` | | List Argo CD applications |
| `argo get <name>` | | Get an application |
| `argo sync <name>` | `--revision <git-ref>` (default: app's latest), `--outpost <id>` (default: the app's reported outpost) | Trigger a sync |
| `argo sync-status <sync-id>` | | Get the status of a sync operation |

### `theme`

Choose the color theme for the interactive TUIs (`armory` home screen,
`pipelines`, `hooks`, `tickets`, forge/audit views). See
[Configuration](#configuration) for the resolution order.

| Command | Flags | Description |
|---------|-------|-------------|
| `theme list` | | List available themes (active one marked) |
| `theme show` | | Print the active theme name |
| `theme set <name>` | | Persist a theme to config (`cyber`, `tokyo-night`, `light-cyber`, `dracula`, `nord`, `gruvbox`, `catppuccin`, `solarized`) |

### `settings`

Interactive screen for client-side configuration — theme (with live preview),
conductor URL, and account sign-in — saved to `~/.config/codearmory/config.json`.
Also reachable from the **Settings** entry on the `armory` home screen, and shown
automatically as a welcome screen on first use.

| Command | Flags | Description |
|---------|-------|-------------|
| `settings` | | Open the settings TUI (←/→ cycle theme, enter save, esc cancel) |

### `setup`

Scriptable first-run wizard for the conductor URL and authentication — the
non-interactive counterpart to the `settings` TUI, useful in scripts and CI.

| Command | Flags | Description |
|---------|-------|-------------|
| `setup` | `--url`, `--email`, `--username`, `--signup` | Configure conductor URL and log in (or create an account with `--signup` / `--username`) |

## Administration (`armory admin`)

`armory admin` opens the **admin hub** — a TUI listing the platform-administration
screens (Forge Runtimes, Outposts, Audit Log, Gatekeeper). Each control is also
available as a subcommand, e.g. `armory admin gatekeeper`, `armory admin roles`.

The split is organisational, not a security boundary: permission enforcement
stays server-side, so a user without admin grants can open a screen, but their
mutating actions come back as a `403` status line.

### `admin gatekeeper`

`armory admin gatekeeper` opens the Gatekeeper TUI for managing teams, invites,
service requests, roles, organizations, and users interactively (roles are edited
together with their permissions in a unified editor). The same resources are also
scriptable via the subcommands below.

### `admin forge-runtimes`

`armory admin forge-runtimes` opens the Forge Runtimes TUI for managing runner
classes and runtime backends. Runner classes are also managed from the CLI:

| Command | Flags | Description |
|---------|-------|-------------|
| `admin forge-runtimes runner-classes list` | | List runner classes |
| `admin forge-runtimes runner-classes get <name>` | | Get a runner class |
| `admin forge-runtimes runner-classes create` | `--name` (required), `--memory-mb` (default 512), `--cpu-millicores` (default 500), `--pids-limit` (default 64), `--tmpfs-mb` (default 128), `--enabled` (default true) | Create a runner class |
| `admin forge-runtimes runner-classes update <name>` | same flags as `create` | Update a runner class |
| `admin forge-runtimes runner-classes delete <name>` | | Delete a runner class |

The `privileged` flag (root + writable rootfs, honoured only on VM-isolated
`kata`/`proxmox` backends) is set via the Forge Runtimes TUI editor or the
runner-class API, not these CLI flags. See [forge runner classes](../forge/README.md#runner-classes).

### `admin outpost`

| Command | Flags | Description |
|---------|-------|-------------|
| `admin outpost list` | | List outposts |
| `admin outpost create` | `--name` (required), `--module <chaos\|argo>` (repeatable) | Register an outpost (prints its single-use enrollment token) |
| `admin outpost get <id>` | | Get an outpost |
| `admin outpost delete <id>` | | Delete an outpost |

### `admin audit`

`armory admin audit` opens the Audit Log TUI. It is also scriptable:

| Command | Description |
|---------|-------------|
| `admin audit list` | List audit log entries |

### `admin users`

| Command | Description |
|---------|-------------|
| `admin users list` | List all users |
| `admin users get <id>` | Get a user by ID |
| `admin users me` | Get your own profile |
| `admin users update <id>` | Update a user (self-profile fields: email/username/password/name) |
| `admin users delete <id>` | Delete a user |

### `admin orgs`

| Command | Description |
|---------|-------------|
| `admin orgs list` | List all organizations |
| `admin orgs get <id>` | Get an organization by ID |
| `admin orgs mine` | Get your organization |
| `admin orgs create` | Create an organization |
| `admin orgs update <id>` | Update an organization |
| `admin orgs invite <id>` | Send an invite for an organization |
| `admin orgs delete <id>` | Delete an organization |

### `admin teams`

| Command | Description |
|---------|-------------|
| `admin teams list` | List all teams |
| `admin teams get <id>` | Get a team by ID |
| `admin teams mine` | Get your team |
| `admin teams create` | Create a team |
| `admin teams update <id>` | Update a team |
| `admin teams invite <id>` | Send an invite for a team |
| `admin teams delete <id>` | Delete a team |

### `admin roles`

| Command | Description |
|---------|-------------|
| `admin roles list` | List all roles |
| `admin roles get <id>` | Get a role by ID |
| `admin roles mine` | Get your role |
| `admin roles create` | Create a role |
| `admin roles update <id>` | Update a role |
| `admin roles delete <id>` | Delete a role |

In the TUI, roles and their permissions are edited together as one item and split
back into separate permission/role API calls on save (see `admin gatekeeper`).

### `admin permissions`

| Command | Description |
|---------|-------------|
| `admin permissions get <id>` | Get a permission by ID |
| `admin permissions create` | Create a permission |
| `admin permissions update <id>` | Update a permission |
| `admin permissions delete <id>` | Delete a permission |

### `admin service-requests`

| Command | Description |
|---------|-------------|
| `admin service-requests list` | List service permission requests |
| `admin service-requests get <id>` | Get a service permission request |
| `admin service-requests approve <id>` | Approve a request |
| `admin service-requests decline <id>` | Decline a request |
