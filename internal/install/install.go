// Package install lays down the systemd unit for coding-agent-loop and starts
// it. The unit template is embedded in the binary and rendered against
// whichever account should run the service.
//
// gh and Claude Code store their auth under the account that logged into
// them (~/.config/gh, ~/.claude/.credentials.json), so the service must run
// as that same account or it authenticates as nobody. When --install is run
// via `sudo`, SUDO_USER names that account and Run uses it directly. Only
// when there is no SUDO_USER (already logged in as root, e.g. in a
// container) does this fall back to creating a fresh, isolated system user —
// which then needs its own `gh auth login` / Claude Code login before the
// service can do anything.
package install

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	embedded "github.com/ableinc/coding-agent-loop"
)

//go:embed coding-agent-loop.service
var serviceTemplate []byte

const (
	unitPath      = "/etc/systemd/system/coding-agent-loop.service"
	installRoot   = "/opt/coding-agent-loop"
	binaryName    = "coding-agent-loop"
	dedicatedUser = "coding-agent-loop"
	dedicatedHome = "/home/coding-agent-loop"
)

// target is the account the systemd unit will run as.
type target struct {
	user, group, home string
	uid, gid          int
	// dedicated is true when target is a fresh system user Run must create;
	// false when it is an existing, already-authenticated account.
	dedicated bool
}

// resolveTarget decides who the service should run as. It does not require
// root and makes no changes, so it is also used by PreviewUnit.
func resolveTarget() (target, error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		u, err := user.Lookup(sudoUser)
		if err != nil {
			return target{}, fmt.Errorf("look up SUDO_USER %q: %w", sudoUser, err)
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		group := sudoUser
		if g, err := user.LookupGroupId(u.Gid); err == nil {
			group = g.Name
		}
		return target{user: u.Username, group: group, home: u.HomeDir, uid: uid, gid: gid}, nil
	}
	// No SUDO_USER: fall back to a dedicated, isolated system user. Its
	// uid/gid are not known until ensureServiceUser creates it, so Run
	// resolves them separately in that path.
	return target{user: dedicatedUser, group: dedicatedUser, home: dedicatedHome, dedicated: true}, nil
}

func render(t target) []byte {
	r := strings.NewReplacer("{{USER}}", t.user, "{{GROUP}}", t.group, "{{HOME}}", t.home)
	return []byte(r.Replace(string(serviceTemplate)))
}

// PreviewUnit renders the unit for whichever account --install would use
// right now, without requiring root or making any changes.
func PreviewUnit() ([]byte, error) {
	t, err := resolveTarget()
	if err != nil {
		return nil, err
	}
	return render(t), nil
}

// Options controls what Run copies into place before it writes the unit.
type Options struct {
	// ConfigPath is the config file to install as /opt/coding-agent-loop/config.json.
	// Skipped (with a warning) if empty or missing — the operator can drop one
	// in later; the unit will simply fail to start until they do.
	ConfigPath string
	Log        func(format string, args ...any)
}

// Run installs the systemd unit and starts it. It must run as root, since it
// writes into /etc and /opt and calls systemctl.
func Run(opts Options) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--install must run as root (try: sudo %s --install)", os.Args[0])
	}
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}

	t, err := resolveTarget()
	if err != nil {
		return err
	}

	if t.dedicated {
		log("no SUDO_USER in the environment; creating an isolated service user",
			"user", t.user)
		uid, gid, err := ensureServiceUser(log)
		if err != nil {
			return fmt.Errorf("create service user: %w", err)
		}
		t.uid, t.gid = uid, gid
		log("remember: this user has no gh/claude auth of its own yet",
			"next_step", fmt.Sprintf("sudo -u %s gh auth login", t.user))
	} else {
		log("running as the invoking user, since that account already has gh/claude auth",
			"user", t.user)
	}

	if err := os.MkdirAll(filepath.Join(installRoot, "bin"), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", installRoot, err)
	}
	agentDir := filepath.Join(t.home, ".agent-loop")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", agentDir, err)
	}
	if err := os.Chown(agentDir, t.uid, t.gid); err != nil {
		return fmt.Errorf("chown %s: %w", agentDir, err)
	}

	binDest := filepath.Join(installRoot, "bin", binaryName)
	if err := copySelf(binDest); err != nil {
		return fmt.Errorf("install binary to %s: %w", binDest, err)
	}
	log("binary installed", "path", binDest)

	cfgDest := filepath.Join(installRoot, "config.json")
	if err := installConfig(opts.ConfigPath, cfgDest, t, log); err != nil {
		return fmt.Errorf("install config: %w", err)
	}

	if err := os.WriteFile(unitPath, render(t), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}
	log("unit installed", "path", unitPath, "run_as", t.user)

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

// ensureServiceUser creates the dedicated system user used when there is no
// SUDO_USER to run as, and returns its uid/gid.
func ensureServiceUser(log func(string, ...any)) (uid, gid int, err error) {
	u, err := user.Lookup(dedicatedUser)
	if err != nil {
		if _, lookErr := exec.LookPath("useradd"); lookErr != nil {
			return 0, 0, fmt.Errorf("user %q does not exist and `useradd` is not available; create it manually", dedicatedUser)
		}
		log("creating service user", "user", dedicatedUser)
		cmd := exec.Command("useradd",
			"--system",
			"--home-dir", dedicatedHome,
			"--create-home",
			"--shell", "/usr/sbin/nologin",
			dedicatedUser,
		)
		out, cmdErr := cmd.CombinedOutput()
		if cmdErr != nil {
			return 0, 0, fmt.Errorf("useradd: %w: %s", cmdErr, out)
		}
		u, err = user.Lookup(dedicatedUser)
		if err != nil {
			return 0, 0, err
		}
	}
	uid, _ = strconv.Atoi(u.Uid)
	gid, _ = strconv.Atoi(u.Gid)
	return uid, gid, nil
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
//
// The systemd unit's ExecStart always passes an explicit --config pointing at
// dest, so dest must exist for the service to start at all: an explicit
// --config path is never allowed to fall back silently (see config.Load).
// When the operator supplied no config (or it couldn't be read), this writes
// the config embedded in the binary at build time instead, so the freshly
// installed service still boots rather than failing until someone drops a
// file in by hand.
func installConfig(configPath, dest string, t target, log func(string, ...any)) error {
	if _, err := os.Stat(dest); err == nil {
		log("config already present, leaving it untouched", "path", dest)
		return nil
	}

	data := embedded.Config
	source := "embedded config (compiled in at build time)"
	if configPath != "" {
		if fileData, err := os.ReadFile(configPath); err != nil {
			log("config not readable, installing the embedded config instead", "source", configPath, "err", err.Error())
		} else {
			data, source = fileData, configPath
		}
	} else {
		log("no config supplied, installing the embedded config instead")
	}

	if err := os.WriteFile(dest, data, 0o640); err != nil {
		return err
	}
	if err := os.Chown(dest, t.uid, t.gid); err != nil {
		return err
	}
	log("config installed", "path", dest, "source", source)
	return nil
}

// installedConfigPath is where Run copies the operator's config.json, and the
// one that actually drives the running service — the definitive source of
// where it was told to keep its state.
var installedConfigPath = filepath.Join(installRoot, "config.json")

// UninstallOptions controls uninstall logging and where to read state paths
// from when the installed config.json is already gone.
type UninstallOptions struct {
	// ConfigPath is consulted for workspace/store paths only when
	// installedConfigPath does not exist. Optional.
	ConfigPath string
	Log        func(format string, args ...any)
}

// Uninstall reverses Run: stops and disables the unit, removes the unit file
// and /opt/coding-agent-loop, and removes exactly the state directories and
// files the operator's config.json told the service to use — workspace.root,
// workspace.repos_root, workspace.logs_root, store.path, and
// claude.usage_cache_path — resolved against the account the service ran as,
// the same way Run resolves it. It never touches claude.credentials_path:
// that file is Claude Code's own login, not something this app created, and
// other tools may depend on it surviving. It must run as root, since it
// touches /etc, /opt, and the service account's files.
func Uninstall(opts UninstallOptions) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--uninstall must run as root (try: sudo %s --uninstall)", os.Args[0])
	}
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}

	if err := runSystemctl(log, "disable", "--now", "coding-agent-loop.service"); err != nil {
		log("service was not running or not enabled, continuing", "detail", err.Error())
	}

	// State paths must be read, and resolved against the service account's
	// home, before /opt/coding-agent-loop (which holds the installed
	// config.json) is removed.
	t, err := resolveTarget()
	if err != nil {
		log("could not resolve the service account; skipping its state directories", "error", err.Error())
	} else {
		removeConfiguredStatePaths(t.home, opts.ConfigPath, log)
	}

	if _, err := os.Stat(unitPath); err == nil {
		if err := os.Remove(unitPath); err != nil {
			return fmt.Errorf("remove %s: %w", unitPath, err)
		}
		log("unit removed", "path", unitPath)
	}
	if err := runSystemctl(log, "daemon-reload"); err != nil {
		log("systemctl daemon-reload failed, continuing", "detail", err.Error())
	}

	if _, err := os.Stat(installRoot); err == nil {
		if err := os.RemoveAll(installRoot); err != nil {
			return fmt.Errorf("remove %s: %w", installRoot, err)
		}
		log("removed", "path", installRoot)
	}

	// The dedicated fallback user, if Run ever created one: its entire home
	// exists solely for this service, so the account and home go together.
	// This is a superset of removeConfiguredStatePaths above when state paths
	// live under that home (the common case), and also mops up anything else
	// under it.
	if _, err := user.Lookup(dedicatedUser); err == nil {
		if _, lookErr := exec.LookPath("userdel"); lookErr != nil {
			log("userdel not available; remove the service user and its home manually",
				"user", dedicatedUser, "home", dedicatedHome)
		} else {
			cmd := exec.Command("userdel", "-r", dedicatedUser)
			if out, err := cmd.CombinedOutput(); err != nil {
				log("could not remove service user, remove it manually",
					"user", dedicatedUser, "error", err.Error(), "output", string(out))
			} else {
				log("service user and home removed", "user", dedicatedUser, "home", dedicatedHome)
			}
		}
	}

	return nil
}

// statePaths is the subset of config.Config that names files/directories the
// running service writes to, as raw strings straight from JSON (an optional
// leading "~" is not yet expanded — resolvePaths does that against the
// service account's home, not whichever account happens to run --uninstall).
type statePaths struct {
	Workspace struct {
		Root      string `json:"root"`
		ReposRoot string `json:"repos_root"`
		LogsRoot  string `json:"logs_root"`
	} `json:"workspace"`
	Store struct {
		Path string `json:"path"`
	} `json:"store"`
	Claude struct {
		UsageCachePath string `json:"usage_cache_path"`
	} `json:"claude"`
}

// defaultStatePaths mirrors config.Default()'s values for the fields above,
// so a missing/unreadable config.json still resolves to where the service
// would have kept its state out of the box.
func defaultStatePaths() statePaths {
	var s statePaths
	s.Workspace.Root = "~/.agent-loop/work"
	s.Workspace.ReposRoot = "~/.agent-loop/repos"
	s.Workspace.LogsRoot = "~/.agent-loop/logs"
	s.Store.Path = "~/.agent-loop/state.db"
	s.Claude.UsageCachePath = "~/.agent-loop/usage-cache.json"
	return s
}

// loadStatePaths reads workspace/store/usage-cache paths from the first of
// installedConfigPath or fallbackConfigPath that exists and parses, falling
// back to defaultStatePaths otherwise. It deliberately does not use
// config.Load: that requires github.owners and other unrelated fields to
// validate, and a leftover or partially-removed config here must not block
// cleanup of the directories it names.
func loadStatePaths(fallbackConfigPath string, log func(string, ...any)) statePaths {
	paths := defaultStatePaths()
	for _, candidate := range []string{installedConfigPath, fallbackConfigPath} {
		if candidate == "" {
			continue
		}
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &paths); err != nil {
			log("could not parse, falling back to defaults for state paths", "path", candidate, "error", err.Error())
			continue
		}
		log("read state paths", "path", candidate)
		return paths
	}
	log("no config.json found; assuming default state paths under ~/.agent-loop")
	return paths
}

// expandHome resolves a leading "~" against home — not os.UserHomeDir(),
// which under `sudo` resolves to root's home rather than the service
// account's, silently pointing cleanup at the wrong directory.
func expandHome(p, home string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// removeConfiguredStatePaths removes exactly the directories/file the
// service's own config told it to use, resolved against home.
func removeConfiguredStatePaths(home, fallbackConfigPath string, log func(string, ...any)) {
	paths := loadStatePaths(fallbackConfigPath, log)
	removeDir(expandHome(paths.Workspace.Root, home), log)
	removeDir(expandHome(paths.Workspace.ReposRoot, home), log)
	removeDir(expandHome(paths.Workspace.LogsRoot, home), log)
	removeFile(expandHome(paths.Store.Path, home), log)
	removeFile(expandHome(paths.Claude.UsageCachePath, home), log)
}

func removeDir(dir string, log func(string, ...any)) {
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log("could not remove, remove it manually", "path", dir, "error", err.Error())
		return
	}
	log("removed", "path", dir)
}

func removeFile(path string, log func(string, ...any)) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := os.Remove(path); err != nil {
		log("could not remove, remove it manually", "path", path, "error", err.Error())
		return
	}
	log("removed", "path", path)
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
