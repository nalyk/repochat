package cli

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The helper is OpenAI's official tunnel-client. repochat can install it
// privately; by default it installs exactly this version and verifies the
// download against the SHA-256 values below, which are compiled into repochat.
const (
	helperPinnedVersion = "0.0.21"
	helperName          = "tunnel-client"
	helperRepoURL       = "https://github.com/openai/tunnel-client"

	maxHelperDownload = 80 << 20
	maxHelperBinary   = 200 << 20
	maxHelperEntries  = 64
)

// helperMin is the oldest tunnel-client repochat has been verified against.
var helperMin = [3]int{0, 0, 21}

var helperPinnedSHA256 = map[string]string{
	"linux-amd64":  "9e91a539e3ae1c436d499f216a1d7514b3c86cc7779032f41b2a05821d74b03a",
	"linux-arm64":  "78a5202e0737199208a0c3e8f7c123805634546b0a587b83303b973b7f56aca6",
	"darwin-amd64": "4aff53fb903374b83284657c39cf940e7cb9cfebfb7b2eb8c2e85da94c583770",
	"darwin-arm64": "353a535d2513f599f9fe7897606e89b3248611453b30203ebddfb6e62fd4f58b",
}

// Hosts a helper download may be fetched from, including redirects.
var helperHosts = map[string]bool{
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

var helperVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func helperPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

func helperInstallDir() (string, error) {
	d, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "bin"), nil
}

// findHelper locates tunnel-client: an explicit override, then repochat's
// private copy, then PATH.
func findHelper() (path, source string, err error) {
	if p := os.Getenv("REPOCHAT_TUNNEL_CLIENT"); p != "" {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return "", "", fmt.Errorf("REPOCHAT_TUNNEL_CLIENT=%s is not a file", p)
		}
		// One binary, whatever directory it is later started from: the
		// version check runs here, tunnel-client itself in the repository
		// root, and a relative path would name two different files.
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", "", fmt.Errorf("REPOCHAT_TUNNEL_CLIENT=%s: %v", p, err)
		}
		return abs, "REPOCHAT_TUNNEL_CLIENT", nil
	}
	if dir, err := helperInstallDir(); err == nil {
		p := filepath.Join(dir, helperName)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, "installed by repochat", nil
		}
	}
	if p, err := exec.LookPath(helperName); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return p, "PATH", nil
	}
	return "", "", errHelperMissing
}

var errHelperMissing = errors.New("tunnel-client is not installed")

// helperEnv is the environment for every tunnel-client invocation: a small
// allowlist, so stray CONTROL_PLANE_*, MCP_*, HEALTH_*, LOG_* or admin-key
// variables from the user's shell can never change what tunnel-client does.
func helperEnv(extra ...string) []string {
	var env []string
	for _, k := range []string{
		"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TERM",
		"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
		"REPOCHAT_CONFIG_DIR", "REPOCHAT_STATE_DIR", "REPOCHAT_DATA_DIR", "NO_COLOR",
		"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

var versionNumberRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// helperVersion runs `tunnel-client --version`.
func helperVersion(ctx context.Context, path string) (text string, v [3]int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = helperEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", v, fmt.Errorf("cannot run %s --version: %v", path, err)
	}
	text = strings.TrimSpace(firstLine(string(out)))
	m := versionNumberRe.FindStringSubmatch(text)
	if m == nil {
		return text, v, nil // development build: version unknown
	}
	for i := 0; i < 3; i++ {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return text, v, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func versionOlder(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// helperHTTPClient only follows https redirects to allowlisted hosts.
func helperHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return checkHelperURL(req.URL.Scheme, req.URL.Hostname())
		},
	}
}

func checkHelperURL(scheme, host string) error {
	if scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS download URL (%s)", scheme)
	}
	if !helperHosts[strings.ToLower(host)] {
		return fmt.Errorf("refusing to download from unexpected host %q", host)
	}
	return nil
}

func helperAssetName(version string) string {
	return fmt.Sprintf("tunnel-client-v%s-%s.zip", version, helperPlatform())
}

func helperAssetURL(version, asset string) string {
	return fmt.Sprintf("%s/releases/download/v%s/%s", helperRepoURL, version, asset)
}

// httpGetLimited downloads url into w, failing if it exceeds limit bytes.
func httpGetLimited(ctx context.Context, client *http.Client, url string, w io.Writer, limit int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if err := checkHelperURL(req.URL.Scheme, req.URL.Hostname()); err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "repochat-installer")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download failed: HTTP %d for %s", resp.StatusCode, url)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("download is larger than the %d MB limit", limit>>20)
	}
	return n, nil
}

// expectedHelperSHA returns the SHA-256 the download must have and whether it
// comes from repochat's embedded pins (true) or only from the release's own
// checksum file (false).
func expectedHelperSHA(ctx context.Context, client *http.Client, version string) (sum string, pinned bool, err error) {
	if version == helperPinnedVersion {
		sum, ok := helperPinnedSHA256[helperPlatform()]
		if !ok {
			return "", false, fmt.Errorf("no tunnel-client build is pinned for %s; install it manually from %s/releases and set REPOCHAT_TUNNEL_CLIENT", helperPlatform(), helperRepoURL)
		}
		return sum, true, nil
	}
	var buf strings.Builder
	if _, err := httpGetLimited(ctx, client, helperAssetURL(version, "SHA256SUMS.txt"), &buf, 1<<20); err != nil {
		return "", false, fmt.Errorf("cannot fetch the release checksums: %w", err)
	}
	asset := helperAssetName(version)
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && len(f[0]) == 64 {
			return strings.ToLower(f[0]), false, nil
		}
	}
	return "", false, fmt.Errorf("release v%s has no checksum for %s", version, asset)
}

// installHelper downloads, verifies and installs tunnel-client privately.
// Nothing is downloaded without the user's consent.
func installHelper(ctx context.Context, c *console, version string) (string, error) {
	if version == "" {
		version = helperPinnedVersion
	}
	version = strings.TrimPrefix(version, "v")
	if !helperVersionRe.MatchString(version) {
		return "", fmt.Errorf("invalid version %q (expected something like %s)", version, helperPinnedVersion)
	}
	dir, err := helperInstallDir()
	if err != nil {
		return "", err
	}
	client := helperHTTPClient()
	want, pinned, err := expectedHelperSHA(ctx, client, version)
	if err != nil {
		return "", err
	}
	asset := helperAssetName(version)
	url := helperAssetURL(version, asset)

	c.say("")
	c.say("repochat needs OpenAI's official %s (it carries the connection to ChatGPT).", c.bold(helperName))
	c.say("  download   %s", url)
	if pinned {
		c.say("  verify     SHA-256 %s… (pinned inside repochat)", want[:16])
	} else {
		c.say("  verify     SHA-256 %s… — taken from that release's own SHA256SUMS.txt,", want[:16])
		c.say("             NOT pinned in repochat: this only detects a corrupted download.")
	}
	c.say("  install to %s", filepath.Join(dir, helperName))
	if !c.confirm("Download and install it now?", true) {
		if c.interrupted {
			return "", errInterrupted
		}
		if !c.tty && !c.yes {
			return "", errors.New("tunnel-client is not installed. Run `repochat install-helper --yes`, or install it yourself and put it on PATH")
		}
		return "", errors.New("not installed. You can install tunnel-client yourself from " + helperRepoURL + "/releases and put it on PATH")
	}

	if err := privateDir(dir); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	archive, err := os.CreateTemp(dir, ".download-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	c.say("Downloading…")
	h := sha256.New()
	size, err := httpGetLimited(ctx, client, url, io.MultiWriter(archive, h), maxHelperDownload)
	if err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s. Nothing was installed", asset, want, got)
	}
	c.ok("downloaded %.1f MB, checksum verified", float64(size)/(1<<20))

	dest := filepath.Join(dir, helperName)
	if err := extractHelper(archive, size, dir, dest); err != nil {
		return "", err
	}
	if text, _, err := helperVersion(ctx, dest); err != nil {
		return "", fmt.Errorf("the installed helper does not run: %w", err)
	} else {
		c.ok("installed %s %s", helperName, text)
	}
	return dest, nil
}

// extractHelper extracts the single tunnel-client binary from the archive
// into dest, atomically. Anything unexpected about the archive is an error.
func extractHelper(archive *os.File, size int64, dir, dest string) error {
	zr, err := zip.NewReader(archive, size)
	if err != nil {
		return fmt.Errorf("the download is not a valid zip archive: %w", err)
	}
	if len(zr.File) > maxHelperEntries {
		return fmt.Errorf("unexpected archive: %d entries", len(zr.File))
	}
	var bin *zip.File
	for _, f := range zr.File {
		if f.Name != helperName {
			continue
		}
		if bin != nil {
			return errors.New("unexpected archive: more than one tunnel-client entry")
		}
		if !f.Mode().IsRegular() {
			return errors.New("unexpected archive: tunnel-client is not a regular file")
		}
		bin = f
	}
	if bin == nil {
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		return fmt.Errorf("unexpected archive: no %s binary at its root (found: %s)", helperName, strings.Join(names, ", "))
	}
	if bin.UncompressedSize64 > maxHelperBinary {
		return errors.New("unexpected archive: the binary is implausibly large")
	}
	src, err := bin.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".tunnel-client-*.tmp") // O_EXCL, mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(src, maxHelperBinary+1))
	if err == nil && n > maxHelperBinary {
		err = errors.New("unexpected archive: the binary is implausibly large")
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// ensureHelper returns a usable tunnel-client, offering to install or upgrade
// it when needed.
func ensureHelper(ctx context.Context, c *console) (path, version string, err error) {
	path, _, err = findHelper()
	if errors.Is(err, errHelperMissing) {
		path, err = installHelper(ctx, c, "")
	}
	if err != nil {
		return "", "", err
	}
	text, v, err := helperVersion(ctx, path)
	if err != nil {
		return "", "", err
	}
	if v != [3]int{} && versionOlder(v, helperMin) {
		c.warn("%s at %s is version %s; repochat needs %d.%d.%d or newer.", helperName, path, text, helperMin[0], helperMin[1], helperMin[2])
		newPath, ierr := installHelper(ctx, c, "")
		if ierr != nil {
			return "", "", fmt.Errorf("tunnel-client %s is too old: %w", text, ierr)
		}
		path = newPath
		if text, _, err = helperVersion(ctx, path); err != nil {
			return "", "", err
		}
	}
	return path, text, nil
}
