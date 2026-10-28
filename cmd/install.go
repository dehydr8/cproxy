package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/wrap"
)

var installPathFlag string

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Replace the claude binary with the cproxy shim",
	Long: `Wrap the installed claude binary: the real binary is moved to
claude.cproxy-real beside itself and a tiny shim takes its place, so every
` + "`claude`" + ` launch runs through cproxy when credentials are
configured, and completely untouched when they are not (see ` + "`cproxy claude`" + `).

The shim holds no secrets and no decisions; credentials are read on each
launch. ` + "`cproxy uninstall`" + ` restores the original binary, and re-running
` + "`cproxy install`" + ` after Claude Code updates itself refreshes the shim (its
updater can overwrite it). Use --path when the binary is not on PATH.`,
	RunE: runInstall,
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Restore the real claude binary moved aside by install",
	Long: `Undo ` + "`cproxy install`" + `: the shim is replaced by the real claude
binary that was moved to claude.cproxy-real. Only a cproxy shim is replaced.`,
	RunE: runUninstall,
}

func init() {
	pathUsage := "path to the claude binary (default: discover from PATH, ~/.local/bin, ~/bin)"
	installCmd.Flags().StringVar(&installPathFlag, "path", "", pathUsage)
	uninstallCmd.Flags().StringVar(&installPathFlag, "path", "", pathUsage)
}

func runInstall(cmd *cobra.Command, args []string) error {
	target, err := discoverClaude(installPathFlag)
	if err != nil {
		return err
	}
	shim, backup, err := wrap.Install(target)
	switch {
	case err == nil:
		fmt.Printf("installed cproxy shim at %s\nreal claude binary moved to %s\n"+
			"`claude` now runs through cproxy when credentials are configured; `cproxy uninstall` restores it.\n", shim, backup)
		return nil
	case errors.Is(err, wrap.ErrAlreadyInstalled):
		fmt.Printf("the cproxy shim is already installed at %s (real claude binary: %s)\n", target, wrap.BackupPath(target))
		return nil
	default:
		return err
	}
}

func runUninstall(cmd *cobra.Command, args []string) error {
	target, err := discoverClaude(installPathFlag)
	if err != nil {
		return err
	}
	restored, err := wrap.Uninstall(target)
	switch {
	case err == nil:
		fmt.Printf("restored the real claude binary to %s\n", restored)
		return nil
	case errors.Is(err, wrap.ErrNotInstalled):
		fmt.Printf("no cproxy shim found at %s\n", target)
		return nil
	default:
		return err
	}
}
