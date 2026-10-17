package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/pool"
)

var (
	addToken string
	addType  string
	addPools []string
)

var accountsCmd = &cobra.Command{
	Use:   "accounts",
	Short: "Manage the Anthropic credentials in credentials.json",
	Long: `Accounts are the upstream Anthropic credentials cproxy rotates through.
They live in ~/.cproxy/credentials.json (or $CPROXY_HOME) and may belong to
any number of pools via --pool; accounts that list no pools belong to the
"default" pool. cproxy never writes to Claude's own files.`,
}

var accountsListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		file, err := config.Load()
		if err != nil {
			return err
		}
		rt, err := config.Build(file, os.Environ())
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSOURCE\tTYPE\tPOOLS\tTOKEN\tSTATUS")
		for _, a := range rt.Accounts {
			typ := a.Type
			if typ == "" {
				typ = pool.DetectTokenType(a.Token)
			}
			pools := strings.Join(a.Pools, ",")
			if pools == "" {
				pools = config.DefaultPoolName
			}
			source := a.Source
			if source == "" {
				source = "file"
			}
			status := "ok"
			if a.Disabled {
				status = "disabled"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				a.Name, source, typ, pools, pool.MaskToken(a.Token), status)
		}
		if len(rt.Accounts) == 0 {
			fmt.Fprintln(os.Stderr, "no accounts yet — add one: cproxy accounts add NAME --token TOKEN")
		}
		return w.Flush()
	},
}

var accountsAddCmd = &cobra.Command{
	Use:   "add NAME [--token TOKEN] [--pool POOL]... [--type oauth|api]",
	Short: "Add an account",
	Long: `Add an account to credentials.json. The token is read from --token, a
hidden prompt (on a TTY), or a line on stdin. Its type is detected from the
prefix (sk-ant-oat01-… → oauth, sk-ant-api03-… → api) unless --type is
given. Without --pool the account joins the "default" pool.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		token := addToken
		if token == "" {
			var err error
			token, err = readToken(name)
			if err != nil {
				return err
			}
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return errors.New("empty token")
		}

		file, err := config.Load()
		if err != nil {
			return err
		}
		for _, a := range file.Accounts {
			if a.Name == name {
				return fmt.Errorf("account %q already exists; remove it first or pick another name", name)
			}
		}

		var typ pool.TokenType
		switch addType {
		case "":
			typ = pool.DetectTokenType(token)
			if typ == pool.TokenTypeOAuth && !strings.HasPrefix(token, "sk-ant-oat") {
				fmt.Fprintf(os.Stderr, "note: token %q does not match a known Anthropic prefix; treating it as an OAuth bearer token (--type api to override)\n", pool.MaskToken(token))
			}
		case string(pool.TokenTypeOAuth):
			typ = pool.TokenTypeOAuth
		case string(pool.TokenTypeAPI):
			typ = pool.TokenTypeAPI
		default:
			return fmt.Errorf("invalid --type %q (want oauth or api)", addType)
		}

		pools := addPools
		if len(pools) == 0 {
			pools = []string{config.DefaultPoolName}
		}
		file.Accounts = append(file.Accounts, pool.Account{
			Name:  name,
			Token: token,
			Type:  typ,
			Pools: pools,
		})
		if err := config.Save(file); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "added account %q (%s) to pool(s) %s\n", name, typ, strings.Join(pools, ","))
		fmt.Fprintf(os.Stderr, "try it: cproxy run --pool %s -- claude\n", pools[0])
		return nil
	},
}

var accountsRemoveCmd = &cobra.Command{
	Use:     "remove NAME",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove an account",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateAccount(args[0], func(accs []pool.Account, i int) []pool.Account { return append(accs[:i:i], accs[i+1:]...) })
	},
}

var accountsDisableCmd = &cobra.Command{
	Use:   "disable NAME",
	Short: "Disable an account (kept in the file, excluded from pools)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateAccount(args[0], func(accs []pool.Account, i int) []pool.Account { accs[i].Disabled = true; return accs })
	},
}

var accountsEnableCmd = &cobra.Command{
	Use:   "enable NAME",
	Short: "Re-enable a disabled account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateAccount(args[0], func(accs []pool.Account, i int) []pool.Account { accs[i].Disabled = false; return accs })
	},
}

// mutateAccount loads the file, applies fn to the named account's slot and
// saves. Env-defined accounts cannot be mutated here.
func mutateAccount(name string, fn func(accs []pool.Account, i int) []pool.Account) error {
	file, err := config.Load()
	if err != nil {
		return err
	}
	for i := range file.Accounts {
		if file.Accounts[i].Name == name {
			file.Accounts = fn(file.Accounts, i)
			if err := config.Save(file); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "updated account %q in %s\n", name, config.Path())
			return nil
		}
	}
	if isEnvAccount(name) {
		return fmt.Errorf("account %q is defined by the environment (CPROXY_TOKENS / CPROXY_POOL_TOKENS_*); change the environment instead", name)
	}
	return fmt.Errorf("no account named %q in %s; see: cproxy accounts list", name, config.Path())
}

func isEnvAccount(name string) bool {
	file, err := config.Load()
	if err != nil {
		return false
	}
	rt, err := config.Build(file, os.Environ())
	if err != nil {
		return false
	}
	for _, a := range rt.Accounts {
		if a.Name == name {
			return a.Source == "env"
		}
	}
	return false
}

// readToken reads the secret from a hidden prompt on a TTY, or one line from
// stdin otherwise.
func readToken(name string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "Anthropic token for account %q (input hidden): ", name)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("read token from stdin: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func init() {
	accountsAddCmd.Flags().StringVar(&addToken, "token", "", "the account's Anthropic token (prompted if omitted)")
	accountsAddCmd.Flags().StringSliceVar(&addPools, "pool", nil, "pool(s) this account belongs to (repeatable)")
	accountsAddCmd.Flags().StringVar(&addType, "type", "", "force token type: oauth or api (default: detected)")
	accountsCmd.AddCommand(accountsListCmd, accountsAddCmd, accountsRemoveCmd, accountsDisableCmd, accountsEnableCmd)
}
