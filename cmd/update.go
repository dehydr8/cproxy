package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dehydr8/cproxy/internal/daemon"
	"github.com/dehydr8/cproxy/internal/version"
)

// releaseRepo is where releases are published; tests point it elsewhere.
var releaseRepo = "https://github.com/dehydr8/cproxy"

// selfPath is the binary update replaces; tests point it elsewhere.
var selfPath = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

var (
	updateCheck bool
	updateForce bool
)

var updateCmd = &cobra.Command{
	Use:   "update [--check] [--force]",
	Short: "Update cproxy to the latest release",
	Long: `Update cproxy in place to the latest GitHub release: the release
tarball for this platform is downloaded, checked against its published
SHA-256 and swapped in for the running binary. A cproxy installed with
` + "`go install`" + ` is updated with ` + "`go install`" + ` instead.

The local daemon keeps running the old version until the next claude launch
hands it over to the new one, so streams in flight are never cut.
--check only reports whether an update is available; --force also replaces
a development build.`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheck, "check", false, "only report whether a newer release is out")
	updateCmd.Flags().BoolVar(&updateForce, "force", false, "install the latest release even over a development build or the same version")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	current := version.String()
	latest, err := latestRelease()
	if err != nil {
		return fmt.Errorf("find the latest cproxy release: %w", err)
	}
	release := !strings.HasPrefix(current, "dev")
	switch {
	case release && !daemon.Newer(latest, current) && !updateForce:
		fmt.Fprintf(out, "cproxy %s is up to date (latest release: %s)\n", current, latest)
		return nil
	case updateCheck:
		fmt.Fprintf(out, "cproxy %s is available (installed: %s); run `cproxy update` to install it\n", latest, current)
		return nil
	case !release && !updateForce:
		return fmt.Errorf("this cproxy (%s) is a development build; pass --force to replace it with release %s", current, latest)
	}

	if goInstalled() {
		fmt.Fprintf(out, "updating cproxy %s → %s with go install\n", current, latest)
		gi := exec.Command("go", "install", "github.com/dehydr8/cproxy@"+latest)
		gi.Stdout, gi.Stderr = out, cmd.ErrOrStderr()
		if err := gi.Run(); err != nil {
			return fmt.Errorf("go install github.com/dehydr8/cproxy@%s: %w", latest, err)
		}
	} else {
		target, err := selfPath()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "downloading cproxy %s for %s/%s…\n", latest, runtime.GOOS, runtime.GOARCH)
		bin, err := downloadRelease(latest)
		if err != nil {
			return err
		}
		if err := replaceFile(target, bin); err != nil {
			return err
		}
		fmt.Fprintf(out, "updated %s: cproxy %s → %s\n", target, current, latest)
	}
	if _, st, err := daemon.Dial(); err == nil && st.Version != latest {
		fmt.Fprintf(out, "the running daemon (%s) hands over to %s on the next claude launch\n", st.Version, latest)
	}
	return nil
}

// latestRelease reads the latest release tag from the redirect GitHub
// answers for /releases/latest (no API token or rate limit involved).
func latestRelease() (string, error) {
	c := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(releaseRepo + "/releases/latest")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	tag := path.Base(loc)
	if !strings.Contains(loc, "/releases/tag/") || !daemon.Newer(tag, "v0.0.0") {
		return "", fmt.Errorf("unexpected answer %s (Location %q)", resp.Status, loc)
	}
	return tag, nil
}

// downloadRelease fetches a release's tarball for this platform, checks it
// against its published SHA-256 and returns the cproxy binary inside.
func downloadRelease(tag string) ([]byte, error) {
	name := fmt.Sprintf("cproxy-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
	base := releaseRepo + "/releases/download/" + tag + "/" + name
	tgz, err := httpGet(base)
	if err != nil {
		return nil, err
	}
	sum, err := httpGet(base + ".sha256")
	if err != nil {
		return nil, err
	}
	want, _, _ := strings.Cut(strings.TrimSpace(string(sum)), " ")
	got := sha256.Sum256(tgz)
	if !strings.EqualFold(want, hex.EncodeToString(got[:])) {
		return nil, fmt.Errorf("%s does not match its published SHA-256; not installing it", name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("%s has no cproxy binary: %w", name, err)
		}
		if path.Base(h.Name) == "cproxy" && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
}

func httpGet(url string) ([]byte, error) {
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// replaceFile swaps bin in for target atomically: written beside it, then
// renamed over it, so a running cproxy (or a failed update) never sees a
// half-written binary.
func replaceFile(target string, bin []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".cproxy-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot write to %s: %w (re-run with the permissions that installed it, e.g. sudo)", filepath.Dir(target), err)
		}
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// goInstalled reports whether this binary came from `go install …@vX.Y.Z`:
// no release version linked in, but a module version in its build info.
func goInstalled() bool {
	if version.Version != "dev" {
		return false
	}
	bi, ok := debug.ReadBuildInfo()
	return ok && daemon.Newer(bi.Main.Version, "v0.0.0")
}
