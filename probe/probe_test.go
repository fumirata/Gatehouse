package probe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagan/gatehouse/config"
)

func checkConfig(url string) config.InternetProbe {
	return config.InternetProbe{
		URL:          url,
		ExpectStatus: http.StatusOK,
		ExpectBody:   config.DefaultInternetExpectBody,
	}
}

func TestInternetReached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(config.DefaultInternetExpectBody))
	}))
	defer srv.Close()

	got := Internet(context.Background(), NewClient(), checkConfig(srv.URL))
	if got.Kind != Online {
		t.Errorf("Kind = %v (%s), want Online", got.Kind, got.Detail)
	}
}

func TestInternetInterceptedByRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/portal", http.StatusFound)
	}))
	defer srv.Close()

	got := Internet(context.Background(), NewClient(), checkConfig(srv.URL))
	if got.Kind != Intercepted {
		t.Fatalf("Kind = %v (%s), want Intercepted", got.Kind, got.Detail)
	}
	if got.RedirectHost != "10.0.0.1" {
		t.Errorf("RedirectHost = %q, want %q", got.RedirectHost, "10.0.0.1")
	}
}

func TestInternetInterceptedByLoginPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body><form><input type="password" name="p"></form></body></html>`))
	}))
	defer srv.Close()

	got := Internet(context.Background(), NewClient(), checkConfig(srv.URL))
	if got.Kind != Intercepted {
		t.Errorf("Kind = %v (%s), want Intercepted", got.Kind, got.Detail)
	}
}

func TestInternetUnreachable(t *testing.T) {
	// A port that is closed, so the dial fails the way a dead network does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	got := Internet(context.Background(), NewClient(), checkConfig(url))
	if got.Kind != NoNetwork {
		t.Errorf("Kind = %v (%s), want NoNetwork", got.Kind, got.Detail)
	}
}

// A 404 with no body means the probe settings are wrong, not that the network is
// down — reporting a phantom outage here would be actively misleading.
func TestInternetUnexpectedStatusIsMisconfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got := Internet(context.Background(), NewClient(), checkConfig(srv.URL))
	if got.Kind != Misconfigured {
		t.Errorf("Kind = %v (%s), want Misconfigured", got.Kind, got.Detail)
	}
}

func TestInternetRejectsAnInvalidURL(t *testing.T) {
	got := Internet(context.Background(), NewClient(), checkConfig("://nonsense"))
	if got.Kind != Misconfigured {
		t.Errorf("Kind = %v, want Misconfigured", got.Kind)
	}
}

func TestPortalReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	dial := (&net.Dialer{}).DialContext

	ok, addr, err := PortalReachable(context.Background(), dial, srv.URL+"/login")
	if !ok {
		t.Fatalf("PortalReachable = false (%v) for %s", err, addr)
	}

	// The same dialer against a closed port must report unreachable.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := closed.URL
	closed.Close()

	ok, _, _ = PortalReachable(context.Background(), dial, deadURL+"/login")
	if ok {
		t.Error("PortalReachable = true for a closed port")
	}
}

func TestPortalReachablePropagatesDialFailure(t *testing.T) {
	sentinel := errors.New("network is unreachable")
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, sentinel }

	ok, addr, err := PortalReachable(context.Background(), dial, "https://portal.example.edu/login")
	if ok {
		t.Fatal("PortalReachable = true despite the dial failing")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the dial error", err)
	}
	if addr != "portal.example.edu:443" {
		t.Errorf("addr = %q, want the https default port to be applied", addr)
	}
}

func TestHostPortDefaultsByScheme(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"http://10.0.0.1/login", "10.0.0.1:80"},
		{"https://auth.example.edu/eportal/x", "auth.example.edu:443"},
		{"http://10.0.0.1:8080/login", "10.0.0.1:8080"},
		{"https://auth.example.edu:9443/", "auth.example.edu:9443"},
	}
	for _, tc := range tests {
		got, err := hostPort(tc.in)
		if err != nil {
			t.Errorf("hostPort(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("hostPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHostPortRejectsAHostlessURL(t *testing.T) {
	if _, err := hostPort("http:///login"); err == nil {
		t.Error("hostPort accepted a URL with no host")
	}
}
