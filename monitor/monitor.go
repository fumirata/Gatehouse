// Package monitor is the heart of Gatehouse: it decides whether anything needs
// doing, and if so, signs in.
//
// The rule it enforces, in one line:
//
//	only send credentials when the internet is unreachable AND the campus portal
//	host is demonstrably reachable.
//
// That second clause is what keeps a campus password from being handed to a
// hotel, airport, or cafe portal.
package monitor

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sagan/gatehouse/config"
	"github.com/sagan/gatehouse/login"
	"github.com/sagan/gatehouse/probe"
)

// defaultDial is the production portal-reachability dialer.
var defaultDial probe.DialFunc = (&net.Dialer{}).DialContext

const (
	// Transient failures double from baseBackoff to maxBackoff.
	baseBackoff = 5 * time.Second
	maxBackoff  = 300 * time.Second

	// A definitive rejection (wrong password) will not fix itself, so it gets a
	// long fixed delay rather than a short doubling one.
	definitiveBackoff = 300 * time.Second

	// After this many consecutive rejections, stop retrying for a while and
	// leave the problem in front of the user instead of hammering the portal.
	maxDefinitiveStreak = 3
	suspendDuration     = 30 * time.Minute

	// If the success pattern keeps matching while the probe keeps saying we are
	// offline, the pattern or the URL is wrong. Stop and say so.
	maxRegexDisagreements = 3

	// Hard floor between two credential-bearing requests, whatever the backoff
	// says. No code path may post twice inside this window.
	minPostInterval = 10 * time.Second

	// Portals often apply authorisation asynchronously, so the first re-check is
	// immediate and these are the follow-ups.
	verifyDelay1 = 1 * time.Second
	verifyDelay2 = 2 * time.Second

	historyLimit = 20
)

// HTTPDoer is the part of *http.Client this package needs. A single client can
// serve both probing and logging in, since both want redirects disabled.
type HTTPDoer = probe.HTTPDoer

// State is the monitor's externally visible condition.
type State string

const (
	StateUnknown State = "unknown"
	// StateNeedsConfig means there are no credentials to sign in with.
	StateNeedsConfig State = "needs_config"
	// StateUnreadable means the portal answered but not in a way we understand.
	// Nothing is sent: a portal error page or an interception must never be read
	// as "you are offline, sign in".
	StateUnreadable   State = "unreadable"
	StateOnline       State = "online"
	StateNotOnCampus  State = "not_on_campus"
	StateLoggingIn    State = "logging_in"
	StateLoginFailed  State = "login_failed"
	StateAuthRejected State = "auth_rejected"
	StateSuspended    State = "suspended"
)

// Attempt records one login try, for the UI's history list. This is the surface
// that makes a misconfigured portal debuggable.
type Attempt struct {
	At      time.Time     `json:"at"`
	OK      bool          `json:"ok"`
	Verdict string        `json:"verdict"`
	Summary string        `json:"summary"`
	Elapsed time.Duration `json:"elapsed"`
}

// Snapshot is an immutable view of the monitor, safe to hand to the UI.
type Snapshot struct {
	State       State     `json:"state"`
	Detail      string    `json:"detail"`
	SessionKind string    `json:"session_kind"`
	SessionNote string    `json:"session_note"`
	LastCheckAt time.Time `json:"last_check_at"`
	NextRetryAt time.Time `json:"next_retry_at"`
	OnlineSince time.Time `json:"online_since"`
	Consecutive int       `json:"consecutive_failures"`
	ConfigError string    `json:"config_error"`
	LastAttempt *Attempt  `json:"last_attempt,omitempty"`
	History     []Attempt `json:"history"`
	// SessionUID and SessionSeconds are what the portal reports about the
	// current session, for the settings window.
	SessionUID     string `json:"session_uid"`
	SessionSeconds int64  `json:"session_seconds"`
}

// Options are the monitor's dependencies. Everything that touches the outside
// world is injectable so the state machine can be tested exhaustively without a
// campus network.
type Options struct {
	// Config is the starting configuration.
	Config *config.Config
	// Client performs both probes and logins. Use one built with redirects
	// disabled — see probe.NewClient and login.NewClient.
	Client HTTPDoer
	// Dial is used for the portal-reachability check.
	Dial probe.DialFunc
	// Clock is RealClock in production and a FakeClock in tests.
	Clock Clock
	// Log receives structured diagnostics.
	Log *slog.Logger
	// OnChange is called after every state change, on the monitor's goroutine.
	// It must not block: it runs between network operations.
	OnChange func(Snapshot)
	// Notify surfaces a problem to the user. Called at most once per failure
	// streak.
	Notify func(title, body string)
	// Endpoint overrides the portal address. Production leaves it empty so the
	// fixed campus portal in the config package is used; tests point it at a
	// local fake portal.
	Endpoint *Endpoint
}

// Endpoint is where the status check and the login request are aimed.
type Endpoint struct {
	Session string // e.g. http://10.255.0.19/drcom/chkstatus?callback=dr1002
	Login   string // e.g. http://10.255.0.19/drcom/login
}

// campusEndpoint is the fixed portal this application exists for.
func campusEndpoint() Endpoint {
	return Endpoint{Session: config.SessionURL(), Login: config.LoginURL()}
}

// command is a nudge from another goroutine. Routing it through a channel keeps
// all mutable monitor state owned by the Run goroutine.
type command struct {
	resetBackoff bool
}

// Monitor runs the check-and-login loop.
type Monitor struct {
	opts Options
	log  *slog.Logger

	// mu guards cfg and snap, which are read from other goroutines.
	mu   sync.RWMutex
	cfg  *config.Config
	snap Snapshot

	cmd  chan command
	done chan struct{}

	// The fields below are owned exclusively by the Run goroutine; no lock
	// needed, and none may be added without revisiting that invariant.
	backoff          time.Duration
	consecutive      int
	definitiveStreak int
	regexDisagree    int
	notified         bool
	lastPostAt       time.Time
	suspendedUntil   time.Time
	probeFailures    int
}

// New builds a Monitor.
func New(opts Options) *Monitor {
	if opts.Clock == nil {
		opts.Clock = RealClock{}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Client == nil {
		opts.Client = login.NewClient()
	}
	if opts.Dial == nil {
		opts.Dial = defaultDial
	}
	if opts.Config == nil {
		opts.Config = &config.Config{}
	}

	if opts.Endpoint == nil {
		ep := campusEndpoint()
		opts.Endpoint = &ep
	}

	m := &Monitor{
		opts: opts,
		log:  opts.Log,
		cfg:  opts.Config.Clone(),
		cmd:  make(chan command, 1),
		done: make(chan struct{}),
		snap: Snapshot{State: StateUnknown, Detail: "starting up"},
	}
	return m
}

// Done is closed when Run returns.
func (m *Monitor) Done() <-chan struct{} { return m.done }

// Snapshot returns the current state, for the UI's initial load.
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s := m.snap
	s.History = append([]Attempt(nil), m.snap.History...)
	return s
}

// Wake asks for a check as soon as the loop can service it. It never blocks, so
// it is safe to call from a tray-menu handler running on the UI thread.
func (m *Monitor) Wake() { m.enqueue(command{}) }

// LoginNow asks for an immediate attempt, clearing any backoff or suspension.
// This is what the tray's "Reconnect now" and the UI's "Test login" use.
func (m *Monitor) LoginNow() { m.enqueue(command{resetBackoff: true}) }

func (m *Monitor) enqueue(c command) {
	select {
	case m.cmd <- c:
	default:
		// A nudge is already queued; that is enough.
	}
}

// ApplyConfig swaps in new settings. Changing the credentials clears backoff and
// suspension, because whatever the user just fixed is worth retrying at once.
func (m *Monitor) ApplyConfig(cfg *config.Config) {
	next := cfg.Clone()

	m.mu.Lock()
	credentialsChanged := m.cfg == nil ||
		m.cfg.AccountName() != next.AccountName() ||
		m.cfg.Password != next.Password
	m.cfg = next
	m.mu.Unlock()

	if credentialsChanged {
		m.LoginNow()
	} else {
		m.Wake()
	}
}

// Run drives the loop until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	defer close(m.done)

	m.step(ctx)

	for {
		wait := m.nextWait()
		timer := m.opts.Clock.After(wait)

		select {
		case <-ctx.Done():
			return
		case c := <-m.cmd:
			if c.resetBackoff {
				m.resetBackoff()
			}
		case <-timer:
		}

		if ctx.Err() != nil {
			return
		}
		m.step(ctx)
	}
}

// step is one full decision cycle.
func (m *Monitor) step(ctx context.Context) {
	cfg := m.currentConfig()

	if err := cfg.Validate(); err != nil {
		m.update(func(s *Snapshot) {
			s.State = StateNeedsConfig
			s.Detail = "Gatehouse is not configured yet"
			s.ConfigError = err.Error()
			s.LastCheckAt = m.opts.Clock.Now()
		})
		return
	}
	m.update(func(s *Snapshot) { s.ConfigError = "" })

	// Ask the portal directly. There is no internet probe: the portal's own
	// verdict is both faster and unambiguous, which is what lets a login happen
	// on the first cycle after a session expires.
	sess := m.session(ctx)
	m.recordSession(sess)

	switch sess.Kind {
	case probe.SessionOnline:
		m.markOnline(sess)

	case probe.SessionOffline:
		// Authoritative: the portal is reachable and says we are not signed in.
		// No confirmation step — there is nothing ambiguous to confirm.
		m.attemptLogin(ctx, cfg, sess)

	case probe.SessionUnreachable:
		m.update(func(s *Snapshot) {
			s.State = StateNotOnCampus
			s.Detail = "the portal did not answer, so this is not the campus network"
			if sess.Detail != "" {
				s.Detail = "the portal did not answer (" + sess.Detail + "), so this is not the campus network"
			}
		})

	default:
		// Anything we cannot read — an HTTP error, an HTML page, an interception —
		// must never be treated as "offline". Doing so would let a broken portal
		// trigger a login on every cycle.
		m.log.Debug("portal status could not be read", "detail", sess.Detail)
		m.update(func(s *Snapshot) {
			s.State = StateUnreadable
			s.Detail = sess.Detail
			if s.Detail == "" {
				s.Detail = "the portal's status reply could not be read"
			}
		})
	}
}

// session asks the portal about this machine's session, through the injectable
// endpoint so tests can point at a fake portal.
func (m *Monitor) session(ctx context.Context) probe.Session {
	return probe.SessionStatusAt(ctx, m.opts.Client, m.opts.Endpoint.Session)
}

// attemptLogin performs exactly one credential-bearing request and judges it.
func (m *Monitor) attemptLogin(ctx context.Context, cfg *config.Config, sess probe.Session) {
	now := m.opts.Clock.Now()

	// The hard floor. No path posts twice inside minPostInterval, however the
	// backoff was configured or however many times Wake was called.
	if !m.lastPostAt.IsZero() && now.Sub(m.lastPostAt) < minPostInterval {
		m.update(func(s *Snapshot) {
			s.State = StateLoginFailed
			s.Detail = "waiting before the next attempt"
			s.NextRetryAt = m.lastPostAt.Add(minPostInterval)
		})
		return
	}
	m.lastPostAt = now

	m.update(func(s *Snapshot) {
		s.State = StateLoggingIn
		s.Detail = "the portal reports no session — signing in"
		s.NextRetryAt = time.Time{}
	})

	start := m.opts.Clock.Now()
	out, err := login.Executef(ctx, m.opts.Client, m.opts.Endpoint.Login, config.PortalRoot(), cfg.AccountName(), cfg.Password)
	if err != nil {
		m.log.Warn("login request failed", "err", err)
		m.recordAttempt(Attempt{At: start, OK: false, Verdict: "request failed", Summary: err.Error()})
		m.failTransient("the login request did not complete: " + err.Error())
		return
	}

	if out.ForeignHost != "" {
		// Unreachable with login.NewClient; a tripwire for a permissive client.
		m.log.Error("login response came from an unexpected host",
			"host", out.ForeignHost, "configured", config.PortalURL)
	}

	verdict := out.Classify()
	m.log.Debug("login response", "summary", out.Summary(), "verdict", verdict.String())

	if verdict == login.VerdictRejected {
		m.recordAttempt(Attempt{At: start, Verdict: "rejected", Summary: out.Summary()})
		m.failDefinitive("the portal rejected the credentials (" + out.Summary() + ")")
		return
	}

	// The re-check is authoritative: "does the portal now consider us signed
	// in?" is the only question that matters, and being online wins over any
	// success pattern.
	ok, vres := m.verifyOnline(ctx)
	if ok {
		m.recordAttempt(Attempt{At: start, OK: true, Verdict: "signed in", Summary: out.Summary()})
		m.markOnline(vres)
		return
	}

	if verdict == login.VerdictAccepted {
		m.regexDisagree++
		if m.regexDisagree >= maxRegexDisagreements {
			m.recordAttempt(Attempt{At: start, Verdict: "unverified", Summary: out.Summary()})
			m.failDefinitive("the portal accepted the request but still reports no session — " +
				"the login URL is probably wrong")
			return
		}
		m.recordAttempt(Attempt{At: start, Verdict: "unverified", Summary: out.Summary()})
		m.failTransient("the portal accepted the request but still reports no session")
		return
	}

	m.recordAttempt(Attempt{At: start, Verdict: "unverified", Summary: out.Summary()})
	m.failTransient("the login was sent, but the portal still reports no session (" + vres.Detail + ")")
}

// verifyOnline re-asks the portal, allowing for authorisation that is applied a
// moment after the login answers. It checks immediately, then twice more.
func (m *Monitor) verifyOnline(ctx context.Context) (bool, probe.Session) {
	delays := []time.Duration{verifyDelay1, verifyDelay2}
	var last probe.Session

	for i := 0; i <= len(delays); i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false, last
			case <-m.opts.Clock.After(delays[i-1]):
			}
		}
		last = m.session(ctx)
		if last.Kind == probe.SessionOnline {
			return true, last
		}
	}
	return false, last
}

// markOnline records a portal-confirmed session and clears every failure counter.
func (m *Monitor) markOnline(sess probe.Session) {
	wasOnline := m.state() == StateOnline
	m.resetBackoff()

	m.update(func(s *Snapshot) {
		s.State = StateOnline
		s.Detail = "the portal reports an active session"
		if sess.UID != "" {
			s.Detail = "signed in as " + sess.UID
		}
		s.SessionUID = sess.UID
		s.SessionSeconds = sess.OnlineSeconds
		s.NextRetryAt = time.Time{}
		s.Consecutive = 0
		if !wasOnline || s.OnlineSince.IsZero() {
			s.OnlineSince = m.opts.Clock.Now()
		}
	})
}

// failTransient handles failures that may well fix themselves.
func (m *Monitor) failTransient(detail string) {
	m.consecutive++
	m.backoff = bumpBackoff(m.backoff)
	m.log.Warn("login not verified", "detail", detail, "retry_in", m.backoff)

	next := m.opts.Clock.Now().Add(m.backoff)
	m.update(func(s *Snapshot) {
		s.State = StateLoginFailed
		s.Detail = detail
		s.NextRetryAt = next
		s.Consecutive = m.consecutive
		s.OnlineSince = time.Time{}
	})
}

// failDefinitive handles failures that will not fix themselves, such as a wrong
// password. Retrying cannot help, so the delay is long and fixed, the user is
// told once, and after enough repeats the monitor gives up for a while.
func (m *Monitor) failDefinitive(detail string) {
	m.consecutive++
	m.definitiveStreak++
	m.log.Error("login rejected", "detail", detail, "streak", m.definitiveStreak)

	if m.definitiveStreak >= maxDefinitiveStreak {
		m.suspendedUntil = m.opts.Clock.Now().Add(suspendDuration)
		m.notify("Gatehouse has paused", detail+". Retrying in 30 minutes.")
		m.update(func(s *Snapshot) {
			s.State = StateSuspended
			s.Detail = detail + " — paused for 30 minutes"
			s.NextRetryAt = m.suspendedUntil
			s.Consecutive = m.consecutive
			s.OnlineSince = time.Time{}
		})
		return
	}

	m.backoff = definitiveBackoff
	m.notifyOnce("Gatehouse could not sign in", detail+". Check the settings.")
	m.update(func(s *Snapshot) {
		s.State = StateAuthRejected
		s.Detail = detail
		s.NextRetryAt = m.opts.Clock.Now().Add(m.backoff)
		s.Consecutive = m.consecutive
		s.OnlineSince = time.Time{}
	})
}

// nextWait is how long to sleep before the next cycle.
func (m *Monitor) nextWait() time.Duration {
	switch m.state() {
	case StateLoginFailed:
		if m.backoff > 0 {
			return m.backoff
		}
		return baseBackoff
	case StateAuthRejected:
		return definitiveBackoff
	case StateSuspended:
		if d := m.suspendedUntil.Sub(m.opts.Clock.Now()); d > 0 {
			if d > suspendDuration {
				return suspendDuration
			}
			return d
		}
		return time.Second
	default:
		// The idle poll. This is the only place the configurable interval
		// applies; the cases above bound how often credentials are sent and must
		// not be shortened by it.
		return m.currentConfig().SessionPollInterval()
	}
}

func (m *Monitor) resetBackoff() {
	m.backoff = 0
	m.consecutive = 0
	m.definitiveStreak = 0
	m.regexDisagree = 0
	m.notified = false
	m.suspendedUntil = time.Time{}
}

func (m *Monitor) notifyOnce(title, body string) {
	if m.notified {
		return
	}
	m.notified = true
	m.notify(title, body)
}

func (m *Monitor) notify(title, body string) {
	if m.opts.Notify == nil {
		return
	}
	m.opts.Notify(title, body)
}

func (m *Monitor) recordSession(sess probe.Session) {
	m.update(func(s *Snapshot) {
		s.SessionKind = sess.Kind.String()
		s.SessionNote = sess.Detail
		s.LastCheckAt = m.opts.Clock.Now()
	})
}

// recordAttempt prepends to the history. A fresh slice is allocated each time so
// a Snapshot already handed to the UI is never mutated underneath it.
func (m *Monitor) recordAttempt(a Attempt) {
	m.update(func(s *Snapshot) {
		a.Elapsed = m.opts.Clock.Now().Sub(a.At)
		s.LastAttempt = &a

		next := make([]Attempt, 0, len(s.History)+1)
		next = append(next, a)
		next = append(next, s.History...)
		if len(next) > historyLimit {
			next = next[:historyLimit]
		}
		s.History = next
	})
}

// update mutates the snapshot under lock and publishes a copy.
func (m *Monitor) update(f func(*Snapshot)) {
	m.mu.Lock()
	f(&m.snap)
	snap := m.snap
	m.mu.Unlock()

	if m.opts.OnChange != nil {
		m.opts.OnChange(snap)
	}
}

func (m *Monitor) currentConfig() *config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Monitor) state() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap.State
}

// bumpBackoff doubles the delay, saturating at maxBackoff.
func bumpBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return baseBackoff
	}
	if next := cur * 2; next < maxBackoff {
		return next
	}
	return maxBackoff
}

func (s State) String() string { return string(s) }

// TestResult reports the outcome of a one-off manual test.
type TestResult struct {
	OK            bool   `json:"ok"`
	Message       string `json:"message"`
	ProbeKind     string `json:"probe_kind"`
	ProbeDetail   string `json:"probe_detail"`
	HTTPStatus    int    `json:"http_status"`
	ResponseHead  string `json:"response_head"`
	PortalAddress string `json:"portal_address"`
}

// TestOnce performs a single, self-contained check-and-login using the supplied
// settings, without touching the running monitor. It backs the UI's "Test login"
// button, which is the main tool for iterating on captured portal parameters.
func TestOnce(ctx context.Context, client HTTPDoer, dial probe.DialFunc, cfg *config.Config) TestResult {
	if client == nil {
		client = login.NewClient()
	}
	if dial == nil {
		dial = defaultDial
	}

	sess := probe.SessionStatus(ctx, client)
	out := TestResult{ProbeKind: sess.Kind.String(), ProbeDetail: sess.Detail}

	switch sess.Kind {
	case probe.SessionOnline:
		out.OK = true
		out.Message = "The portal already reports an active session — nothing to do."
		if sess.UID != "" {
			out.Message = "The portal reports an active session for " + sess.UID + " — nothing to do."
		}
		return out

	case probe.SessionUnreachable:
		out.Message = "The portal did not answer, so this is not the campus network. " +
			"Gatehouse would do nothing here."
		return out

	case probe.SessionUnreadable:
		out.Message = "The portal answered, but its status reply could not be read, so Gatehouse " +
			"would do nothing rather than risk signing in on a bad signal: " + sess.Detail
		return out
	}

	if err := cfg.Validate(); err != nil {
		out.Message = "Cannot sign in: " + err.Error()
		return out
	}

	lout, err := login.Execute(ctx, client, cfg.AccountName(), cfg.Password)
	if err != nil {
		out.Message = "The login request did not complete: " + err.Error()
		return out
	}
	out.HTTPStatus = lout.StatusCode
	out.ResponseHead = headOf(lout.Body)

	// Settle briefly, then let the portal decide.
	select {
	case <-ctx.Done():
		out.Message = "Cancelled."
		return out
	case <-time.After(verifyDelay1):
	}

	if after := probe.SessionStatus(ctx, client); after.Kind == probe.SessionOnline {
		out.OK = true
		out.Message = "Signed in successfully."
		return out
	}

	switch lout.Classify() {
	case login.VerdictRejected:
		out.Message = "The portal rejected these credentials (" + lout.Summary() + ")."
	case login.VerdictAccepted:
		out.Message = "The portal accepted the request but still reports no session — " +
			"the login URL is probably wrong (" + lout.Summary() + ")."
	default:
		out.Message = "The login was sent, but the portal still reports no session (" + lout.Summary() + ")."
	}
	return out
}

func headOf(b []byte) string {
	const max = 400
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
