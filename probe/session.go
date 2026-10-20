package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/sagan/gatehouse/config"
)

// sessionTimeout bounds the portal status check. It is a LAN request that
// normally answers in single-digit milliseconds; the bound exists so a dead
// portal cannot stall a poll cycle.
const sessionTimeout = 4 * time.Second

// SessionKind is what the portal says about this machine's session.
type SessionKind int

const (
	// SessionOnline means the portal reports an authenticated session for this
	// client address.
	SessionOnline SessionKind = iota
	// SessionOffline means the portal answered and says this client is not
	// authenticated.
	//
	// Unlike a failed internet probe, this is unambiguous: the portal is
	// reachable, it processed the request, and it says we are not signed in. It
	// is therefore safe to act on immediately, with no confirmation.
	SessionOffline
	// SessionUnreachable means the portal did not answer at all — in practice,
	// this machine is not on the campus network.
	SessionUnreachable
	// SessionUnreadable means the portal answered, but not in a way we
	// understand: an HTTP error, an HTML page, a captive-portal interception, or
	// a payload without a recognisable result field.
	//
	// This must never be treated as "offline". Doing so would let a portal error
	// page or an interception page trigger a login on every poll cycle.
	SessionUnreadable
)

func (k SessionKind) String() string {
	switch k {
	case SessionOnline:
		return "online"
	case SessionOffline:
		return "offline"
	case SessionUnreachable:
		return "unreachable"
	default:
		return "unreadable"
	}
}

// Session is the outcome of asking the portal about this machine's session.
type Session struct {
	Kind SessionKind
	// UID is the account the portal reports as signed in, when it says so.
	UID string
	// OnlineSeconds is how long that session has been up.
	OnlineSeconds int64
	// Detail explains a non-obvious outcome for the UI and the log.
	Detail string
}

// The portal answers in JSONP: dr1002({...}). result:1 means authenticated and
// result:0 means not — both taken from the portal's own login script, which
// branches on exactly this field to choose between the login page and the
// "already online" page.
var (
	sessionResultPattern = regexp.MustCompile(`"result"\s*:\s*(-?\d+)`)
	sessionUIDPattern    = regexp.MustCompile(`"uid"\s*:\s*"([^"]*)"`)
	sessionTimePattern   = regexp.MustCompile(`"oltime"\s*:\s*(\d+)`)
)

// SessionStatus asks the portal whether this machine is currently signed in.
//
// This is the cheapest and most direct way to know whether a login is needed:
// one LAN round trip, no credentials, and the answer is the portal's own verdict
// rather than an inference from whether the internet happens to work.
func SessionStatus(ctx context.Context, client HTTPDoer) Session {
	return SessionStatusAt(ctx, client, config.SessionURL())
}

// SessionStatusAt is SessionStatus against an explicit endpoint, so tests can
// point at a fake portal. Production always reaches it through SessionStatus,
// which uses the fixed campus address.
func SessionStatusAt(ctx context.Context, client HTTPDoer, url string) Session {
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Session{Kind: SessionUnreadable, Detail: fmt.Sprintf("status URL is invalid: %v", err)}
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", config.PortalRoot())
	req.Header.Set("User-Agent", config.DefaultUserAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return Session{Kind: SessionUnreachable, Detail: describeNetErr(err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Session{Kind: SessionUnreachable, Detail: fmt.Sprintf("reading response: %v", err)}
	}

	if resp.StatusCode != http.StatusOK {
		return Session{
			Kind:   SessionUnreadable,
			Detail: fmt.Sprintf("the portal's status check returned HTTP %d", resp.StatusCode),
		}
	}

	return parseSession(body)
}

// parseSession reads the JSONP payload out of a status response.
func parseSession(body []byte) Session {
	m := sessionResultPattern.FindSubmatch(body)
	if m == nil {
		return Session{Kind: SessionUnreadable, Detail: "the portal's status reply was not understood"}
	}

	code, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return Session{Kind: SessionUnreadable, Detail: "the portal's status code was not a number"}
	}

	switch code {
	case 1:
		s := Session{Kind: SessionOnline}
		if uid := sessionUIDPattern.FindSubmatch(body); uid != nil {
			s.UID = string(uid[1])
		}
		if t := sessionTimePattern.FindSubmatch(body); t != nil {
			if n, err := strconv.ParseInt(string(t[1]), 10, 64); err == nil {
				s.OnlineSeconds = n
			}
		}
		return s
	case 0:
		return Session{Kind: SessionOffline}
	default:
		return Session{
			Kind:   SessionUnreadable,
			Detail: fmt.Sprintf("the portal reported status %d", code),
		}
	}
}
