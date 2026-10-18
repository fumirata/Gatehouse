// Package probe answers two separate questions:
//
//  1. Is the internet actually reachable, or is something standing in the way?
//  2. Is the campus portal host present at all?
//
// The two are deliberately distinct. Question 1 is what decides whether we do
// anything; question 2 is the gate that stops a campus password being handed to
// a hotel or airport portal on some other network.
package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sagan/gatehouse/config"
)

const (
	probeTimeout = 5 * time.Second
	dialTimeout  = 3 * time.Second
	maxBodyBytes = 64 << 10 // portals serve HTML; we only need enough to classify
)

// HTTPDoer is the part of *http.Client the probes use, so tests can substitute
// a fake without a network.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// DialFunc matches net.Dialer.DialContext, and is injectable for the same reason.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Kind classifies the outcome of the internet check.
type Kind int

const (
	// Online means the probe returned exactly what a working connection returns.
	Online Kind = iota
	// Intercepted means something answered, but not with the expected payload —
	// in practice a captive portal getting in the way.
	Intercepted
	// NoNetwork means the request never got an answer at all (DNS, TCP, TLS,
	// timeout). This is the "genuinely offline" case.
	NoNetwork
	// Misconfigured means the probe URL answered with neither the expected
	// payload nor anything portal-shaped. The network is probably fine and the
	// probe settings are wrong, which the UI should say plainly rather than
	// reporting a phantom outage.
	Misconfigured
)

func (k Kind) String() string {
	switch k {
	case Online:
		return "online"
	case Intercepted:
		return "intercepted"
	case NoNetwork:
		return "no network"
	case Misconfigured:
		return "misconfigured"
	default:
		return "unknown"
	}
}

// Result is the full outcome, carrying enough detail for the UI to explain
// itself rather than showing a bare "offline".
type Result struct {
	Kind         Kind
	StatusCode   int
	RedirectHost string // set when a 3xx redirected us somewhere else
	Detail       string
}

// NewClient builds the HTTP client used for probes.
//
// Redirects are deliberately not followed: a captive portal announces itself by
// redirecting, and following the redirect would replace that clear signal with
// an opaque HTML page. Keep-alives are off so every probe performs a fresh DNS
// lookup and TCP connect — a pooled connection can sail straight past the
// interception we are trying to detect.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			Proxy:             http.ProxyFromEnvironment,
		},
	}
}

// Internet performs the connectivity check described by cfg.
func Internet(ctx context.Context, client HTTPDoer, cfg config.InternetProbe) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return Result{Kind: Misconfigured, Detail: fmt.Sprintf("probe URL is invalid: %v", err)}
	}
	req.Header.Set("User-Agent", config.DefaultUserAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return Result{Kind: NoNetwork, Detail: describeNetErr(err)}
	}
	defer resp.Body.Close()

	// A redirect is the clearest possible sign of a captive portal.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		host := ""
		if loc := resp.Header.Get("Location"); loc != "" {
			if u, perr := url.Parse(loc); perr == nil {
				host = u.Host
			}
		}
		detail := "redirected to another host"
		if host != "" {
			detail = "redirected to " + host
		}
		return Result{Kind: Intercepted, StatusCode: resp.StatusCode, RedirectHost: host, Detail: detail}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Result{Kind: NoNetwork, StatusCode: resp.StatusCode, Detail: fmt.Sprintf("reading response: %v", err)}
	}

	expectStatus := cfg.ExpectStatus
	if expectStatus == 0 {
		expectStatus = http.StatusOK
	}
	bodyOK := cfg.ExpectBody == "" || strings.Contains(string(body), cfg.ExpectBody)

	if resp.StatusCode == expectStatus && bodyOK {
		return Result{Kind: Online, StatusCode: resp.StatusCode}
	}

	// Anything HTML where a known payload was expected is a portal, whether or
	// not it happens to look like a login form.
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		detail := "an HTML page answered instead of the expected response"
		if looksLikeLoginPage(body) {
			detail = "a login page answered instead of the expected response"
		}
		return Result{Kind: Intercepted, StatusCode: resp.StatusCode, Detail: detail}
	}

	return Result{
		Kind:       Misconfigured,
		StatusCode: resp.StatusCode,
		Detail: fmt.Sprintf("probe returned HTTP %d, expected HTTP %d — check the probe settings",
			resp.StatusCode, expectStatus),
	}
}

// PortalReachable reports whether the login host answers at the TCP level, and
// returns the address it tried.
//
// A TCP connect rather than an HTTP request: the point is to prove the host is
// *there*, not that it likes our request. A captive portal answers before
// authentication by definition, and a HEAD to a login endpoint often returns
// 405 while still proving reachability.
func PortalReachable(ctx context.Context, dial DialFunc, rawURL string) (bool, string, error) {
	addr, err := hostPort(rawURL)
	if err != nil {
		return false, "", err
	}

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return false, addr, err
	}
	conn.Close()
	return true, addr, nil
}

// hostPort turns a URL into a dialable host:port, defaulting the port from the
// scheme.
func hostPort(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("no host in %q", rawURL)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// looksLikeLoginPage is a weak heuristic used only to make the UI's wording
// more specific than "an HTML page answered".
func looksLikeLoginPage(body []byte) bool {
	lower := bytes.ToLower(body)
	for _, marker := range []string{`type="password"`, `type='password'`, "<form", "login", "portal"} {
		if bytes.Contains(lower, []byte(marker)) {
			return true
		}
	}
	return false
}

// describeNetErr turns a transport failure into something a person can act on.
func describeNetErr(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the probe timed out"
	case errors.As(err, &dnsErr):
		return fmt.Sprintf("DNS lookup for %s failed", dnsErr.Name)
	default:
		return err.Error()
	}
}
