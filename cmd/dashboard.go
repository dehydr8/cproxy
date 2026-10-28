package cmd

import (
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/wrap"
)

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Print a link to the local daemon's live usage dashboard",
	Long: `Print a link that opens the local daemon's live usage dashboard in a
browser, starting the daemon when needed. The link carries a generated
view-only token (~/.cproxy/dashboard-token): it can see usage, never spend.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		tok, err := config.EnsureDashboardToken()
		if err != nil {
			return err
		}
		_, st, err := wrap.ConnectDaemon(os.Environ())
		if err != nil {
			return err
		}
		if err := wrap.VerifyWithGrace(st.Addr, tok); err != nil {
			return err
		}
		fmt.Printf("http://%s/usage?token=%s\n", st.Addr, url.QueryEscape(tok))
		return nil
	},
}
