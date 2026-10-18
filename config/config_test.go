package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempConfig(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.json")
}

// A first run must not be an error: the app opens its settings window instead of
// failing to start.
func TestMissingFileYieldsDefaults(t *testing.T) {
	cfg, err := LoadFrom(tempConfig(t))
	if err != nil {
		t.Fatalf("LoadFrom on a missing file: %v", err)
	}
	if cfg.InternetProbe.URL != DefaultInternetProbeURL {
		t.Errorf("probe URL = %q, want the default", cfg.InternetProbe.URL)
	}
	if cfg.InternetProbe.ExpectStatus != 200 {
		t.Errorf("expected status = %d, want 200", cfg.InternetProbe.ExpectStatus)
	}
	if cfg.PollInterval() != DefaultPollInterval {
		t.Errorf("poll interval = %v, want %v", cfg.PollInterval(), DefaultPollInterval)
	}

	// And it must be recognisably unconfigured, because there are no credentials.
	if err := cfg.Validate(); err == nil {
		t.Error("a fresh config should not validate as ready to use")
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := tempConfig(t)

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Username = "2026000000@cmcc"
	loaded.Password = "s3cret"
	loaded.PollIntervalSeconds = 42
	loaded.Autostart = true

	if err := loaded.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom after Save: %v", err)
	}
	if again.Username != "2026000000@cmcc" || again.Password != "s3cret" {
		t.Errorf("credentials did not survive: %q / %q", again.Username, again.Password)
	}
	if again.PollIntervalSeconds != 42 {
		t.Errorf("poll interval = %d, want 42", again.PollIntervalSeconds)
	}
	if !again.Autostart {
		t.Error("autostart did not survive")
	}
}

// A config written by an older build carries a "login" object that no longer
// exists. It must be ignored rather than break loading.
func TestLoadIgnoresLeftoverFields(t *testing.T) {
	path := tempConfig(t)
	body := `{
	  "username": "2026000000@cmcc",
	  "password": "s3cret",
	  "portal_url": "http://10.255.0.19/",
	  "login": {"url": "http://old.example/login", "method": "POST", "body": "x={username}"}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("leftover fields should be ignored, got: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}

	msg := err.Error()
	for _, want := range []string{"username", "password"} {
		if !strings.Contains(msg, want) {
			t.Errorf("validation message %q does not mention %q", msg, want)
		}
	}
}

// An empty expected body is a legitimate choice, not a missing value: it is
// exactly what a 204 endpoint returns. Defaulting it away would silently break
// every preset that uses one.
func TestEmptyExpectedBodySurvivesAReload(t *testing.T) {
	path := tempConfig(t)

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Username = "alice"
	cfg.Password = "s3cret"
	cfg.InternetProbe.URL = "http://connect.rom.miui.com/generate_204"
	cfg.InternetProbe.ExpectStatus = 204
	cfg.InternetProbe.ExpectBody = "" // deliberately empty

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if again.InternetProbe.ExpectBody != "" {
		t.Errorf("expected body = %q after a reload, want it left empty", again.InternetProbe.ExpectBody)
	}
	if again.InternetProbe.URL != cfg.InternetProbe.URL {
		t.Errorf("probe URL = %q, want %q", again.InternetProbe.URL, cfg.InternetProbe.URL)
	}
	if again.InternetProbe.ExpectStatus != 204 {
		t.Errorf("expected status = %d, want 204", again.InternetProbe.ExpectStatus)
	}
}

// A config that never mentions a probe still gets the whole default block.
func TestAnAbsentProbeBlockGetsFullyDefaulted(t *testing.T) {
	cfg, err := LoadFrom(tempConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternetProbe.URL != DefaultInternetProbeURL {
		t.Errorf("probe URL = %q, want the default", cfg.InternetProbe.URL)
	}
	if cfg.InternetProbe.ExpectStatus != 200 {
		t.Errorf("expected status = %d, want 200", cfg.InternetProbe.ExpectStatus)
	}
	if cfg.InternetProbe.ExpectBody != DefaultInternetExpectBody {
		t.Errorf("expected body = %q, want the default", cfg.InternetProbe.ExpectBody)
	}
}

func TestAccountNameAppendsTheCarrierSuffix(t *testing.T) {
	tests := []struct {
		name     string
		username string
		carrier  string
		want     string
	}{
		{"suffix appended", "2026000000", "@cmcc", "2026000000@cmcc"},
		{"other outlet", "2026000000", "@aust", "2026000000@aust"},
		{"missing @ is tolerated", "2026000000", "unicom", "2026000000@unicom"},
		{"no carrier set", "2026000000", "", "2026000000"},
		// An account typed the old way, with the suffix already attached, must
		// not gain a second one.
		{"already suffixed is left alone", "2026000000@cmcc", "@unicom", "2026000000@cmcc"},
		{"whitespace trimmed", "  2026000000  ", "@cmcc", "2026000000@cmcc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Username: tc.username, Carrier: tc.carrier}
			if got := cfg.AccountName(); got != tc.want {
				t.Errorf("AccountName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCarrierDefaultsToThePortalsDefaultOutlet(t *testing.T) {
	cfg, err := LoadFrom(tempConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Carrier != DefaultCarrier {
		t.Errorf("carrier = %q, want %q", cfg.Carrier, DefaultCarrier)
	}

	// And every offered outlet should be one the portal actually knows.
	found := false
	for _, c := range Carriers {
		if c.Suffix == DefaultCarrier {
			found = true
		}
	}
	if !found {
		t.Errorf("default carrier %q is not in Carriers", DefaultCarrier)
	}
}

// The portal address is compiled in, not configured, so there is nothing for a
// user to get wrong here.
func TestPortalAddressIsFixed(t *testing.T) {
	if PortalURL != "http://10.255.0.19/" {
		t.Errorf("PortalURL = %q, want the campus portal", PortalURL)
	}
	if got, want := LoginURL(), "http://10.255.0.19/drcom/login"; got != want {
		t.Errorf("LoginURL() = %q, want %q", got, want)
	}
	if got, want := PortalRoot(), "http://10.255.0.19/"; got != want {
		t.Errorf("PortalRoot() = %q, want %q", got, want)
	}
}

func TestPlainHTTPPortalIsAWarningNotAnError(t *testing.T) {
	cfg := &Config{Username: "a", Password: "b"}
	cfg.applyDefaults()

	// Campus portals are reached over plain HTTP on a LAN address, so this must
	// not block anything — it is surfaced as a warning instead.
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected a plain-HTTP portal: %v", err)
	}
	warnings := cfg.Warnings()
	if len(warnings) == 0 || !strings.Contains(warnings[0], "unencrypted") {
		t.Errorf("expected a warning about plain HTTP, got %v", warnings)
	}
}

func TestCloneIsIndependent(t *testing.T) {
	cfg := &Config{Username: "alice", Password: "s3cret"}
	clone := cfg.Clone()

	clone.Username = "changed"
	clone.Password = "changed"

	if cfg.Username != "alice" || cfg.Password != "s3cret" {
		t.Error("mutating the clone changed the original")
	}
}

func TestRedactedHidesThePassword(t *testing.T) {
	cfg := &Config{Username: "alice", Password: "s3cret"}

	red := cfg.Redacted()
	if red.Password == "s3cret" {
		t.Error("Redacted left the password in place")
	}
	if red.Username != "alice" {
		t.Error("Redacted should leave the username alone")
	}
	if cfg.Password != "s3cret" {
		t.Error("Redacted mutated the original")
	}
}

func TestResetClearsCredentialsButKeepsTuning(t *testing.T) {
	cfg := &Config{Username: "a", Password: "b", PollIntervalSeconds: 30}
	cfg.Reset()

	if cfg.Username != "" || cfg.Password != "" {
		t.Error("Reset left the credentials in place")
	}
	if cfg.PollIntervalSeconds != 30 {
		t.Errorf("poll interval = %d, want the tuning to be preserved", cfg.PollIntervalSeconds)
	}
}

func TestCorruptFileIsAnError(t *testing.T) {
	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadFrom(path); err == nil {
		t.Error("expected a corrupt config file to be reported")
	}
}

func TestSaveLeavesNoTemporaryFiles(t *testing.T) {
	path := tempConfig(t)

	cfg, _ := LoadFrom(path)
	cfg.Username = "alice"
	cfg.Password = "s3cret"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("unexpected leftover file %q after an atomic save", e.Name())
		}
	}
}
