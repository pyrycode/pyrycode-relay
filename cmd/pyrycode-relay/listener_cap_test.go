package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pyrycode/pyrycode-relay/internal/relay"
	"golang.org/x/net/netutil"
)

const challengeBody = "fixture-token.fixture-authorization"

type cappedRelay struct {
	public, http01, metrics string
	secure, proxy           bool
	client                  *http.Client
	cancel                  context.CancelFunc
	exit                    chan int
}

func capAddr(t *testing.T) string {
	t.Helper()
	a, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func startCappedRelay(t *testing.T, secure, proxy bool, cap int) *cappedRelay {
	t.Helper()
	r := &cappedRelay{public: capAddr(t), metrics: capAddr(t), secure: secure, proxy: proxy, exit: make(chan int, 1)}
	args := []string{"--metrics-listen", r.metrics}
	if cap > 0 {
		args = append(args, "--max-listener-connections", strconv.Itoa(cap))
	}
	tr := &http.Transport{DisableKeepAlives: true}
	if secure {
		r.http01 = capAddr(t)
		cache := t.TempDir() + "/certs"
		mgr, err := relay.NewAutocertManager("relay.invalid", cache)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay.invalid"}, DNSNames: []string{"relay.invalid"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		priv, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		bundle := append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		if err := mgr.Cache.Put(context.Background(), "relay.invalid", bundle); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Cache.Put(context.Background(), "fixture-token+http-01", []byte(challengeBody)); err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		tr.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "relay.invalid", MinVersion: tls.VersionTLS12}
		args = append(args, "--domain", "relay.invalid", "--cert-cache", cache, "--https-listen", r.public, "--http-listen", r.http01)
		if proxy {
			args = append(args, "--https-proxy-protocol")
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				if err := capProxyHeader(c); err != nil {
					c.Close()
					return nil, err
				}
				return c, nil
			}
		}
	} else {
		args = append(args, "--insecure-listen", r.public)
	}
	r.client = &http.Client{Transport: tr, Timeout: 7 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.exit <- run(args, ctx) }()
	t.Cleanup(func() {
		cancel()
		tr.CloseIdleConnections()
		select {
		case code := <-r.exit:
			if code != 0 {
				t.Errorf("run exit = %d", code)
			}
		case <-time.After(drainDeadline + time.Second):
			t.Error("run did not stop")
		}
	})
	if err := waitForDial(r.public, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	// A completed request proves application admission; TCP dial alone does not.
	r.check(t, r.public, "/healthz", http.StatusOK, "")
	return r
}

func capProxyHeader(c net.Conn) error {
	h := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4, SourceAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000}, DestinationAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8443}}
	_, err := h.WriteTo(c)
	return err
}

func (r *cappedRelay) check(t *testing.T, addr, path string, code int, body string) {
	t.Helper()
	scheme := "http"
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 7 * time.Second}
	if addr == r.public {
		client = r.client
		if r.secure {
			scheme = "https"
		}
	}
	req, err := http.NewRequest(http.MethodGet, scheme+"://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "relay.invalid"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != code || (body != "" && string(got) != body) || resp.Header.Get("Location") != "" {
		t.Fatalf("GET %s = %d %q, Location %q", path, resp.StatusCode, got, resp.Header.Get("Location"))
	}
}

func capDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (r *cappedRelay) admittedHTTP(t *testing.T, addr string) net.Conn {
	t.Helper()
	c := capDial(t, addr)
	if addr == r.public && r.secure {
		if r.proxy {
			if err := capProxyHeader(c); err != nil {
				t.Fatal(err)
			}
		}
		c = tls.Client(c, r.client.Transport.(*http.Transport).TLSClientConfig.Clone())
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(c, "GET /healthz HTTP/1.1\r\nHost: relay.invalid\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	c.SetDeadline(time.Time{})
	return c
}

// Send bytes that would immediately elicit a response if parsing occurred.
// A timed-out read proves the peer remains outside relay admission.
func capWaiting(t *testing.T, addr string, secure bool) net.Conn {
	t.Helper()
	c := capDial(t, addr)
	fmt.Fprint(c, "GET /healthz HTTP/1.1\r\nHost: relay.invalid\r\nConnection: close\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	var b [1]byte
	_, err := c.Read(b[:])
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("queued peer parsed before admission: %v, %q (TLS=%v)", err, b, secure)
	}
	c.SetReadDeadline(time.Time{})
	return c
}

func TestRun_ListenerCapNonPositive(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			// Otherwise-valid startup would drain with exit 0 if validation
			// were absent. A poisoned environment could falsely pass with 2.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if got := run([]string{"--insecure-listen", capAddr(t), "--metrics-listen", "", "--max-listener-connections", value}, ctx); got != 2 {
				t.Fatalf("exit = %d", got)
			}
		})
	}
}

func TestRun_ListenerCapDefault(t *testing.T) {
	r := startCappedRelay(t, false, false, 0) // omit the flag
	var admitted []net.Conn
	for range 32 {
		admitted = append(admitted, r.admittedHTTP(t, r.public))
	}
	queued := capWaiting(t, r.public, false)
	admitted[0].Close()
	queued.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(queued), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queued health status = %d", resp.StatusCode)
	}
}

func TestRun_ListenerCapPublicModes(t *testing.T) {
	for _, mode := range []struct {
		name          string
		secure, proxy bool
	}{{"insecure", false, false}, {"https", true, false}, {"proxy-https", true, true}} {
		t.Run(mode.name, func(t *testing.T) {
			r := startCappedRelay(t, mode.secure, mode.proxy, 1)
			first := r.admittedHTTP(t, r.public)
			queued := capWaiting(t, r.public, r.secure)
			r.check(t, r.metrics, "/metrics", http.StatusOK, "")
			first.Close()
			queued.SetReadDeadline(time.Now().Add(3 * time.Second))
			got, err := io.ReadAll(queued)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("slot not released: %v", err)
			}
			want := "HTTP/1.1 200"
			if r.secure {
				want = "HTTP/1.0 400"
			}
			if (!r.proxy && !strings.HasPrefix(string(got), want)) || (r.proxy && len(got) != 0) {
				t.Fatalf("unexpected queued response %q", got)
			}
			queued.Close()
			r.check(t, r.public, "/healthz", http.StatusOK, "")
			if r.secure {
				idle := r.admittedHTTP(t, r.http01)
				challenge := capDial(t, r.http01)
				fmt.Fprint(challenge, "GET /.well-known/acme-challenge/fixture-token HTTP/1.1\r\nHost: relay.invalid\r\nConnection: close\r\n\r\n")
				challenge.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
				var b [1]byte
				if _, err := challenge.Read(b[:]); err == nil {
					t.Fatal("HTTP-01 exceeded cap")
				} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
					t.Fatal(err)
				}
				// HTTP-01 has its own slot; HTTPS upgrades remain possible.
				ws := r.dialWS(t, "independent")
				ws.Close(websocket.StatusNormalClosure, "")
				idle.Close()
				challenge.SetReadDeadline(time.Now().Add(3 * time.Second))
				resp, err := http.ReadResponse(bufio.NewReader(challenge), nil)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || string(body) != challengeBody {
					t.Fatalf("challenge = %d %q", resp.StatusCode, body)
				}
				r.check(t, r.http01, "/", http.StatusNotFound, "404 page not found\n")
			}
		})
	}
}

func (r *cappedRelay) dialWS(t *testing.T, id string) *websocket.Conn {
	t.Helper()
	scheme := "ws"
	if r.secure {
		scheme = "wss"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, scheme+"://"+r.public+"/v1/server", &websocket.DialOptions{HTTPClient: r.client, Host: "relay.invalid", HTTPHeader: http.Header{"X-Pyrycode-Server": []string{id}, "X-Pyrycode-Version": []string{"0.0.0"}, "User-Agent": []string{"relay-cap-test"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func TestRun_ListenerCapReleasePaths(t *testing.T) {
	for _, tc := range []struct {
		name          string
		secure, proxy bool
		initial, fail string
		abrupt        bool
	}{
		{"abrupt", false, false, "GET /healthz HTTP/1.1\r\n", "", true},
		{"incomplete-http", false, false, "GET /healthz HTTP/1.1\r\n", "", false},
		{"failed-tls", true, false, "", "GET / HTTP/1.1\r\n\r\n", false},
		{"stalled-tls", true, false, "", "", false},
		{"rejected-proxy", true, true, "", "PROXY TCP4 127.0.0.1 127.0.0.1 1 2\r\n", false},
		{"stalled-proxy", true, true, "\r\n\r\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startCappedRelay(t, tc.secure, tc.proxy, 1)
			first := capDial(t, r.public)
			if tc.initial != "" {
				fmt.Fprint(first, tc.initial)
			}
			// Queue a valid fresh peer before failing the admitted one. Its
			// HTTP/TLS bytes must remain unparsed until the first slot frees.
			fresh := capDial(t, r.public)
			fresh.SetDeadline(time.Now().Add(7 * time.Second))
			served := make(chan error, 1)
			go func() {
				c := fresh
				if r.proxy {
					if err := capProxyHeader(c); err != nil {
						served <- err
						return
					}
				}
				if r.secure {
					c = tls.Client(c, r.client.Transport.(*http.Transport).TLSClientConfig.Clone())
				}
				_, err := io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: relay.invalid\r\nConnection: close\r\n\r\n")
				if err == nil {
					var resp *http.Response
					resp, err = http.ReadResponse(bufio.NewReader(c), nil)
					if err == nil {
						_, err = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode != http.StatusOK {
							err = fmt.Errorf("fresh health status = %d", resp.StatusCode)
						}
					}
				}
				served <- err
			}()
			select {
			case err := <-served:
				t.Fatalf("fresh peer served before cleanup: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			if tc.abrupt {
				first.(*net.TCPConn).SetLinger(0)
				first.Close()
			}
			if tc.fail != "" {
				fmt.Fprint(first, tc.fail)
			}
			select {
			case err := <-served:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("fresh connection was not admitted after cleanup")
			}
		})
	}
}

func TestRun_ListenerCapWebSocketAndShutdown(t *testing.T) {
	r := startCappedRelay(t, false, false, 1)
	ws := r.dialWS(t, "slot-reuse")
	queued := capWaiting(t, r.public, false)
	if err := ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	queued.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(queued), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	queued.Close()
	ws = r.dialWS(t, "slot-reuse")
	reads := readPump(ws)
	capWaiting(t, r.public, false)
	started := time.Now()
	r.cancel()
	select {
	case got := <-reads:
		if websocket.CloseStatus(got.err) != websocket.StatusGoingAway {
			t.Fatalf("close = %v", got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no 1001 drain")
	}
	select {
	case code := <-r.exit:
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		r.exit <- code
	case <-time.After(drainDeadline - time.Since(started)):
		t.Fatal("shutdown exceeded drain deadline")
	}
	if c, err := net.DialTimeout("tcp", r.public, 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("listener remains open")
	}
}

// Directly observe server-side Accept, so repeated Close cannot silently free
// a newer owner's slot. This exercises the pinned dependency's actual contract.
func TestListenerCap_CloseOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln = netutil.LimitListener(ln, 1)
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	accept := func() { c, _ := ln.Accept(); accepted <- c }
	go accept()
	capDial(t, ln.Addr().String())
	first := <-accepted
	go accept()
	capDial(t, ln.Addr().String())
	select {
	case <-accepted:
		t.Fatal("over cap")
	case <-time.After(100 * time.Millisecond):
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { first.Close() })
	}
	wg.Wait()
	second := <-accepted
	defer second.Close()
	go accept()
	capDial(t, ln.Addr().String())
	first.Close()
	select {
	case <-accepted:
		t.Fatal("old Close released new owner's slot")
	case <-time.After(100 * time.Millisecond):
	}
	ln.Close()
	select {
	case c := <-accepted:
		if c != nil {
			c.Close()
			t.Fatal("accepted after listener Close")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Accept did not wake")
	}
}

func TestListenerCap_ListenerFailureDrain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln = netutil.LimitListener(ln, 1)
	defer ln.Close()
	reg := relay.NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &http.Server{Handler: relay.ServerHandler(reg, logger, time.Second, 256*1024, nil), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	exit := make(chan int, 1)
	go func() {
		exit <- runServers(context.Background(), logger, reg, []*http.Server{srv}, func(s *http.Server) error { return s.Serve(ln) })
	}()
	ws := dialServer(t, ln.Addr().String(), "failure")
	defer ws.CloseNow()
	reads := readPump(ws)
	capWaiting(t, ln.Addr().String(), false)
	ln.Close() // unexpected listener failure while Accept is capacity-blocked
	select {
	case got := <-reads:
		if websocket.CloseStatus(got.err) != websocket.StatusGoingAway {
			t.Fatalf("close = %v", got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failure did not drain WS")
	}
	select {
	case code := <-exit:
		if code != 1 {
			t.Fatalf("exit = %d", code)
		}
	case <-time.After(drainDeadline):
		t.Fatal("failure did not return")
	}
}
