package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
)

var tokenScopes []string

var tokenCmd = &cobra.Command{
	Use:   "token [--scopes proxy,usage]",
	Short: "Generate a random client token for serve-mode auth",
	Long: `Prints a fresh random token (cp_…). Give it to a serve-mode user: they
set ANTHROPIC_AUTH_TOKEN to it. Register it in credentials.json:

	"client_tokens": {"alice": "cp_…"}

or pass it via --client-tokens alice:cp_… / CPROXY_CLIENT_TOKENS.

Scopes limit what a token may do: "proxy" (send requests through cproxy)
and "usage" (view the /usage dashboard and JSON). A token with no scopes
registered may do both. Example: a dashboard-only token:

	"monitor": {"token": "cp_…", "scopes": ["usage"]}`,
	RunE: func(cmd *cobra.Command, args []string) error {
		scopes, err := config.ParseScopes(tokenScopes)
		if err != nil {
			return err
		}
		tok, err := randomToken()
		if err != nil {
			return err
		}
		fmt.Println(tok)
		fmt.Fprintf(os.Stderr, "register it under client_tokens in credentials.json with scopes %s (or --client-tokens name:%s:%s)\n",
			strings.Join(scopes, "+"), tok, strings.Join(scopes, "+"))
		return nil
	},
}

func init() {
	tokenCmd.Flags().StringSliceVar(&tokenScopes, "scopes", nil,
		"scopes this token gets when registered: proxy, usage (default: both)")
}
