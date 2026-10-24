package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/daemon"
	"github.com/dehydr8/cproxy/internal/proxy"
	"github.com/dehydr8/cproxy/internal/version"
	"github.com/dehydr8/cproxy/internal/wrap"
)

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the local daemon runs, its version, address and log",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		c, st, err := daemon.Dial()
		if err != nil {
			fmt.Fprintln(out, "no cproxy daemon is running; the next claude launch or `cproxy daemon start` starts one")
			return nil
		}
		fmt.Fprintf(out, "cproxy daemon %s · pid %d · up %s\n", st.Version, st.PID, humanDur(time.Since(st.Started)))
		fmt.Fprintf(out, "  listening  http://%s\n", st.Addr)
		if body, err := c.Usage(); err == nil {
			var snap proxy.UsageSnapshot
			if json.Unmarshal(body, &snap) == nil {
				live := int64(0)
				for _, a := range snap.Accounts {
					live += a.InFlight
				}
				fmt.Fprintf(out, "  serving    %d accounts · %d streaming\n", len(snap.Accounts), live)
			}
		}
		fmt.Fprintf(out, "  socket     %s\n", daemon.SocketPath())
		fmt.Fprintf(out, "  log        %s\n", daemon.LogPath())
		if daemon.Newer(version.String(), st.Version) {
			fmt.Fprintf(out, "this cproxy is %s: `cproxy daemon restart` (or the next claude launch) moves the daemon to it\n", version.String())
		}
		return nil
	},
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the local daemon in the background if it is not running",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		if _, st, err := daemon.Dial(); err == nil {
			fmt.Fprintf(out, "the cproxy daemon is already running (%s, pid %d, http://%s)\n", st.Version, st.PID, st.Addr)
			return nil
		}
		_, st, err := wrap.ConnectDaemon(os.Environ())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "started the cproxy daemon (%s, pid %d, http://%s)\n", st.Version, st.PID, st.Addr)
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the local daemon; streams in flight finish first",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		st, ok, err := wrap.StopDaemon()
		switch {
		case err != nil:
			return err
		case !ok:
			fmt.Fprintln(out, "no cproxy daemon is running")
			return nil
		}
		fmt.Fprintf(out, "stopped the cproxy daemon (pid %d); requests it was serving finish in the background.\n"+
			"The next claude launch starts it again.\n", st.PID)
		return nil
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the local daemon on the installed cproxy, keeping streams",
	Long: `Restart the local daemon: a fresh daemon of the cproxy binary now
installed (say, after ` + "`cproxy update`" + `) takes over the port and keeps the
configuration the old one ran with, while the old one finishes the streams
it was serving. Starts one when none runs.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		prev, next, err := wrap.RestartDaemon(os.Environ())
		if err != nil {
			return err
		}
		if prev.PID == 0 {
			fmt.Fprintf(out, "no cproxy daemon was running; started one (%s, pid %d, http://%s)\n", next.Version, next.PID, next.Addr)
			return nil
		}
		fmt.Fprintf(out, "restarted the cproxy daemon: %s (pid %d) → %s (pid %d) on http://%s\n",
			prev.Version, prev.PID, next.Version, next.PID, next.Addr)
		if next.Addr != prev.Addr {
			fmt.Fprintf(out, "its address moved from %s (the port was busy); running claude sessions keep the old one until relaunched\n", prev.Addr)
		}
		return nil
	},
}

var (
	daemonLogLines  int
	daemonLogFollow bool
)

var daemonLogsCmd = &cobra.Command{
	Use:   "logs [-n N] [-f]",
	Short: "Print the local daemon's log",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		path := daemon.LogPath()
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) && !daemonLogFollow {
			fmt.Fprintf(out, "no daemon log yet (%s)\n", path)
			return nil
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		out.Write(lastLines(b, daemonLogLines))
		if !daemonLogFollow {
			return nil
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		return followFile(ctx, path, int64(len(b)), out)
	},
}

func init() {
	daemonLogsCmd.Flags().IntVarP(&daemonLogLines, "lines", "n", 50, "how many of the last lines to print")
	daemonLogsCmd.Flags().BoolVarP(&daemonLogFollow, "follow", "f", false, "keep printing new lines as they are logged")
	daemonCmd.AddCommand(daemonStatusCmd, daemonStartCmd, daemonStopCmd, daemonRestartCmd, daemonLogsCmd)
}

// lastLines returns the last n lines of b.
func lastLines(b []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	i := end
	for count := 0; i > 0; i-- {
		if b[i-1] == '\n' {
			if count++; count == n {
				break
			}
		}
	}
	return b[i:]
}

// followFile prints what is appended to path from offset on, like tail -f,
// starting over when the file is truncated or replaced.
func followFile(ctx context.Context, path string, offset int64, out io.Writer) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if f, err := os.Open(path); err == nil {
			if fi, err := f.Stat(); err == nil && fi.Size() < offset {
				offset = 0
			}
			f.Seek(offset, io.SeekStart)
			n, _ := io.Copy(out, f)
			offset += n
			f.Close()
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil
		}
	}
}
