package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// git_factory is the platform-native git host. Unlike the `git` module (which fronts
// the git_connector credential broker for EXTERNAL backends), these commands wire the
// local `git` client up to git_factory's own HTTP wire so clone/push authenticate with
// the CodeArmory session token — no PAT, no manual credential entry.

// gitFactoryServiceName is git_factory's REGISTERED identity — not "git_factory",
// which is only its directory and Go module name. Every RBAC resource it declares is
// prefixed with this, and it is what GET /services reports, so it is the only spelling
// a client may match on.
const gitFactoryServiceName = "codearmory_git_factory"

// gitFactoryBaseURL resolves the clone base to offer as the prompt default, preferring
// what git_factory itself ADVERTISES over any guess made from conductor's address.
//
// The guess below assumes git_factory sits on conductor's host at port 9002, which is
// only true when it is reached in-cluster. Once it is fronted by its own ingress — which
// the chart now does, at its own hostname — the guess names an address that is not
// reachable from where the CLI runs, and the user has to know to correct it. git_factory
// composes every repo's http_url from GIT_HTTP_BASE_URL, which is precisely the
// externally-usable clone base an operator configured, so asking for it beats deriving it.
//
// Falls back to the guess when the platform cannot answer — not signed in, git_factory
// not deployed, or no repo exists yet to advertise a URL. The result is a prompt default
// either way, so a wrong answer costs a keystroke rather than a broken setup.
func gitFactoryBaseURL() string {
	if base, ok := gitFactoryAdvertisedBaseURL(); ok {
		return base
	}
	return gitFactoryDefaultBaseURL()
}

// gitFactoryAdvertisedBaseURL reads the clone base out of any repo's http_url.
func gitFactoryAdvertisedBaseURL() (string, bool) {
	raw, err := doRequest("GET", "/"+gitFactoryServiceName+"/repos", nil)
	if err != nil {
		return "", false
	}
	var repos []struct {
		HTTPURL string `json:"http_url"`
	}
	if err := json.Unmarshal(raw, &repos); err != nil {
		return "", false
	}
	for _, r := range repos {
		if base, ok := baseFromCloneURL(r.HTTPURL); ok {
			return base, true
		}
	}
	return "", false
}

// baseFromCloneURL strips the "/<namespace>/<name>.git" that git_factory appends to its
// configured base, recovering the base itself. Both segments are dropped rather than
// just the last, since the namespace is part of the repo path and not of the base.
func baseFromCloneURL(clone string) (string, bool) {
	if !strings.HasSuffix(clone, ".git") {
		return "", false
	}
	u, err := url.Parse(clone)
	if err != nil || u.Host == "" {
		return "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 {
		return "", false
	}
	u.Path = "/" + strings.Join(segs[:len(segs)-2], "/")
	return strings.TrimRight(u.String(), "/"), true
}

// gitFactoryDefaultBaseURL guesses git_factory's git base URL from the conductor URL:
// same scheme/host, port 9002 (git_factory's service port). Used only when the platform
// cannot be asked — see gitFactoryBaseURL.
func gitFactoryDefaultBaseURL() string {
	u, err := url.Parse(conductorURL())
	if err != nil || u.Hostname() == "" {
		return "http://localhost:9002"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:9002", scheme, u.Hostname())
}

// configureGitCredentialHelper points `git`, for the given base URL, at this binary's
// credential-helper subcommand so it hands git the session token as the password. It
// writes global git config (credential.<base>.helper / .username). Scoped to the exact
// base URL so the token is never offered to any other host.
func configureGitCredentialHelper(base string) error {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "armory"
	}
	helper := fmt.Sprintf("!%q git credential-helper", self)
	settings := [][2]string{
		{"credential." + base + ".helper", helper},
		{"credential." + base + ".username", gitCredentialUsername},
	}
	for _, kv := range settings {
		out, err := exec.Command("git", "config", "--global", kv[0], kv[1]).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git config %s: %w: %s", kv[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// gitCredentialUsername is the username handed to git. git_factory reads the token from
// the password field (falling back to the username), so the username is informational;
// "token" is the conventional placeholder for token-in-password auth.
const gitCredentialUsername = "token"

// setupGitFactory is the `armory setup` git step. It is a no-op (returns nil) unless
// git_factory is a registered service AND we can prompt — an unattended run or a cluster
// without git_factory silently skips it. Returns an error only if the user opted in and
// configuration then failed.
func setupGitFactory() error {
	if !isSignedIn() {
		return nil // service list needs auth; auth step handles its own errors
	}
	// The REGISTERED name, not the directory or module name. git_factory registers
	// itself as "codearmory_git_factory" — that string is its service identity, the
	// prefix of every one of its RBAC resources, and what GET /services returns.
	// Checking "git_factory" here matched nothing, so this whole step silently did
	// nothing on every install: no error, no prompt, just no git configuration.
	if !registeredServices()[gitFactoryServiceName] {
		return nil // git_factory not enabled on this platform — nothing to wire up
	}
	if !isTerminal() {
		return nil // never prompt unattended
	}
	answer, err := prompt("git_factory detected. Configure git to authenticate with your CodeArmory token? [Y/n]", "y")
	if err != nil {
		return fmt.Errorf("reading input: %w", err)
	}
	if strings.EqualFold(answer, "n") || strings.EqualFold(answer, "no") {
		return nil
	}
	base, err := prompt("git_factory base URL", gitFactoryBaseURL())
	if err != nil {
		return fmt.Errorf("reading URL: %w", err)
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return fmt.Errorf("git_factory base URL is required")
	}
	if err := configureGitCredentialHelper(base); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Configured git credentials for %s.\n", base)
	fmt.Fprintf(os.Stderr, "  Clone/push now authenticate with your token, e.g.:\n    git clone %s/<namespace>/<repo>.git\n", base)
	return nil
}

// gitFactorySetupCmd re-runs the git_factory wiring on demand (e.g. after the base URL
// changes), independent of the full `armory setup` wizard.
var gitFactorySetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure git to authenticate with git_factory using your CodeArmory token",
	Long: `Point the local git client at git_factory's HTTP wire so clone/push use your
CodeArmory session token — no personal access token or manual credential entry.

Writes a scoped git credential helper (global git config, for the git_factory base URL
only). Runs automatically inside 'armory setup' when git_factory is detected; use this
to (re)configure it, e.g. after the base URL changes.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !isSignedIn() {
			return fmt.Errorf("not signed in — run 'armory auth login' first")
		}
		base, _ := cmd.Flags().GetString("url")
		if base == "" {
			if isTerminal() {
				var err error
				if base, err = prompt("git_factory base URL", gitFactoryBaseURL()); err != nil {
					return fmt.Errorf("reading URL: %w", err)
				}
			} else {
				base = gitFactoryBaseURL()
			}
		}
		base = strings.TrimRight(base, "/")
		if base == "" {
			return fmt.Errorf("git_factory base URL is required")
		}
		if err := configureGitCredentialHelper(base); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Configured git credentials for %s.\n", base)
		return nil
	},
}

// gitCredentialHelperCmd implements git's credential-helper protocol. git invokes it as
// `armory git credential-helper <op>` with the request on stdin. On "get" it prints the
// session token as the password; "store"/"erase" are no-ops (the token is owned by
// `armory auth`, not git).
var gitCredentialHelperCmd = &cobra.Command{
	Use:    "credential-helper [get|store|erase]",
	Short:  "git credential helper backed by the CodeArmory session token",
	Hidden: true,
	Args:   cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Drain stdin (git writes the request there) so git never sees a broken pipe.
		_, _ = io.Copy(io.Discard, cmd.InOrStdin())
		op := ""
		if len(args) > 0 {
			op = args[0]
		}
		if op != "get" {
			return nil // store/erase: nothing to persist here
		}
		tok := bearerToken()
		if tok == "" {
			return nil // no usable token — stay silent so git falls back / prompts
		}
		fmt.Fprintf(cmd.OutOrStdout(), "username=%s\npassword=%s\n", gitCredentialUsername, tok)
		return nil
	},
}

func init() {
	gitCmd.AddCommand(gitFactorySetupCmd, gitCredentialHelperCmd)
	gitFactorySetupCmd.Flags().String("url", "", "git_factory base URL (skips the prompt)")
}
