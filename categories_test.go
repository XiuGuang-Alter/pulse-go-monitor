package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func categoryOf(t *testing.T, value any) string {
	t.Helper()
	field := reflect.ValueOf(value).FieldByName("ErrorCategory")
	if !field.IsValid() {
		t.Fatal("missing structured ErrorCategory")
	}
	return field.String()
}

// Classifying status alone would mislabel an accepted 404 or a failed 200 assertion.
func TestCheckErrorCategoriesRespectRules(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		body           string
		rules          CheckRules
		kind, category string
	}{
		{"success", 200, "ok", CheckRules{}, "ok", ""},
		{"allowed404", 404, "ok", CheckRules{ExpectedStatuses: []int{404}}, "ok", ""},
		{"client", 404, "ok", CheckRules{}, "http_error", "http_4xx"},
		{"server", 503, "ok", CheckRules{}, "http_error", "http_5xx"},
		{"other", 300, "ok", CheckRules{}, "http_error", "http_other"},
		{"assertion", 200, "no", CheckRules{BodyContains: "expected"}, "assertion_failed", "assertion"},
		{"tooLarge", 200, strings.Repeat("a", (1<<20)+1), CheckRules{BodyContains: "a"}, "body_too_large", "body_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer server.Close()
			result := CheckWithRules(context.Background(), server.URL+"?token=SYNTHETIC_SECRET", time.Second, tc.rules)
			if got := categoryOf(t, result); got != tc.category || result.Kind != tc.kind {
				t.Fatalf("kind=%s category=%s want %s/%s", result.Kind, got, tc.kind, tc.category)
			}
		})
	}
}

func TestCheckNetworkCategoriesAndSafeDetail(t *testing.T) {
	t.Run("connection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		address := server.URL
		server.Close()
		result := Check(context.Background(), address+"?token=SYNTHETIC_SECRET", time.Second)
		if got := categoryOf(t, result); got != "connection" {
			t.Fatalf("category=%s", got)
		}
		if strings.Contains(result.Detail, "SYNTHETIC_SECRET") || strings.Contains(result.Detail, address) {
			t.Fatalf("unsafe detail: %s", result.Detail)
		}
	})
	t.Run("tls", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()
		result := Check(context.Background(), server.URL+"?token=SYNTHETIC_SECRET", time.Second)
		if got := categoryOf(t, result); got != "tls" {
			t.Fatalf("category=%s", got)
		}
		if strings.Contains(result.Detail, "SYNTHETIC_SECRET") || strings.Contains(result.Detail, server.URL) {
			t.Fatalf("unsafe detail: %s", result.Detail)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		result := Check(context.Background(), server.URL, 30*time.Millisecond)
		if got := categoryOf(t, result); got != "timeout" || result.Kind != "timeout" {
			t.Fatalf("result=%+v", result)
		}
	})
	t.Run("bodyRead", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "20")
			io.WriteString(w, "short")
		}))
		defer server.Close()
		result := CheckWithRules(context.Background(), server.URL, time.Second, CheckRules{BodyContains: "short"})
		if got := categoryOf(t, result); got != "body_read" || result.Kind != "request_error" {
			t.Fatalf("result=%+v", result)
		}
	})
	t.Run("input", func(t *testing.T) {
		if got := categoryOf(t, Check(context.Background(), "bad", time.Second)); got != "input" {
			t.Fatal(got)
		}
	})
}

// The local UDP responder returns NXDOMAIN for every query. No public DNS or
// internet connectivity is involved; removing errors.As(DNSError) breaks this.
func TestCheckControlledDNSCategory(t *testing.T) {
	dns, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dns.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			n, peer, err := dns.ReadFrom(buffer)
			if err != nil {
				return
			}
			if n < 17 {
				continue
			}
			end := 12
			for end < n && buffer[end] != 0 {
				end += int(buffer[end]) + 1
			}
			end += 5
			if end > n {
				continue
			}
			response := append([]byte(nil), buffer[:end]...)
			response[2] = 0x81
			response[3] = 0x83
			for i := 6; i < 12; i++ {
				response[i] = 0
			}
			dns.WriteTo(response, peer)
		}
	}()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", dns.LocalAddr().String())
	}}
	transport := &http.Transport{DialContext: (&net.Dialer{Resolver: resolver}).DialContext}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections(); dns.Close(); <-done }()
	result := Check(context.Background(), "http://synthetic-dns.invalid/?token=SYNTHETIC_SECRET", time.Second)
	if got := categoryOf(t, result); got != "dns" || result.Kind != "request_error" {
		t.Fatalf("result=%+v", result)
	}
	if strings.Contains(result.Detail, "SYNTHETIC_SECRET") || strings.Contains(result.Detail, "synthetic-dns.invalid") {
		t.Fatalf("unsafe detail=%s", result.Detail)
	}
}

func TestCheckBodyTimeoutAndUnknownCategories(t *testing.T) {
	t.Run("bodyTimeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		result := CheckWithRules(context.Background(), server.URL, 30*time.Millisecond, CheckRules{BodyContains: "expected"})
		if got := categoryOf(t, result); got != "timeout" || result.Kind != "timeout" || result.StatusCode != 200 {
			t.Fatalf("result=%+v", result)
		}
	})
	t.Run("redirectLimit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/?token=SYNTHETIC_SECRET", http.StatusFound)
		}))
		defer server.Close()
		result := Check(context.Background(), server.URL, time.Second)
		if got := categoryOf(t, result); got != "unknown" || result.Kind != "request_error" {
			t.Fatalf("result=%+v", result)
		}
		if strings.Contains(result.Detail, "SYNTHETIC_SECRET") {
			t.Fatal("unsafe detail")
		}
	})
}

// TLS protocol alerts are not always exported typed errors in crypto/tls;
// the handshake stage must establish the category rather than error strings.
func TestCheckTLSProtocolFailureCategory(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	transport := &http.Transport{TLSClientConfig: &tls.Config{MaxVersion: tls.VersionTLS12}}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
	result := Check(context.Background(), server.URL, time.Second)
	if result.ErrorCategory != "tls" || result.Kind != "request_error" {
		t.Fatalf("result=%+v", result)
	}
}

func TestCheckTransportTimeoutPreservesLegacyKind(t *testing.T) {
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
	}}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
	result := Check(context.Background(), "http://synthetic-timeout.invalid", time.Second)
	if result.ErrorCategory != "timeout" || result.Kind != "request_error" {
		t.Fatalf("result=%+v", result)
	}
}
