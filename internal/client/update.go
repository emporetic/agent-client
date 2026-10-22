package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Version is injected by the release build. Source builds never self-update.
var Version = "dev"
var stableVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Release struct {
	Version string         `json:"version"`
	Assets  []ReleaseAsset `json:"assets"`
}
type ReleaseAsset struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"download_url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size_bytes"`
}
type updateResult struct {
	path string
	err  error
}

func newer(a, b string) bool {
	aa, bb := stableVersion.FindStringSubmatch(a), stableVersion.FindStringSubmatch(b)
	if aa == nil || bb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		// Compare decimal lengths first: no integer overflow, no lexical version ordering.
		if len(aa[i]) != len(bb[i]) {
			return len(aa[i]) > len(bb[i])
		}
		if aa[i] != bb[i] {
			return aa[i] > bb[i]
		}
	}
	return false
}

func bundleRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	if filepath.Base(filepath.Dir(dir)) != "versions" || filepath.Base(dir) != Version || !stableVersion.MatchString(Version) {
		return "", fmt.Errorf("install a complete stable release package to enable updates")
	}
	return filepath.Dir(filepath.Dir(dir)), nil
}

// Pin bundled adapter arguments to this process's version. Other instances may
// switch current while a task still uses imports from the old examples directory.
func pinBundleCommands(c *Config) {
	root, err := bundleRoot()
	if err != nil {
		return
	}
	for _, command := range [][]string{c.Command, c.JudgeCommand, c.PickCommand} {
		for i, arg := range command {
			if !filepath.IsAbs(arg) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(arg)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(filepath.Join(root, "versions"), resolved)
			if err != nil {
				continue
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) >= 3 && stableVersion.MatchString(parts[0]) && (parts[1] == "examples" || parts[1] == ".agents") {
				command[i] = filepath.Join(root, "versions", Version, filepath.Join(parts[1:]...))
			}
		}
	}

}

func stageUpdate(ctx context.Context, release Release) (string, error) {
	root, err := bundleRoot()
	if err != nil {
		return "", err
	}
	h := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		u := req.URL
		if len(via) > 5 || u.Scheme != "https" || u.User != nil || !(u.Host == "github.com" || u.Host == "release-assets.githubusercontent.com" || u.Host == "objects.githubusercontent.com") {
			return fmt.Errorf("untrusted download redirect")
		}
		return nil
	}}
	return stageBundle(ctx, root, release, h)
}

func stageBundle(ctx context.Context, root string, release Release, h *http.Client) (string, error) {
	if !newer(release.Version, Version) {
		return "", fmt.Errorf("not a newer stable version")
	}
	var asset *ReleaseAsset
	for i := range release.Assets {
		a := &release.Assets[i]
		if a.OS == runtime.GOOS && a.Arch == runtime.GOARCH {
			asset = a
			break
		}
	}
	if asset == nil {
		return "", fmt.Errorf("no package for this platform")
	}
	expected := "https://github.com/emporetic/agent-client/releases/download/" + release.Version + "/emporetic-agent_" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	digest, err := hex.DecodeString(asset.SHA256)
	if err != nil || len(digest) != 32 || asset.URL != expected || asset.Size <= 0 || asset.Size > 128<<20 {
		return "", fmt.Errorf("invalid release metadata")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", expected, nil)
	if err != nil {
		return "", err
	}

	resp, err := h.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	archive, err := os.CreateTemp(root, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, sum), io.LimitReader(resp.Body, asset.Size+1))
	if err != nil {
		return "", err
	}
	if n != asset.Size || hex.EncodeToString(sum.Sum(nil)) != strings.ToLower(asset.SHA256) {
		return "", fmt.Errorf("package checksum or size mismatch")
	}
	if _, err = archive.Seek(0, 0); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Join(root, "versions"), ".stage-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err = extractBundle(archive, stage, release.Version); err != nil {
		return "", err
	}
	if err = checkBundle(ctx, stage, release.Version); err != nil {
		return "", err
	}
	unlock, err := lockFile(filepath.Join(root, ".update.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()
	dest := filepath.Join(root, "versions", release.Version)
	if _, err = os.Stat(dest); err == nil {
		if err = checkBundle(ctx, dest, release.Version); err != nil {
			return "", err
		}
		return dest, nil
	}
	if err = os.Rename(stage, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func extractBundle(src io.Reader, dest, version string) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	prefix := "versions/" + version + "/"
	var total int64
	files := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		files++
		if files > 10000 {
			return fmt.Errorf("too many package entries")
		}
		name := strings.TrimPrefix(h.Name, "./")
		// Root launcher and links are installation scaffolding, never updated from an archive.
		if name == "emporetic-agent" || name == "README.md" || name == "current" || name == "examples" || name == ".agents" || name == "versions/" || name == "versions/"+version+"/" {
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			return fmt.Errorf("unexpected package path %q", name)
		}
		rel := strings.TrimPrefix(name, prefix)
		clean := filepath.Clean(rel)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(rel, "\\") {
			return fmt.Errorf("unsafe package path")
		}
		path := filepath.Join(dest, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > 512<<20 {
				return fmt.Errorf("package too large")
			}
			if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			mode := os.FileMode(0644)
			if clean == "emporetic-agent" || h.Mode&0111 != 0 {
				mode = 0755
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("links and special files are forbidden in version bundles")
		}
	}
}
func checkBundle(ctx context.Context, dir, version string) error {
	for _, name := range []string{"emporetic-agent", ".agents/skills/emporetic-agent/SKILL.md", "examples/README.md", "examples/adapter.mjs", "examples/adapter-args.mjs", "examples/pi-executor.mjs", "examples/pi-picker.mjs", "examples/pi-judge.mjs"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("incomplete bundle: %s", name)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(dir, "emporetic-agent"), "version").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != version {
		return fmt.Errorf("binary version mismatch")
	}
	return nil
}
func switchCurrent(root, target string) error {
	temp := filepath.Join(root, ".current-"+strconv.Itoa(os.Getpid()))
	_ = os.Remove(temp)
	if err := os.Symlink(target, temp); err != nil {
		return err
	}
	defer os.Remove(temp)
	return os.Rename(temp, filepath.Join(root, "current"))
}
func activateUpdate(ctx context.Context, dir string) error {
	root, err := bundleRoot()
	if err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(root, ".update.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	old, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		return err
	}
	version := filepath.Base(dir)
	current := filepath.Base(old)
	if newer(current, version) {
		version = current
		dir = filepath.Join(root, "versions", current)
	}
	if err = checkBundle(ctx, dir, version); err != nil {
		return err
	}
	if err = switchCurrent(root, filepath.Join("versions", version)); err != nil {
		return err
	}
	err = syscall.Exec(filepath.Join(dir, "emporetic-agent"), os.Args, os.Environ())
	if rollbackErr := switchCurrent(root, old); rollbackErr != nil {
		return fmt.Errorf("exec: %v; rollback: %w", err, rollbackErr)
	}
	return err
}
