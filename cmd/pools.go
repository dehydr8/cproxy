package cmd

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
)

var poolsCmd = &cobra.Command{
	Use:     "pools",
	Aliases: []string{"pool"},
	Short:   "List credential pools",
	Long: `Pools group accounts (membership lives on each account's "pools" list, so
it follows the account). Accounts with no pools belong to "default". A
pool's strategy is sticky by default: each client stays on one account and
only switches when that account hits its limit.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		file, err := config.Load()
		if err != nil {
			return err
		}
		rt, err := config.Build(file, os.Environ())
		if err != nil {
			return err
		}
		names := rt.PoolNames()
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "POOL\tSTRATEGY\tMEMBERS\tSOURCE")
		for _, name := range names {
			strategy := string(pool.DefaultStrategy)
			if pc, ok := rt.Pools[name]; ok && pc.Strategy != "" {
				strategy = pc.Strategy
			}
			var members []string
			sources := map[string]bool{}
			for _, a := range rt.Accounts {
				if !slices.Contains(rt.AccountPools(a), name) {
					continue
				}
				label := a.Name
				if a.Disabled {
					label += " (disabled)"
				}
				members = append(members, label)
				src := a.Source
				if src == "" {
					src = "file"
				}
				sources[src] = true
			}
			var srcs []string
			for s := range sources {
				srcs = append(srcs, s)
			}
			slices.Sort(srcs)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, strategy, strings.Join(members, ","), strings.Join(srcs, "+"))
		}
		if len(names) == 0 {
			fmt.Fprintln(os.Stderr, "no pools yet — add an account: cproxy accounts add NAME --pool POOL")
		}
		return w.Flush()
	},
}
