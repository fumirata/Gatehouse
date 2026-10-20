// Package config loads and saves Gatehouse's settings.
//
// Settings live in a single JSON file under %APPDATA%\Gatehouse. The password is
// stored in plaintext by the user's explicit choice; the mitigation for that is
// that credentials are only ever sent to the host named by Login.URL (see the
// login package).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// AppDirName is the folder created under the user's config directory.
	AppDirName = "Gatehouse"
	// FileName is the settings file name.
	FileName = "config.json"

	// DefaultSessionPollInterval is how often the portal is asked whether this
	// machine is signed in.
	//
	// The check is a LAN round trip that answers in single-digit milliseconds at
	// best and ~100ms typically, so polling this often costs almost nothing and
	// is what lets the app react to an expired session in seconds rather than
	// tens of seconds.
	DefaultSessionPollInterval = 5 * time.Second

	// SessionPollIntervalMin and SessionPollIntervalMax bound the setting. Below
	// the minimum the app would issue requests faster than it can act on them;
	// above the maximum it is slower than the design it replaced.
	SessionPollIntervalMin = 1
	SessionPollIntervalMax = 60

	// PortalURL is this campus's Dr.COM portal. It is a constant rather than a
	// setting: Gatehouse exists for one network.
	PortalURL = "http://10.255.0.19/"

	// LoginPath is the Dr.COM login endpoint, relative to the portal address.
	LoginPath = "/drcom/login"

	// SessionPath reports whether this client currently has an authenticated
	// session. The callback parameter is not optional: the portal answers HTTP
	// 400 without it.
	SessionPath = "drcom/chkstatus?callback=dr1002"

	// DefaultUserAgent is sent with the portal status check.
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Gatehouse/1.0"

	// BrowserUserAgent is sent with the login request.
	//
	// This is the exact string a browser sent when the portal's login was
	// captured, and it is reproduced verbatim on purpose: portals routinely
	// branch on the User-Agent, and a request that does not look like the one the
	// portal expects is a silent way to be refused.
	BrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

	// DefaultCarrier is the portal's default outlet (学生移动出口).
	DefaultCarrier = "@cmcc"
)

// Carrier is one of the portal's service outlets. The portal appends the suffix
// to the account number when it submits the login form.
type Carrier struct {
	Suffix string `json:"suffix"`
	Name   string `json:"name"`
}

// Carriers lists the outlets offered on the portal's own login page.
//
// The order matches the portal's dropdown so the UI reads the same way.
var Carriers = []Carrier{
	{Suffix: "@aust", Name: "学生电信出口"},
	{Suffix: "@unicom", Name: "学生联通出口"},
	{Suffix: "@cmcc", Name: "学生移动出口"},
}

// Config is the whole of Gatehouse's persisted state.
//
// The portal address and the login request are both fixed: they describe this
// campus's Dr.COM portal and the handshake it expects, and are compiled in
// rather than configured. Only the credentials and a little tuning vary.
type Config struct {
	Username string `json:"username"`
	Password string `json:"password"`

	// Carrier is the service outlet suffix appended to the username, e.g. @cmcc.
	// The portal does this itself when you pick an outlet on its login page.
	Carrier string `json:"carrier"`

	// SessionPollIntervalSeconds is how often the portal is asked whether this
	// machine is signed in. Clamped to [SessionPollIntervalMin,
	// SessionPollIntervalMax] on load.
	SessionPollIntervalSeconds int `json:"session_poll_interval_seconds"`

	Autostart bool `json:"autostart"`

	// path remembers where this config was loaded from so Save does not have to
	// re-derive it (and so tests can save to a temp file).
	path string `json:"-"`
}

// Path returns the location of the config file.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, AppDirName, FileName), nil
}

// Load reads the config from its default location.
//
// A missing file is not an error: the returned Config has defaults applied and
// fails Validate, which the caller treats as "first run, open the settings
// window" rather than as a crash.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom reads the config from an explicit path. It exists so tests can point
// at a temp file.
func LoadFrom(path string) (*Config, error) {
	cfg := &Config{path: path}

	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// First run. Defaults only; the caller will see Validate fail and open
		// the settings window.
		cfg.applyDefaults()
		return cfg, nil
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// Save writes the config to the path it was loaded from.
func (c *Config) Save() error {
	if c.path == "" {
		path, err := Path()
		if err != nil {
			return err
		}
		c.path = path
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory %s: %w", dir, err)
	}

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	b = append(b, '\n')

	// Write to a sibling temp file and rename, so an interrupted save cannot
	// leave a half-written config behind.
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("replace config %s: %w", c.path, err)
	}
	return nil
}

// Validate reports problems that make logging in impossible. Every problem is
// returned at once so the settings window can show them together.
func (c *Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.Username) == "" {
		problems = append(problems, "username is not set")
	}
	if c.Password == "" {
		problems = append(problems, "password is not set")
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Reset clears the user-supplied fields, leaving the probe tuning in place.
func (c *Config) Reset() {
	c.Username = ""
	c.Password = ""
	c.applyDefaults()
}

// Warnings returns non-fatal issues worth showing in the settings window but
// which must not block saving.
func (c *Config) Warnings() []string {
	// The portal is a LAN address reached over plain HTTP, so the password
	// travels unencrypted. That is expected for a campus portal, but it is worth
	// stating once.
	return []string{
		"The portal uses plain HTTP, so the password is sent unencrypted. " +
			"This is normal for a campus portal.",
	}
}

// SessionPollInterval returns how long to wait between portal status checks.
//
// The value is clamped rather than rejected: a hand-edited file or an odd form
// submission should not stop the app working, and a value outside the bounds is
// far more likely to be a mistake than an intention.
func (c *Config) SessionPollInterval() time.Duration {
	n := c.SessionPollIntervalSeconds
	switch {
	case n <= 0:
		return DefaultSessionPollInterval
	case n < SessionPollIntervalMin:
		n = SessionPollIntervalMin
	case n > SessionPollIntervalMax:
		n = SessionPollIntervalMax
	}
	return time.Duration(n) * time.Second
}

// SessionURL is the portal endpoint that reports this client's session state.
func SessionURL() string { return PortalURL + SessionPath }

// AccountName is the username as the portal expects it: the bare account number
// with the selected carrier suffix.
//
// The suffix is added here rather than stored with the username so that changing
// outlets does not mean retyping the account. A username that already carries a
// suffix is left alone, so an account typed the old way keeps working.
func (c *Config) AccountName() string {
	user := strings.TrimSpace(c.Username)
	suffix := strings.TrimSpace(c.Carrier)

	if suffix == "" || strings.Contains(user, "@") {
		return user
	}
	if !strings.HasPrefix(suffix, "@") {
		suffix = "@" + suffix
	}
	return user + suffix
}

// LoginURL is the Dr.COM login endpoint.
//
// This is the address the reachability gate checks and the only address
// credentials are ever sent to.
func LoginURL() string { return PortalURL + strings.TrimPrefix(LoginPath, "/") }

// PortalRoot is the portal's base address, used as the reachability target and
// sent as the Referer.
func PortalRoot() string { return PortalURL }

// Redacted returns a copy safe to log or send to the UI: the password is
// replaced with a fixed marker.
func (c *Config) Redacted() *Config {
	cp := *c
	if cp.Password != "" {
		cp.Password = "******"
	}
	return &cp
}

// Clone returns a deep copy, so the monitor goroutine can read a stable
// snapshot while the UI replaces the live config.
func (c *Config) Clone() *Config {
	cp := *c
	return &cp
}

// applyDefaults fills in every field that was absent or left blank, so both a
// first run and a hand-edited file end up usable.
func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.Carrier) == "" {
		c.Carrier = DefaultCarrier
	}

	if c.SessionPollIntervalSeconds <= 0 {
		c.SessionPollIntervalSeconds = int(DefaultSessionPollInterval / time.Second)
	}
}
