package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
	"golang.org/x/term"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage authentication",
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate and save token to config",
	RunE: func(cmd *cobra.Command, args []string) error {
		email, _ := cmd.Flags().GetString("email")
		fmt.Fprint(os.Stderr, "Password: ")
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		body, _ := json.Marshal(map[string]string{"email": email, "password": string(raw)})
		data, err := doRequest("POST", "/login", body)
		if err != nil {
			return err
		}
		var resp struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(data, &resp); err != nil || resp.Token == "" {
			return fmt.Errorf("unexpected login response: %s", data)
		}
		where, err := storeToken(resp.Token)
		if err != nil {
			return fmt.Errorf("saving token: %w", err)
		}
		fmt.Printf("Logged in. Token saved to %s\n", where)
		return nil
	},
}

var signupCmd = &cobra.Command{
	Use:   "signup",
	Short: "Register a new account",
	RunE: func(cmd *cobra.Command, args []string) error {
		email, _ := cmd.Flags().GetString("email")
		username, _ := cmd.Flags().GetString("username")
		fmt.Fprint(os.Stderr, "Password: ")
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		body, _ := json.Marshal(map[string]string{
			"email":    email,
			"username": username,
			"password": string(raw),
		})
		return apiCall("POST", "/signup", body)
	},
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear saved token from keychain and config",
	RunE: func(cmd *cobra.Command, args []string) error {
		clearToken()
		fmt.Println("Logged out.")
		return nil
	},
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current auth configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("URL:   %s\n", conductorURL())

		switch {
		case flagToken != "":
			fmt.Printf("Token: %s... (--token flag)\n", flagToken[:min(20, len(flagToken))])
		case os.Getenv("CODEARMORY_TOKEN") != "":
			t := os.Getenv("CODEARMORY_TOKEN")
			fmt.Printf("Token: %s... (CODEARMORY_TOKEN)\n", t[:min(20, len(t))])
		default:
			if t, err := keyring.Get(keychainService, keychainAccount); err == nil && t != "" {
				fmt.Printf("Token: %s... (keychain)\n", t[:min(20, len(t))])
			} else if t := loadConfig().Token; t != "" {
				fmt.Printf("Token: %s... (config file)\n", t[:min(20, len(t))])
			} else {
				fmt.Println("Token: (not set — run `armory auth login`)")
			}
		}
		return nil
	},
}

func init() {
	loginCmd.Flags().String("email", "", "email address")
	loginCmd.MarkFlagRequired("email") //nolint:errcheck

	signupCmd.Flags().String("email", "", "email address")
	signupCmd.Flags().String("username", "", "username (alphanumeric, hyphens, underscores; 1–64 chars)")
	signupCmd.MarkFlagRequired("email")    //nolint:errcheck
	signupCmd.MarkFlagRequired("username") //nolint:errcheck

	authCmd.AddCommand(loginCmd, signupCmd, logoutCmd, authStatusCmd)
	rootCmd.AddCommand(authCmd)
}
