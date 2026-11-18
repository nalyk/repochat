package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

const configVersion = 1

// Profile is one tunnel a user can serve repositories through.
type Profile struct {
	TunnelID   string `json:"tunnel_id"`
	APIKey     string `json:"api_key"`
	TunnelName string `json:"tunnel_name,omitempty"`
	// Onboarded is set once a tool call has been served through this tunnel,
	// i.e. the ChatGPT side is known to work. Until then the plugin-creation
	// steps are shown on every start.
	Onboarded bool `json:"onboarded,omitempty"`
	// BoundRepo pins the profile to one repository: it is selected
	// automatically there and refuses to serve any other.
	BoundRepo string `json:"bound_repo,omitempty"`
	LastRepo  string `json:"last_repo,omitempty"`
}

// Config is the user's saved configuration.
type Config struct {
	Version        int                 `json:"version"`
	DefaultProfile string              `json:"default_profile,omitempty"`
	Profiles       map[string]*Profile `json:"profiles"`
	Allow          []string            `json:"allow,omitempty"`
	Deny           []string            `json:"deny,omitempty"`
}

var (
	// Same shapes tunnel-client itself accepts.
	tunnelIDRe = regexp.MustCompile(`^tunnel_([a-z0-9]{4}_)?[a-z0-9]{32}$`)
	apiKeyRe   = regexp.MustCompile(`^[0-9A-Za-z_-]{20,400}$`)
	profileRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
)

func configDir() (string, error) {
	if d := os.Getenv("REPOCHAT_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the user configuration directory: %w", err)
	}
	return filepath.Join(base, "repochat"), nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// stateDir holds the audit log, logs and per-tunnel runtime files.
func stateDir() (string, error) {
	if d := os.Getenv("REPOCHAT_STATE_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "repochat"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "repochat"), nil
}

// dataDir holds the privately installed tunnel-client helper.
func dataDir() (string, error) {
	if d := os.Getenv("REPOCHAT_DATA_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "repochat"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "repochat"), nil
}

// privateDir creates dir (0700) if needed and verifies that it is a real
// directory owned by the current user that nobody else can enter.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s must be a real directory, not a symlink", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by you", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("%s is accessible to other users and could not be fixed: %w", dir, err)
		}
	}
	return nil
}

// loadConfig reads the configuration. A missing file is an empty config.
// A file other users can read, or that is not a plain file owned by the
// user, is rejected: it holds an API key.
func loadConfig() (*Config, error) {
	cfg := &Config{Version: configVersion, Profiles: map[string]*Profile{}}
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s is not owned by you; refusing to use it", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users (it contains an API key). Fix with: chmod 600 %s", path, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v). Fix it or delete it and run `repochat setup`", path, err)
	}
	if cfg.Version > configVersion {
		return nil, fmt.Errorf("%s was written by a newer repochat (config version %d); upgrade repochat", path, cfg.Version)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	for name, p := range cfg.Profiles {
		if p == nil {
			delete(cfg.Profiles, name)
		}
	}
	return cfg, nil
}

// saveConfig writes the configuration atomically with mode 0600.
func saveConfig(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := privateDir(dir); err != nil {
		return err
	}
	cfg.Version = configVersion
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// updateConfig changes the saved configuration safely when several repochat
// processes run at once: under an exclusive lock it re-reads the file, applies
// the change to that fresh copy and writes it back. Writing back a copy read
// earlier would undo whatever another instance saved in the meantime (for
// example a profile added while this one was running).
func updateConfig(change func(*Config)) (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".config.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	change(cfg)
	if err := saveConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// updateProfile applies a change to one saved profile, if it still exists and
// still refers to the same tunnel.
func updateProfile(name, tunnelID string, change func(*Profile)) error {
	_, err := updateConfig(func(c *Config) {
		if p := c.Profiles[name]; p != nil && p.TunnelID == tunnelID {
			change(p)
		}
	})
	return err
}

func (c *Config) profileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (c *Config) defaultName() string {
	if c.DefaultProfile != "" {
		return c.DefaultProfile
	}
	return "default"
}

// selectProfile picks the profile for a repository: an explicit name wins,
// then a profile bound to this repository, then the default profile.
// The returned profile may be nil when nothing has been set up yet.
func (c *Config) selectProfile(explicit, repoRoot string) (name string, p *Profile, err error) {
	if explicit != "" {
		if !profileRe.MatchString(explicit) {
			return "", nil, fmt.Errorf("invalid profile name %q", explicit)
		}
		name = explicit
	} else {
		for _, n := range c.profileNames() {
			if b := c.Profiles[n].BoundRepo; b != "" && b == repoRoot {
				name = n
				break
			}
		}
		if name == "" {
			name = c.defaultName()
		}
	}
	p = c.Profiles[name]
	if p != nil && p.BoundRepo != "" && repoRoot != "" && p.BoundRepo != repoRoot {
		return name, nil, fmt.Errorf("profile %q is bound to %s and will not serve another repository. Use --profile NAME for a different tunnel, or `repochat setup --profile %s --unbind`", name, p.BoundRepo, name)
	}
	return name, p, nil
}

// tunnelKey is a filesystem-safe identifier for a tunnel's runtime files.
func tunnelKey(tunnelID string) string {
	sum := sha256.Sum256([]byte(tunnelID))
	return hex.EncodeToString(sum[:])[:16]
}

// maskID shortens an identifier for display.
func maskID(id string) string {
	if len(id) <= 14 {
		return id
	}
	return id[:10] + "…" + id[len(id)-4:]
}

// credentials resolved for a run, and where each value came from.
type credentials struct {
	TunnelID, APIKey string
	FromEnv          bool
}

// resolveCredentials prefers a complete pair from the environment, then the
// profile. ok is false when no complete pair is available.
func resolveCredentials(p *Profile) (credentials, bool) {
	id := strings.TrimSpace(os.Getenv("CONTROL_PLANE_TUNNEL_ID"))
	key := strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_KEY"))
	if id != "" && key != "" {
		return credentials{TunnelID: id, APIKey: key, FromEnv: true}, true
	}
	if p != nil && p.TunnelID != "" && p.APIKey != "" {
		return credentials{TunnelID: p.TunnelID, APIKey: p.APIKey}, true
	}
	return credentials{}, false
}

func validateCredentials(c credentials) error {
	if !tunnelIDRe.MatchString(c.TunnelID) {
		return errors.New("the tunnel ID does not look right (expected tunnel_ followed by 32 lowercase letters/digits)")
	}
	if !apiKeyRe.MatchString(c.APIKey) {
		return errors.New("the API key does not look right (letters, digits, '-' and '_' only)")
	}
	return nil
}
