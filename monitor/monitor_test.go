package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagan/gatehouse/config"
	"github.com/sagan/gatehouse/login"
	"github.com/sagan/gatehouse/probe"
)

// fakePortal stands in for both the internet and the campus portal, so every
// branch of the state machine can be exercised without a campus network.
type fakePortal struct {
	srv *httptest.Server

	mu          sync.Mutex
	offline     bool     // the portal reports no session
	unreadable  bool     // the portal answers with something we cannot parse
	logins      []string // query strings received at the Dr.COM login endpoint
	reject      bool     // the portal answers "wrong password"
	rejectBody  string
	stayOffline bool // accepts the login but still reports no session
}

// drcomSuccess and drcomReject are the JSONP replies a real Dr.COM portal sends.
const (
	drcomSuccess      = `dr1003({"result":1,"login_result":1})`
	drcomReject       = `dr1003({"result":0,"msg":"密码错误"})`
	drcomInconclusive = `dr1003({"result":1})`
)

func newFakePortal(t *testing.T) *fakePortal {
	t.Helper()

	f := &fakePortal{offline: true, rejectBody: drcomReject}

	mux := http.NewServeMux()
	// The portal's own status endpoint: the single signal the monitor acts on.
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		if f.unreadable {
			// A portal that answers, but not in a way we understand.
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "<html><body>Error code: 400 URL no found</body></html>")
			return
		}
		if f.offline {
			io.WriteString(w, `dr1002({"result":0})`)
			return
		}
		io.WriteString(w, `dr1002({"result":1,"uid":"alice@cmcc","oltime":3600})`)
	})
	// The Dr.COM login endpoint: a GET whose query carries the credentials.
	mux.HandleFunc(config.LoginPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.logins = append(f.logins, r.URL.RawQuery)
		if f.reject {
			io.WriteString(w, f.rejectBody)
			return
		}
		if f.stayOffline {
			// Accepts the request but the portal still reports no session — the
			// case where only the re-check can decide.
			io.WriteString(w, drcomInconclusive)
			return
		}
		f.offline = false
		io.WriteString(w, drcomSuccess)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePortal) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.logins)
}

func (f *fakePortal) lastLogin() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logins) == 0 {
		return ""
	}
	return f.logins[len(f.logins)-1]
}

func (f *fakePortal) setOffline(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offline = v
}

func (f *fakePortal) setUnreadable(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreadable = v
}

func (f *fakePortal) setReject(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reject = v
}

// ------------------------------------------------------------------ harness

type harness struct {
	t     *testing.T
	f     *fakePortal
	m     *Monitor
	clock *FakeClock
	cfg   *config.Config
	notes []string
}

func dialAlwaysOK(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func dialNever(ctx context.Context, network, addr string) (net.Conn, error) {
	return nil, errors.New("connect: connection refused")
}

func newHarness(t *testing.T, dial probe.DialFunc, tweak func(*config.Config)) *harness {
	t.Helper()

	f := newFakePortal(t)
	clock := NewFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))

	cfg := &config.Config{
		Username:                   "alice",
		Password:                   "correct horse",
		SessionPollIntervalSeconds: 5,
	}
	if tweak != nil {
		tweak(cfg)
	}

	h := &harness{t: t, f: f, clock: clock, cfg: cfg}

	root := strings.TrimSuffix(f.srv.URL, "/") + "/"
	h.m = New(Options{
		Config: cfg,
		Client: login.NewClient(),
		Dial:   dial,
		Clock:  clock,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notify: func(title, body string) { h.notes = append(h.notes, title+": "+body) },
		// Point at the fake portal instead of the fixed campus address.
		Endpoint: &Endpoint{
			Session: root + "status",
			Login:   root + strings.TrimPrefix(config.LoginPath, "/"),
		},
	})
	return h
}

// step runs one decision cycle, advancing the fake clock only when the monitor
// actually parks on a timer, so the post-login verification probes complete
// instead of taking real seconds.
func (h *harness) step() {
	h.t.Helper()
	h.stepAdvancingBy(30 * time.Second)
}

// stepAdvancingBy runs one cycle, releasing any timer the monitor waits on by
// exactly d. Using a fixed step keeps time deterministic, which matters because
// the minimum-interval guard compares wall-clock values: an oversized jump can
// silently carry the clock past it.
//
// A step of zero is used to prove the interval guard holds: it lets the cycle
// run without moving time forward, so any timer it parks on simply times out.
func (h *harness) stepAdvancingBy(d time.Duration) {
	h.t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.m.step(context.Background())
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case <-done:
			return
		default:
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("monitor step did not finish within 20s (step %v)", d)
		}

		// Advancing by zero cannot release a waiter, so only do it when there is
		// time to move.
		if d > 0 && h.clock.WaiterCount() > 0 {
			h.clock.Advance(d)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// stepAfter advances the clock first, standing in for the wait between cycles.
// Tests whose code path returns without touching the clock (a definitive
// rejection) need this, or the minimum-interval floor blocks the next cycle.
func (h *harness) stepAfter(d time.Duration) {
	h.clock.Advance(d)
	h.step()
}

func (h *harness) state() State { return h.m.Snapshot().State }

// ----------------------------------------------------------- the two promises

// The whole point of the trigger rule: while the internet works, nothing is sent.
func TestNothingIsSentWhenOnline(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(false)

	for i := 0; i < 5; i++ {
		h.step()
	}

	if got := h.state(); got != StateOnline {
		t.Errorf("state = %q, want %q", got, StateOnline)
	}
	if n := h.f.loginCount(); n != 0 {
		t.Errorf("sent %d login requests while already online, want 0", n)
	}
}

// The security property: the portal does not answer, so this is not the campus
// network and the credentials must stay put.
func TestNothingIsSentWhenThePortalIsNotReachable(t *testing.T) {
	h := newHarness(t, dialNever, nil)
	h.m.opts.Endpoint.Session = "http://127.0.0.1:1/status"

	for i := 0; i < 3; i++ {
		h.step()
	}

	if got := h.state(); got != StateNotOnCampus {
		t.Errorf("state = %q, want %q", got, StateNotOnCampus)
	}
	if n := h.f.loginCount(); n != 0 {
		t.Fatalf("sent %d login requests to a network that is not the campus one, want 0", n)
	}
}

func TestSignsInWhenThePortalIsPresent(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(true)

	h.step()

	if got := h.state(); got != StateOnline {
		t.Errorf("state = %q, want %q (detail: %s)", got, StateOnline, h.m.Snapshot().Detail)
	}
	if n := h.f.loginCount(); n != 1 {
		t.Fatalf("sent %d login requests, want exactly 1", n)
	}

	// The request must carry Dr.COM's own field names and fixed parameters.
	values := parseForm(t, h.f.lastLogin())
	want := map[string]string{
		"callback": "dr1003",
		"DDDDD":    "alice",
		"upass":    "correct horse",
		"0MKKey":   "123456",
		"para":     "00",
	}
	for k, v := range want {
		if got := values.Get(k); got != v {
			t.Errorf("%s = %q, want %q (query was %q)", k, got, v, h.f.lastLogin())
		}
	}
	if values.Get("v") == "" {
		t.Error("the v= cache-buster is missing from the request")
	}
}

// A password containing & or = would silently split the query if it were not
// escaped, producing a failure that looks exactly like wrong credentials.
func TestPasswordWithMetacharactersSurvivesTheRoundTrip(t *testing.T) {
	const password = "a&b=c%20d+e"

	h := newHarness(t, dialAlwaysOK, func(c *config.Config) {
		c.Password = password
	})
	h.f.setOffline(true)
	h.step()

	if n := h.f.loginCount(); n != 1 {
		t.Fatalf("sent %d login requests, want 1", n)
	}

	values := parseForm(t, h.f.lastLogin())
	if got := values.Get("upass"); got != password {
		t.Errorf("portal decoded password %q, want %q (raw query %q)", got, password, h.f.lastLogin())
	}
	if got := values.Get("DDDDD"); got != "alice" {
		t.Errorf("portal decoded username %q, want %q", got, "alice")
	}
}

// ------------------------------------------------------------ the portal says

// The portal's verdict is authoritative, so a reported "no session" is acted on
// immediately — no confirmation cycle, which is the whole point of asking the
// portal instead of inferring from a failed internet check.
func TestPortalReportingNoSessionLogsInImmediately(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(true)

	h.step()

	if n := h.f.loginCount(); n != 1 {
		t.Errorf("sent %d login requests on the first cycle, want 1", n)
	}
	if got := h.state(); got != StateOnline {
		t.Errorf("state = %q, want %q", got, StateOnline)
	}
}

// A portal that answers with something we cannot read must never be taken as
// "you are offline". A broken portal would otherwise cause a login every cycle.
func TestUnreadablePortalStatusNeverLogsIn(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setUnreadable(true)

	for i := 0; i < 5; i++ {
		h.step()
	}

	if n := h.f.loginCount(); n != 0 {
		t.Errorf("sent %d login requests on an unreadable status, want 0", n)
	}
	if got := h.state(); got != StateUnreadable {
		t.Errorf("state = %q, want %q", got, StateUnreadable)
	}
}

func TestPortalOnlineSendsNothing(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(false)

	for i := 0; i < 5; i++ {
		h.step()
	}

	if n := h.f.loginCount(); n != 0 {
		t.Errorf("sent %d login requests while the portal reports a session, want 0", n)
	}
	if got := h.state(); got != StateOnline {
		t.Errorf("state = %q, want %q", got, StateOnline)
	}
}

// ------------------------------------------------------------------- backoff

func TestTransientFailuresBackOffExponentially(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(true)
	h.f.stayOffline = true // the portal never actually opens up

	h.step()
	waits := []time.Duration{h.m.nextWait()}

	for i := 1; i < 8; i++ {
		h.step()
		waits = append(waits, h.m.nextWait())
	}

	// The first failure schedules the base delay, then it doubles to the cap.
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second,
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %v, want %v (full sequence: %v)", i, waits[i], want[i], waits)
		}
	}
}

func TestNoTwoRequestsInsideTheMinimumInterval(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(true)
	h.f.stayOffline = true

	h.step()
	if n := h.f.loginCount(); n != 1 {
		t.Fatalf("first attempt cycle sent %d requests, want 1", n)
	}

	// The guard parks the cycle on a timer rather than returning, so the clock has
	// to be driven precisely. Rewind it to one second after the first attempt,
	// then run cycles and confirm nothing is sent until the interval has passed.
	h.clock.Set(h.m.lastPostAt.Add(time.Second))

	for i := 0; i < 3; i++ {
		h.step()
		if n := h.f.loginCount(); n != 1 {
			t.Fatalf("sent %d requests %ds after the last one, want the 10s floor to hold", n, i+1)
		}
	}

	// And once the floor has genuinely elapsed, a new attempt is allowed —
	// otherwise the guard would be an indefinite block rather than a rate limit.
	h.clock.Set(h.m.lastPostAt.Add(minPostInterval + time.Second))
	h.step()
	if n := h.f.loginCount(); n != 2 {
		t.Errorf("sent %d requests after the interval elapsed, want 2", n)
	}
}

// -------------------------------------------------- definitive rejections

func TestRejectedCredentialsBackOffHardAndTellTheUser(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(true)
	h.f.setReject(true)

	h.step()

	if got := h.state(); got != StateAuthRejected {
		t.Fatalf("state = %q, want %q (detail: %s)", got, StateAuthRejected, h.m.Snapshot().Detail)
	}
	if got := h.m.nextWait(); got != definitiveBackoff {
		t.Errorf("next wait = %v, want %v", got, definitiveBackoff)
	}
	if len(h.notes) != 1 {
		t.Errorf("user was notified %d times, want exactly 1", len(h.notes))
	}
}

// A portal that accepts the request but never opens up is a transient failure:
// the credentials may well be fine and the portal is simply not ready.
func TestLoginThatDoesNotOpenTheNetworkIsTransient(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(true)
	h.f.stayOffline = true

	h.step()

	if got := h.state(); got != StateLoginFailed {
		t.Errorf("state = %q, want %q", got, StateLoginFailed)
	}
}

func TestRepeatedRejectionsPauseTheApp(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.f.setOffline(true)
	h.f.setReject(true)

	// The first cycle confirms the outage; the next one is the first rejection.
	h.step()
	for i := 1; i < maxDefinitiveStreak; i++ {
		h.stepAfter(definitiveBackoff)
	}

	if got := h.state(); got != StateSuspended {
		t.Fatalf("state = %q, want %q after %d rejections", got, StateSuspended, maxDefinitiveStreak)
	}

	// Suspension lifts on an explicit request.
	h.m.ApplyConfig(h.cfg)
	h.m.resetBackoff()
	if h.m.nextWait() > suspendDuration {
		t.Errorf("next wait = %v, want no more than %v", h.m.nextWait(), suspendDuration)
	}
}

// -------------------------------------------------------------- configuration

func TestUnconfiguredAppDoesNotProbeOrSignIn(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, func(c *config.Config) {
		c.Username = ""
		c.Password = ""
	})

	h.step()

	if got := h.state(); got != StateNeedsConfig {
		t.Errorf("state = %q, want %q", got, StateNeedsConfig)
	}
	if n := h.f.loginCount(); n != 0 {
		t.Errorf("sent %d requests with no credentials configured, want 0", n)
	}
}

// The portal not answering at all means this is not the campus network, so
// nothing is sent — the same protection the old internet probe provided, now
// resting on the portal being unreachable.
func TestUnreachablePortalSendsNothing(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, nil)
	h.m.opts.Endpoint.Session = "http://127.0.0.1:1/status"

	h.step()

	if got := h.state(); got != StateNotOnCampus {
		t.Errorf("state = %q, want %q", got, StateNotOnCampus)
	}
	if n := h.f.loginCount(); n != 0 {
		t.Errorf("sent %d login requests with no portal, want 0", n)
	}
}

// ------------------------------------------------------------- misc safety

func TestCredentialsAreNotResentAcrossARedirect(t *testing.T) {
	var foreignHits int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits++
		io.WriteString(w, "gotcha")
	}))
	defer foreign.Close()

	// The login endpoint redirects the credential-bearing request elsewhere.
	mux := http.NewServeMux()
	// The portal reports no session, so a login is attempted...
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `dr1002({"result":0})`)
	})
	// ...and the login endpoint redirects the credential-bearing request away.
	mux.HandleFunc(config.LoginPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/evil", http.StatusTemporaryRedirect)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h := newHarness(t, dialAlwaysOK, nil)
	root := strings.TrimSuffix(srv.URL, "/") + "/"
	h.m.opts.Endpoint = &Endpoint{
		Session: root + "status",
		Login:   root + strings.TrimPrefix(config.LoginPath, "/"),
	}

	h.step()

	if foreignHits != 0 {
		t.Errorf("the credential-bearing request was followed to another host %d times, want 0", foreignHits)
	}
}

func TestBackoffSaturatesAtTheMaximum(t *testing.T) {
	got := baseBackoff
	for i := 0; i < 20; i++ {
		got = bumpBackoff(got)
	}
	if got != maxBackoff {
		t.Errorf("bumpBackoff saturated at %v, want %v", got, maxBackoff)
	}
}

func parseForm(t *testing.T, body string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("could not parse the form body %q: %v", body, err)
	}
	return values
}

// ---------------------------------------------------------------- poll timing

// The poll interval controls how quickly an expired session is noticed. It must
// not shorten anything that bounds how often credentials are sent — that is what
// keeps a mis-set value from hammering the portal.
func TestPollIntervalDoesNotShortenLoginBackoff(t *testing.T) {
	h := newHarness(t, dialAlwaysOK, func(c *config.Config) {
		c.SessionPollIntervalSeconds = 1 // the most aggressive poll allowed
	})
	h.f.setOffline(true)
	h.f.stayOffline = true // the portal accepts the login but reports no session

	h.step()

	if got := h.m.nextWait(); got == time.Second {
		t.Fatalf("next wait = %v: the poll interval leaked into login backoff", got)
	}
	if got := h.m.nextWait(); got < baseBackoff {
		t.Errorf("next wait = %v, want at least the %v base backoff", got, baseBackoff)
	}

	// And the hard floor between credential-bearing requests still holds.
	if h.m.lastPostAt.IsZero() {
		t.Error("no attempt was recorded, so the floor was not exercised")
	}
	if minPostInterval <= time.Second {
		t.Errorf("minPostInterval = %v, want it independent of the poll interval", minPostInterval)
	}
}

// A shorter poll interval shortens the idle wait, which is the point.
func TestShorterPollIntervalShortensTheIdleWait(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{
		{1, time.Second},
		{5, 5 * time.Second},
		{30, 30 * time.Second},
	} {
		h := newHarness(t, dialAlwaysOK, func(c *config.Config) {
			c.SessionPollIntervalSeconds = tc.seconds
		})
		h.f.setOffline(false) // online: the idle path

		h.step()

		if got := h.m.nextWait(); got != tc.want {
			t.Errorf("with a %ds interval, next wait = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}
