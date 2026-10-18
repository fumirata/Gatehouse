package login

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sagan/gatehouse/config"
)

// drcomOK is the reply a Dr.COM portal sends when the credentials are accepted.
const drcomOK = `dr1003({"result":1,"login_result":1})`

// testEndpoint builds the login URL a fake portal would serve, so tests can
// exercise the real request path without touching the fixed campus address.
func testEndpoint(serverURL string) (endpoint, referer string) {
	root := strings.TrimSuffix(serverURL, "/")
	return root + config.LoginPath, root + "/"
}

// The query must match the request a real browser sends to this portal.
func TestBuildQueryMatchesTheCapturedRequest(t *testing.T) {
	got := BuildQuery("20260000000@cmcc", "test-password")

	values, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("BuildQuery produced an unparseable query: %v", err)
	}

	want := map[string]string{
		"callback": "dr1003",
		"DDDDD":    "20260000000@cmcc",
		"upass":    "test-password",
		"0MKKey":   "123456",
		"R1":       "0",
		"R3":       "0",
		"R6":       "0",
		"para":     "00",
		"v6ip":     "",
	}
	for k, v := range want {
		if got := values.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if values.Get("v") == "" {
		t.Error("v= cache-buster is missing")
	}
}

// The cache-buster must differ between attempts, or a proxy could answer with a
// stale response.
func TestBuildQueryVariesTheCacheBuster(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		values, err := url.ParseQuery(BuildQuery("u", "p"))
		if err != nil {
			t.Fatal(err)
		}
		seen[values.Get("v")] = true
	}
	if len(seen) < 15 {
		t.Errorf("only %d distinct v= values across 20 attempts; the cache-buster is not varying", len(seen))
	}
}

// A password containing & or = would silently split the query if it were not
// escaped, producing a failure indistinguishable from wrong credentials.
func TestBuildQueryEscapesCredentials(t *testing.T) {
	const password = "a&b=c%20d+e@cmcc"

	values, err := url.ParseQuery(BuildQuery("20260000000@cmcc", password))
	if err != nil {
		t.Fatalf("the query did not survive parsing: %v", err)
	}
	if got := values.Get("upass"); got != password {
		t.Errorf("password round-tripped as %q, want %q", got, password)
	}
	if got := values.Get("DDDDD"); got != "20260000000@cmcc" {
		t.Errorf("username round-tripped as %q", got)
	}
}

func TestExecuteSendsTheRequestToThePortal(t *testing.T) {
	var gotPath, gotQuery, gotReferer, gotUA string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotReferer = r.Header.Get("Referer")
		gotUA = r.Header.Get("User-Agent")
		io.WriteString(w, drcomOK)
	}))
	defer srv.Close()

	ep, ref := testEndpoint(srv.URL)
	out, err := Executef(context.Background(), NewClient(), ep, ref, "20260000000@cmcc", "test-password")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotPath != config.LoginPath {
		t.Errorf("path = %q, want %q", gotPath, config.LoginPath)
	}
	if !strings.Contains(gotQuery, "callback=dr1003") {
		t.Errorf("query = %q, want the dr1003 callback", gotQuery)
	}
	if gotReferer == "" {
		t.Error("Referer was not sent; the portal's own page sets it")
	}
	if gotUA == "" {
		t.Error("User-Agent was not sent")
	}
	if out.Classify() != VerdictAccepted {
		t.Errorf("Classify = %v, want accepted for result:1", out.Classify())
	}
}

// The portal address is a constant, so this is the one place that pins it.
func TestPortalAddressIsFixed(t *testing.T) {
	if got := config.LoginURL(); got != "http://10.255.0.19/drcom/login" {
		t.Errorf("LoginURL() = %q, want the fixed campus endpoint", got)
	}
	if got := config.PortalRoot(); got != "http://10.255.0.19/" {
		t.Errorf("PortalRoot() = %q, want the fixed campus root", got)
	}
}

// doerFunc lets a test fabricate a response without touching the network.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// The load-bearing safety property: Go re-sends the request on 307/308 by
// default, which would hand the password to whatever host the redirect names.
func TestExecuteDoesNotFollowRedirects(t *testing.T) {
	var foreignHits int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits++
		io.WriteString(w, "gotcha")
	}))
	defer foreign.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	ep, ref := testEndpoint(srv.URL)
	out, err := Executef(context.Background(), NewClient(), ep, ref, "u", "p")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if foreignHits != 0 {
		t.Errorf("the credential-bearing request was followed to another host %d time(s), want 0", foreignHits)
	}
	if out.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the redirect to be reported rather than followed", out.StatusCode)
	}
}

// The tripwire that fires if a caller ever supplies a redirect-following client.
func TestExecuteReportsAResponseFromAnotherHost(t *testing.T) {
	answeredBy, err := http.NewRequest(http.MethodGet, "http://somewhere-else.example/x", nil)
	if err != nil {
		t.Fatal(err)
	}

	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    answeredBy,
		}, nil
	})

	out, err := Executef(context.Background(), doer, "http://10.255.0.19"+config.LoginPath, "http://10.255.0.19/", "u", "p")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.ForeignHost != "somewhere-else.example" {
		t.Errorf("ForeignHost = %q, want the host that actually answered", out.ForeignHost)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		out  Outcome
		want Verdict
	}{
		{
			name: "dr1003 result 1 is a success",
			out:  Outcome{StatusCode: 200, Body: []byte(`dr1003({"result":1,"login_result":1})`)},
			want: VerdictAccepted,
		},
		{
			name: "login_result 2 means already online",
			out:  Outcome{StatusCode: 200, Body: []byte(`dr1003({"result":1,"login_result":2})`)},
			want: VerdictAccepted,
		},
		{
			name: "result 0 is a rejection",
			out:  Outcome{StatusCode: 200, Body: []byte(`dr1003({"result":0,"msg":"密码错误"})`)},
			want: VerdictRejected,
		},
		{
			name: "a Chinese failure message is a rejection",
			out:  Outcome{StatusCode: 200, Body: []byte(`dr1003({"msg":"用户不存在"})`)},
			want: VerdictRejected,
		},
		{
			name: "401 is a rejection",
			out:  Outcome{StatusCode: http.StatusUnauthorized},
			want: VerdictRejected,
		},
		{
			name: "an unrecognised reply is inconclusive",
			out:  Outcome{StatusCode: 200, Body: []byte(`<html>hello</html>`)},
			want: VerdictUnknown,
		},
		{
			name: "a failure marker beats a success marker",
			out:  Outcome{StatusCode: 200, Body: []byte(`{"result":0,"login_result":1}`)},
			want: VerdictRejected,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.out.Classify(); got != tc.want {
				t.Errorf("Classify = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReplyExcerptsThePortalAnswer(t *testing.T) {
	long := strings.Repeat("x", 500)
	out := Outcome{Body: []byte(long)}

	got := out.Reply()
	if len(got) > 210 {
		t.Errorf("Reply returned %d characters, want a short excerpt", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated excerpt should be marked as such")
	}
}
