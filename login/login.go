// Package login performs the Dr.COM captive-portal login.
//
// The request shape is fixed rather than configurable: it is the handshake this
// campus's portal expects, captured from a real browser session. Only the portal
// address and the credentials vary.
//
//	GET /drcom/login?callback=dr1003&DDDDD=<user>&upass=<pass>
//	    &0MKKey=123456&R1=0&R3=0&R6=0&para=00&v6ip=&v=<cache-buster>
//
// The portal answers with JSONP: dr1003({...}), where "result":1 and
// "login_result":1 mean success.
//
// Two guarantees are enforced structurally here rather than by heuristic:
//
//   - Credentials are only ever sent to the configured portal. There is no code
//     path that sends them anywhere else.
//   - Redirects are never followed, so a 302 cannot bounce the credential-bearing
//     request to a third party. Use NewClient to get a client that enforces this;
//     Outcome.ForeignHost flags it if a caller supplies a permissive client.
package login

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sagan/gatehouse/config"
)

const (
	// Timeout for the login request itself.
	Timeout = 10 * time.Second
	// maxBodyBytes bounds how much of the response we read.
	maxBodyBytes = 256 << 10

	// callback is the JSONP function name the portal wraps its reply in.
	callback = "dr1003"
	// accountField and passwordField are Dr.COM's names for the credentials.
	accountField  = "DDDDD"
	passwordField = "upass"
	// staticParams are the fixed fields a browser sends. 0MKKey is the submit
	// button's value; R1/R3/R6 and para are portal defaults.
	staticParams = "0MKKey=123456&R1=0&R3=0&R6=0&para=00&v6ip="
)

// DrCOM's reply is JSONP, so the payload has to be read out of the wrapper.
//
// Success requires login_result to be present: a bare "result":1 only says the
// portal understood the request, not that it accepted the credentials. Treating
// that as success would make every "portal answered but the network never
// opened" case look like a pattern/network disagreement.
var (
	successPattern = regexp.MustCompile(`"login_result"\s*:\s*[12]`)
	failurePattern = regexp.MustCompile(`"result"\s*:\s*0|密码错误|用户不存在|验证失败|账号或密码`)
)

// HTTPDoer is the part of *http.Client this package needs.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Verdict is what the response tells us about the credentials.
type Verdict int

const (
	// VerdictUnknown means nothing conclusive matched. Only a connectivity
	// re-probe can settle it.
	VerdictUnknown Verdict = iota
	// VerdictRejected means the portal definitively refused the credentials, so
	// retrying will not help until the user fixes something.
	VerdictRejected
	// VerdictAccepted means the success marker matched. Still only advisory —
	// the re-probe is what actually proves we are online.
	VerdictAccepted
)

func (v Verdict) String() string {
	switch v {
	case VerdictAccepted:
		return "accepted"
	case VerdictRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// Outcome is the delivered response, described as facts rather than a verdict,
// so the caller decides policy.
type Outcome struct {
	StatusCode int
	Body       []byte
	Location   string // set when the portal answered with a redirect
	Elapsed    time.Duration
	// ForeignHost is set when the response came from a host other than the
	// portal's. With NewClient that cannot happen; it is a tripwire for a caller
	// that passed a redirect-following client.
	ForeignHost string
}

// NewClient builds the HTTP client the login request must use.
//
// CheckRedirect is the load-bearing part: Go re-sends the request on 307/308 by
// default, which would hand the password to whatever host the redirect names.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			Proxy:             http.ProxyFromEnvironment,
		},
	}
}

// BuildQuery returns the query string for the login request.
//
// The credentials are escaped: an unescaped "&" or "=" in a password would
// silently split the query and produce a failure that looks like wrong
// credentials. The v= value is regenerated per attempt because the portal uses
// it as a cache-buster.
func BuildQuery(username, password string) string {
	return "callback=" + callback +
		"&" + accountField + "=" + url.QueryEscape(username) +
		"&" + passwordField + "=" + url.QueryEscape(password) +
		"&" + staticParams +
		"&v=" + strconv.FormatInt(rand.Int64N(90000)+10000, 10)
}

// Execute performs the Dr.COM login request.
//
// An error means the request never produced a response. A response saying "wrong
// password" is not an error — it is an Outcome.
func Execute(ctx context.Context, client HTTPDoer, username, password string) (*Outcome, error) {
	return Executef(ctx, client, config.LoginURL(), config.PortalRoot(), username, password)
}

// Executef takes the endpoint explicitly. Production reaches it through Execute,
// which uses the fixed campus portal; the monitor injects a local address in
// tests. Nothing else in the program constructs a login target.
func Executef(ctx context.Context, client HTTPDoer, endpoint, referer, username, password string) (*Outcome, error) {
	target := endpoint + "?" + BuildQuery(username, password)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("login: build request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", config.BrowserUserAgent)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// Deliberately does not include the URL: it carries the password.
		return nil, fmt.Errorf("login: request to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("login: read response: %w", err)
	}

	out := &Outcome{
		StatusCode: resp.StatusCode,
		Body:       body,
		Location:   resp.Header.Get("Location"),
		Elapsed:    time.Since(start),
	}
	if resp.Request != nil && resp.Request.URL != nil {
		if want := hostOf(endpoint); want != "" && !strings.EqualFold(resp.Request.URL.Host, want) {
			out.ForeignHost = resp.Request.URL.Host
		}
	}
	return out, nil
}

// Classify interprets a Dr.COM response.
//
// Status codes come first because they are unambiguous, then the failure marker,
// then the success marker. Order matters: a portal that answers "wrong password"
// is a real rejection, and treating it as transient would mean retrying forever.
func (o *Outcome) Classify() Verdict {
	if o.StatusCode == http.StatusUnauthorized || o.StatusCode == http.StatusForbidden {
		return VerdictRejected
	}
	if failurePattern.Match(o.Body) {
		return VerdictRejected
	}
	if successPattern.Match(o.Body) {
		return VerdictAccepted
	}
	return VerdictUnknown
}

// Summary is a one-line description for logs and the UI.
func (o *Outcome) Summary() string {
	parts := []string{fmt.Sprintf("HTTP %d", o.StatusCode)}
	if o.Location != "" && o.StatusCode >= 300 && o.StatusCode < 400 {
		parts = append(parts, "redirect to "+o.Location)
	}
	if o.ForeignHost != "" {
		parts = append(parts, "WARNING: response came from "+o.ForeignHost)
	}
	return strings.Join(parts, ", ")
}

// Reply returns a short, readable excerpt of the portal's answer. It is what the
// attempt history shows, and it is how a rejected login gets diagnosed.
func (o *Outcome) Reply() string {
	const max = 200
	s := strings.TrimSpace(string(o.Body))
	if s == "" {
		return ""
	}
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func hostOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return u.Host
}
