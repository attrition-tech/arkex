package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetRetriesIPv6ConnectionFailureOverIPv4(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var calls atomic.Int32
	dialer := &net.Dialer{}
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		if network != "tcp4" {
			return &writeFailConn{closed: make(chan struct{})}, nil
		}
		return dialer.DialContext(ctx, network, address)
	}
	c := &Client{HTTP: &http.Client{Transport: transport, Timeout: time.Second}}

	body, err := c.get(context.Background(), srv.URL, 100, nil)
	if err != nil || string(body) != "ok" {
		t.Fatalf("get = %q, %v", body, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dial calls = %d, want 2", got)
	}
}

func TestSignedUpdateWithIPv6Recovery(t *testing.T) {
	for _, tampered := range []bool{false, true} {
		name := "valid release"
		if tampered {
			name = "tampered archive"
		}
		t.Run(name, func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			r := newRelease(t, priv, "0.2.0", "NEW BINARY")
			if tampered {
				r.files["/v0.2.0/arkex_0.2.0_linux_amd64.tar.gz"] = tarGz(t, "arkex", []byte("EVIL"))
			}
			srv := httptest.NewTLSServer(r)
			defer srv.Close()
			exe := filepath.Join(t.TempDir(), "arkex")
			if err := os.WriteFile(exe, []byte("OLD BINARY"), 0755); err != nil {
				t.Fatal(err)
			}
			c := newTestClient(t, srv, pub, exe)
			c.HTTP = srv.Client()
			var retries atomic.Int32
			dialer := &net.Dialer{}
			c.HTTP.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if network != "tcp4" {
					return &writeFailConn{closed: make(chan struct{})}, nil
				}
				retries.Add(1)
				return dialer.DialContext(ctx, network, address)
			}
			manifest, newer, err := c.Check(t.Context())
			if err != nil || !newer || manifest.Version != "0.2.0" {
				t.Fatalf("check: %+v, newer=%v, %v", manifest, newer, err)
			}
			_, err = c.Apply(t.Context(), manifest)
			if (err != nil) != tampered {
				t.Fatalf("apply: tampered=%v, error=%v", tampered, err)
			}
			want := "NEW BINARY"
			if tampered {
				want = "OLD BINARY"
			}
			if got, err := os.ReadFile(exe); err != nil || string(got) != want {
				t.Fatalf("installed binary = %q, %v; want %q", got, err, want)
			}
			if got := retries.Load(); got != 4 {
				t.Fatalf("IPv4 retries = %d, want manifest, sums, signature and archive", got)
			}
		})
	}
}

// writeFailConn reproduces a connection that succeeds and then fails while
// net/http writes the request, as observed on the affected macOS host.
type writeFailConn struct {
	closed chan struct{}
	once   sync.Once
}

func (c *writeFailConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}

func (c *writeFailConn) Write([]byte) (int, error) {
	return 0, &net.OpError{Op: "write", Net: "tcp6", Addr: c.RemoteAddr(), Err: errors.New("socket is not connected")}
}

func (c *writeFailConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (*writeFailConn) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.ParseIP("::1")} }

func (*writeFailConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}
}

func (*writeFailConn) SetDeadline(time.Time) error      { return nil }
func (*writeFailConn) SetReadDeadline(time.Time) error  { return nil }
func (*writeFailConn) SetWriteDeadline(time.Time) error { return nil }

func TestGetIPv6RecoveryIsLimitedToOneRetry(t *testing.T) {
	var calls atomic.Int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, &net.OpError{Op: "write", Net: "tcp6", Addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}, Err: errors.New("socket is not connected")}
	}
	c := &Client{HTTP: &http.Client{Transport: transport}}

	if _, err := c.get(context.Background(), "https://example.test/file", 100, nil); err == nil {
		t.Fatal("expected connection error")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dial calls = %d, want 2", got)
	}
}

func TestGetDoesNotRetryCanceledRequest(t *testing.T) {
	var calls atomic.Int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, &net.OpError{Op: "write", Net: "tcp6", Addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}, Err: context.Canceled}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{HTTP: &http.Client{Transport: transport}}

	_, err := c.get(ctx, "https://example.test/file", 100, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("get error = %v, want context canceled", err)
	}
	if got := calls.Load(); got > 1 {
		t.Fatalf("dial calls = %d, want at most 1", got)
	}
}

func TestGetDoesNotRetryTLSCertificateError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("untrusted"))
	}))
	defer srv.Close()

	var calls atomic.Int32
	dialer := &net.Dialer{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		return dialer.DialContext(ctx, network, address)
	}
	c := &Client{HTTP: &http.Client{Transport: transport}}

	_, err := c.get(context.Background(), srv.URL, 100, nil)
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) {
		t.Fatalf("get error = %v, want unknown authority", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("dial calls = %d, want 1", got)
	}
}

func TestGetDoesNotRetryHTTPStatus(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := (&Client{}).get(context.Background(), srv.URL, 100, nil); err == nil {
		t.Fatal("expected HTTP status error")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestIPv6RecoveryErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		cause         error
		want          bool
	}{
		{"IPv6 socket", "2001:db8::1", errors.New("socket is not connected"), true},
		{"IPv4 socket", "192.0.2.1", errors.New("socket is not connected"), false},
		{"mapped IPv4", "::ffff:192.0.2.1", errors.New("reset"), false},
		{"canceled", "2001:db8::1", context.Canceled, false},
		{"deadline", "2001:db8::1", context.DeadlineExceeded, false},
		{"certificate", "2001:db8::1", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &net.OpError{Op: "write", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP(tc.address), Port: 443}, Err: tc.cause}
			if got := isIPv6NetworkError(err); got != tc.want {
				t.Fatalf("retry = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIPv6RetrySharesOriginalDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	var deadlines []time.Time
	// Redirect callbacks expose the request context for both attempts. Dial
	// contexts do not: net/http detaches them to support connection pooling.
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("request has no deadline")
		}
		deadlines = append(deadlines, deadline)
		if len(deadlines) == 1 {
			return &net.OpError{Op: "dial", Net: "tcp6", Addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}, Err: errors.New("unreachable")}
		}
		return nil
	}}
	body, err := (&Client{HTTP: client}).get(t.Context(), srv.URL+"/start", 100, nil)
	if err != nil || string(body) != "ok" {
		t.Fatalf("retry did not complete: %q, %v", body, err)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("retry received a fresh timeout budget: %v", deadlines)
	}
}

type failedUpdateTransport struct{ calls int }

func (f *failedUpdateTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	return nil, &net.OpError{Op: "dial", Net: "tcp6", Addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::1")}, Err: errors.New("unreachable")}
}

func TestIPv6RetryDoesNotReplaceCustomTransports(t *testing.T) {
	transport := &failedUpdateTransport{}
	_, err := (&Client{HTTP: &http.Client{Transport: transport}}).get(t.Context(), "https://example.test/file", 100, nil)
	if err == nil || transport.calls != 1 {
		t.Fatalf("custom RoundTripper was retried/replaced: calls=%d, error=%v", transport.calls, err)
	}
	for _, transport := range []*http.Transport{
		{Dial: func(string, string) (net.Conn, error) { return nil, errors.New("legacy dial") }},
		{DialTLS: func(string, string) (net.Conn, error) { return nil, errors.New("custom TLS") }},
		{DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("custom TLS context") }},
	} {
		if _, ok := ipv4RetryClient(&http.Client{Transport: transport}); ok {
			t.Fatal("custom dial behavior would be replaced")
		}
	}
}
