package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The shapes the portal actually sends. These come from a live session and from
// the portal's own login script, which branches on result.
const (
	replyOnline  = `dr1002({"result":1,"time":2231,"uid":"2026000000@cmcc","v4ip":"10.1.109.190","oltime":864000,"flow":33396033})`
	replyOffline = `dr1002({"result":0})`
)

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSessionOnline(t *testing.T) {
	srv := serve(t, http.StatusOK, replyOnline)

	got := SessionStatusAt(context.Background(), NewClient(), srv.URL)
	if got.Kind != SessionOnline {
		t.Fatalf("Kind = %v (%s), want Online", got.Kind, got.Detail)
	}
	if got.UID != "2026000000@cmcc" {
		t.Errorf("UID = %q, want the account the portal reports", got.UID)
	}
	if got.OnlineSeconds != 864000 {
		t.Errorf("OnlineSeconds = %d, want 864000", got.OnlineSeconds)
	}
}

func TestSessionOffline(t *testing.T) {
	srv := serve(t, http.StatusOK, replyOffline)

	got := SessionStatusAt(context.Background(), NewClient(), srv.URL)
	if got.Kind != SessionOffline {
		t.Fatalf("Kind = %v (%s), want Offline", got.Kind, got.Detail)
	}
}

// The safety-critical cases. Anything that is not a clean, parsed result must
// never be reported as Offline: doing so would let a broken portal trigger a
// login on every poll cycle.
func TestUnreadableRepliesAreNeverOffline(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP 400 as the portal sends without a callback", http.StatusBadRequest,
			"<html><body>Error code: 400 URL no found</body></html>"},
		{"HTML login page from an interception", http.StatusOK,
			`<html><form><input type="password"></form></html>`},
		{"empty body", http.StatusOK, ""},
		{"payload without a result field", http.StatusOK, `dr1002({"uid":"x"})`},
		{"unexpected status code", http.StatusOK, `dr1002({"result":7})`},
		{"not JSONP at all", http.StatusOK, `success`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, tc.status, tc.body)

			got := SessionStatusAt(context.Background(), NewClient(), srv.URL)
			if got.Kind == SessionOffline {
				t.Fatalf("an unreadable reply was reported as Offline: %+v", got)
			}
			if got.Kind != SessionUnreadable {
				t.Errorf("Kind = %v, want Unreadable", got.Kind)
			}
		})
	}
}

func TestSessionUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	got := SessionStatusAt(context.Background(), NewClient(), url)
	if got.Kind != SessionUnreachable {
		t.Errorf("Kind = %v (%s), want Unreachable", got.Kind, got.Detail)
	}
}

// A captive portal announcing itself with a redirect must not be read as a
// status answer.
func TestSessionDoesNotFollowRedirects(t *testing.T) {
	var foreignHits int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits++
		io.WriteString(w, replyOnline)
	}))
	defer foreign.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusFound)
	}))
	defer srv.Close()

	got := SessionStatusAt(context.Background(), NewClient(), srv.URL)
	if foreignHits != 0 {
		t.Errorf("the status check was followed to another host %d time(s), want 0", foreignHits)
	}
	if got.Kind == SessionOnline {
		t.Error("a redirect was read as an online session")
	}
}

func TestParseSessionToleratesWhitespaceAndKeyOrder(t *testing.T) {
	body := []byte(`dr1002({ "oltime" : 42 , "uid" : "a@b" , "result" : 1 })`)

	got := parseSession(body)
	if got.Kind != SessionOnline {
		t.Fatalf("Kind = %v, want Online", got.Kind)
	}
	if got.UID != "a@b" || got.OnlineSeconds != 42 {
		t.Errorf("parsed %q / %d, want a@b / 42", got.UID, got.OnlineSeconds)
	}
}

func TestParseSessionOfflineWithoutExtraFields(t *testing.T) {
	got := parseSession([]byte(`dr1002({"result":0})`))
	if got.Kind != SessionOffline {
		t.Fatalf("Kind = %v, want Offline", got.Kind)
	}
	if got.UID != "" {
		t.Errorf("UID = %q, want empty for an offline session", got.UID)
	}
}

func TestDescribeNetErrNamesATimeout(t *testing.T) {
	err := &net.DNSError{Name: "10.255.0.19", Err: "no such host"}
	if got := describeNetErr(err); !strings.Contains(got, "DNS lookup") {
		t.Errorf("describeNetErr = %q, want it to name the DNS failure", got)
	}
}
