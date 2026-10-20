// Package probe asks the campus portal whether this machine is signed in.
//
// There is deliberately no internet probe. Asking the portal directly is both
// faster (a LAN round trip instead of an internet one) and unambiguous: the
// portal knows whether it has an authenticated session for this client, whereas
// a failed internet check only implies that *something* is wrong.
//
// The trade is that off-campus protection now rests on the portal being
// unreachable rather than on a second opinion. A Dr.COM portal on a LAN address
// simply does not answer from a hotel or airport network, so nothing is sent
// there — but that is the mechanism, and it is worth knowing.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	// maxBodyBytes bounds how much of a reply we read. Portals serve small
	// payloads; this is only a guard against a pathological response.
	maxBodyBytes = 64 << 10
)

// HTTPDoer is the part of *http.Client the probes use, so tests can substitute
// a fake without a network.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// DialFunc matches net.Dialer.DialContext, and is injectable for the same reason.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// NewClient builds the HTTP client used for portal status checks.
//
// Redirects are not followed: a captive portal announces itself by redirecting,
// and following it would turn a clear "this is not our portal" signal into an
// opaque HTML page. Keep-alives are off so every check performs a fresh DNS
// lookup and TCP connect.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			Proxy:             http.ProxyFromEnvironment,
		},
	}
}

// describeNetErr turns a transport failure into something a person can act on.
func describeNetErr(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the portal did not answer in time"
	case errors.As(err, &dnsErr):
		return fmt.Sprintf("DNS lookup for %s failed", dnsErr.Name)
	default:
		return err.Error()
	}
}
