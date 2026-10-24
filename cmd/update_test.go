package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dehydr8/cproxy/internal/config"
	"github.com/dehydr8/cproxy/internal/version"
)

func TestUpdate(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	tgz := func(bin string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, f := range []struct{ name, body string }{{"README.md", "readme"}, {"cproxy", bin}} {
			tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(f.body))
		}
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}
	archive := tgz("new binary v1.8.0")
	sum := sha256.Sum256(archive)
	published := hex.EncodeToString(sum[:])
	name := fmt.Sprintf("cproxy-v1.8.0-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v1.8.0", http.StatusFound)
		case "/releases/download/v1.8.0/" + name:
			w.Write(archive)
		case "/releases/download/v1.8.0/" + name + ".sha256":
			fmt.Fprintf(w, "%s  %s\n", published, name)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	oldRepo, oldSelf, oldVersion := releaseRepo, selfPath, version.Version
	t.Cleanup(func() { releaseRepo, selfPath, version.Version = oldRepo, oldSelf, oldVersion })
	releaseRepo = ts.URL
	target := filepath.Join(t.TempDir(), "cproxy")
	selfPath = func() (string, error) { return target, nil }

	run := func(installed string) (string, error) {
		version.Version = installed
		os.WriteFile(target, []byte("old binary"), 0o755)
		var out bytes.Buffer
		updateCmd.SetOut(&out)
		err := runUpdate(updateCmd, nil)
		return out.String(), err
	}
	contents := func() string { b, _ := os.ReadFile(target); return string(b) }

	// A newer release replaces the binary, executable.
	if out, err := run("v1.7.1"); err != nil || !strings.Contains(out, "v1.7.1 → v1.8.0") {
		t.Fatalf("update from v1.7.1: %v\n%s", err, out)
	}
	if fi, _ := os.Stat(target); contents() != "new binary v1.8.0" || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary after update: %q (mode %v)", contents(), fi.Mode())
	}

	// Already on the latest (or newer): nothing is downloaded or replaced.
	for _, v := range []string{"v1.8.0", "v1.10.0"} {
		if out, err := run(v); err != nil || !strings.Contains(out, "up to date") || contents() != "old binary" {
			t.Fatalf("update from %s: %v %q\n%s", v, err, contents(), out)
		}
	}

	// A tarball that does not match its published checksum is not installed.
	published = strings.Repeat("0", 64)
	if _, err := run("v1.7.1"); err == nil || !strings.Contains(err.Error(), "SHA-256") || contents() != "old binary" {
		t.Fatalf("checksum mismatch: err=%v binary=%q", err, contents())
	}
}
