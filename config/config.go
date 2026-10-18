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
	"net/http"
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

	// DefaultInternetProbeURL is Microsoft's NCSI endpoint. It is used instead of
	// connectivitycheck.gstatic.com on purpose: gstatic is unreachable from
	// mainland China, which would make every single check look like an outage.
	DefaultInternetProbeURL = "http://www.msftconnecttest.com/connecttest.txt"
	// DefaultInternetExpectBody is the exact payload NCSI returns when the
	// internet is genuinely reachable.
	DefaultInternetExpectBody = "Microsoft Connect Test"

	// DefaultPollInterval is how often the network is re-checked.
	DefaultPollInterval = 15 * time.Second

	// PortalURL is this campus's Dr.COM portal. It is a constant rather than a
	// setting: Gatehouse exists for one network.
	PortalURL = "http://10.255.0.19/"

	// LoginPath is the Dr.COM login endpoint, relative to the portal address.
	LoginPath = "/drcom/login"

	// DefaultUserAgent is sent with probes. Some portals gate on the browser-like
	// shape of this header, so it deliberately does not look like a bare Go client.
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Gatehouse/1.0"

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

	InternetProbe InternetProbe `json:"internet_probe"`

	PollIntervalSeconds int  `json:"poll_interval_seconds"`
	Autostart           bool `json:"autostart"`

	// path remembers where this config was loaded from so Save does not have to
	// re-derive it (and so tests can save to a temp file).
	path string `json:"-"`
}

// InternetProbe describes how "do I have internet?" is decided.
type InternetProbe struct {
	URL          string `json:"url"`
	ExpectStatus int    `json:"expect_status"`
	ExpectBody   string `json:"expect_body"`
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

// PollInterval returns the configured poll interval as a duration.
func (c *Config) PollInterval() time.Duration {
	if c.PollIntervalSeconds <= 0 {
		return DefaultPollInterval
	}
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

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
	if strings.TrimSpace(c.InternetProbe.URL) == "" {
		c.InternetProbe.URL = DefaultInternetProbeURL
	}
	if c.InternetProbe.ExpectStatus == 0 {
		c.InternetProbe.ExpectStatus = http.StatusOK
	}
	if c.InternetProbe.ExpectBody == "" {
		c.InternetProbe.ExpectBody = DefaultInternetExpectBody
	}

	if strings.TrimSpace(c.Carrier) == "" {
		c.Carrier = DefaultCarrier
	}

	if c.PollIntervalSeconds <= 0 {
		c.PollIntervalSeconds = int(DefaultPollInterval / time.Second)
	}
}
