// Package install lays down the systemd unit for coding-agent-loop and starts
// it. The unit file is embedded in the binary, so the paths in it
// (/opt/coding-agent-loop, the coding-agent-loop user) are the contract this
// package installs against — Run does not template the unit, it makes the
// host match it.
package install

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
)

//go:embed coding-agent-loop.service
var ServiceUnit []byte

const (
	unitPath    = "/etc/systemd/system/coding-agent-loop.service"
	installRoot = "/opt/coding-agent-loop"
	serviceUser = "coding-agent-loop"
	serviceHome = "/home/coding-agent-loop"
	binaryName  = "coding-agent-loop"
)

// Options controls what Run copies into place before it writes the unit.
type Options struct {
	// ConfigPath is the config file to install as /opt/coding-agent-loop/config.json.
	// Skipped (with a warning) if empty or missing — the operator can drop one
	// in later; the unit will simply fail to start until they do.
	ConfigPath string
	Log        func(format string, args ...any)
}

// Run installs the systemd unit and starts it. It must run as root, since it
// creates a system user, writes into /etc and /opt, and calls systemctl.
func Run(opts Options) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--install must run as root (try: sudo %s --install)", os.Args[0])
	}
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}

	if err := ensureServiceUser(log); err != nil {
		return fmt.Errorf("create service user: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(installRoot, "bin"), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", installRoot, err)
	}
	if err := os.MkdirAll(filepath.Join(serviceHome, ".agent-loop"), 0o755); err != nil {
		return fmt.Errorf("create %s/.agent-loop: %w", serviceHome, err)
	}
	if err := chownToService(filepath.Join(serviceHome, ".agent-loop")); err != nil {
		return fmt.Errorf("chown %s/.agent-loop: %w", serviceHome, err)
	}

	binDest := filepath.Join(installRoot, "bin", binaryName)
	if err := copySelf(binDest); err != nil {
		return fmt.Errorf("install binary to %s: %w", binDest, err)
	}
	log("binary installed", "path", binDest)

	cfgDest := filepath.Join(installRoot, "config.json")
	if err := installConfig(opts.ConfigPath, cfgDest, log); err != nil {
		return fmt.Errorf("install config: %w", err)
	}

	if err := os.WriteFile(unitPath, ServiceUnit, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}
	log("unit installed", "path", unitPath)

	if err := runSystemctl(log, "daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl(log, "enable", "--now", "coding-agent-loop.service"); err != nil {
		return err
	}
	if err := runSystemctl(log, "is-active", "--quiet", "coding-agent-loop.service"); err != nil {
		return fmt.Errorf("service did not reach active state — check `journalctl -u coding-agent-loop`: %w", err)
	}
	log("service enabled and running")
	return nil
}

func ensureServiceUser(log func(string, ...any)) error {
	if _, err := user.Lookup(serviceUser); err == nil {
		return nil
	}
	if _, err := exec.LookPath("useradd"); err != nil {
		return fmt.Errorf("user %q does not exist and `useradd` is not available; create it manually", serviceUser)
	}
	log("creating service user", "user", serviceUser)
	cmd := exec.Command("useradd",
		"--system",
		"--home-dir", serviceHome,
		"--create-home",
		"--shell", "/usr/sbin/nologin",
		serviceUser,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd: %w: %s", err, out)
	}
	return nil
}

func chownToService(path string) error {
	u, err := user.Lookup(serviceUser)
	if err != nil {
		return err
	}
	var uid, gid int
	if _, err := fmt.Sscanf(u.Uid, "%d", &uid); err != nil {
		return err
	}
	if _, err := fmt.Sscanf(u.Gid, "%d", &gid); err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

// copySelf copies the currently running binary to dest, since that is the
// binary --install was invoked from.
func copySelf(dest string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	src, err = filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// installConfig copies configPath to dest unless dest already exists — an
// operator's live config must never be clobbered by a re-run of --install.
func installConfig(configPath, dest string, log func(string, ...any)) error {
	if _, err := os.Stat(dest); err == nil {
		log("config already present, leaving it untouched", "path", dest)
		return nil
	}
	if configPath == "" {
		log("no config supplied; drop one at " + dest + " before starting the service")
		return nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		log("config not readable; drop one at "+dest+" before starting the service", "source", configPath, "err", err.Error())
		return nil
	}
	if err := os.WriteFile(dest, data, 0o640); err != nil {
		return err
	}
	if err := chownToService(dest); err != nil {
		return err
	}
	log("config installed", "path", dest)
	return nil
}

func runSystemctl(log func(string, ...any), args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
	}
	log("systemctl "+argsString(args), "output", string(out))
	return nil
}

func argsString(args []string) string {
	s := ""
	for i, a := range args {
		if i > 0 {
			s += " "
		}
		s += a
	}
	return s
}
