package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/version"
)

var versionCmd = &cobra.Command{
	Use:    "version",
	Short:  "Print the cproxy version",
	Hidden: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println(version.Full())
		return nil
	},
}
