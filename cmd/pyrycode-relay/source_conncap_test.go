package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	proxyproto "github.com/pires/go-proxyproto"
)

func TestRun_SourceAllowanceValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  int
	}{
		{"zero", []string{"--max-connections-per-source", "0"}, 2},
		{"negative", []string{"--max-connections-per-source", "-1"}, 2},
		{"equal", []string{"--max-connections", "8"}, 2},
		{"greater", []string{"--max-connections", "7"}, 2},
		{"one global", []string{"--max-connections", "1", "--max-connections-per-source", "1"}, 2},
		{"explicit equal", []string{"--max-connections-per-source", "20"}, 2},
		{"explicit greater", []string{"--max-connections-per-source", "21"}, 2},
		{"positive below", []string{"--max-connections", "2", "--max-connections-per-source", "1"}, 0},
		{"default below", []string{"--max-connections", "9"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			args := append([]string{"--insecure-listen", capAddr(t), "--metrics-listen", ""}, tc.flags...)
			if got := run(args, ctx); got != tc.want {
				t.Fatalf("exit=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestRun_SourceAllowanceProxyHTTPS(t *testing.T) {
	r := startCappedRelay(t, true, true, 32)
	// Clone transports before use, choosing distinct declared sources while
	// every TCP connection still originates on the same loopback socket peer.
	clientFor := func(source string) *http.Client {
		tr := r.client.Transport.(*http.Transport).Clone()
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			h := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4, SourceAddr: &net.TCPAddr{IP: net.ParseIP(source), Port: 40000}, DestinationAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8443}}
			if _, err := h.WriteTo(c); err != nil {
				c.Close()
				return nil, err
			}
			return c, nil
		}
		t.Cleanup(tr.CloseIdleConnections)
		return &http.Client{Transport: tr, Timeout: 3 * time.Second}
	}
	a, b := clientFor("192.0.2.1"), clientFor("192.0.2.2")
	dial := func(client *http.Client, path, id string) (*websocket.Conn, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hdr := http.Header{"X-Pyrycode-Server": []string{id}, "X-Pyrycode-Version": []string{"0.0.0"}, "X-Pyrycode-Token": []string{"opaque"}, "User-Agent": []string{"relay-source-test"}}
		return websocket.Dial(ctx, "wss://"+r.public+path, &websocket.DialOptions{HTTPClient: client, Host: "relay.invalid", HTTPHeader: hdr})
	}
	bin, _, err := dial(a, "/v1/server", "source-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bin.CloseNow() })
	reads := readPump(bin)
	phones := make([]*websocket.Conn, 7)
	for i := range phones {
		phone, _, err := dial(a, "/v1/client", "source-a")
		if err != nil {
			t.Fatal(err)
		}
		phones[i] = phone
		t.Cleanup(func() { phone.CloseNow() })
		sendAndExpectAtBinary(t, phone, reads, `{"v":2,"t":"probe"}`)
	}
	for _, path := range []string{"/v1/server", "/v1/client"} {
		c, resp, err := dial(a, path, "source-a")
		if err == nil {
			c.CloseNow()
			t.Fatal("same source upgraded over allowance")
		}
		if resp == nil || resp.StatusCode != 503 {
			t.Fatalf("denial: %v %v", resp, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "\n" {
			t.Fatalf("body=%q", body)
		}
	}
	other, _, err := dial(b, "/v1/server", "source-b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.CloseNow() })
	_ = readPump(other)
	r.check(t, r.public, "/healthz", 200, "")
	resp, err := http.Get("http://" + r.metrics + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("metrics status=%d", resp.StatusCode)
	}
	rateSeries := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, `outcome="reject_rate_limit"`) || strings.Contains(line, `kind="ratelimit"`) {
			rateSeries++
			if !strings.HasSuffix(line, " 0") {
				t.Fatalf("503 counted as 429: %s", line)
			}
		}
	}
	if rateSeries != 3 {
		t.Fatalf("rate-limit series checked=%d, want 3", rateSeries)
	}
	phones[0].CloseNow()
	select {
	case notice := <-reads:
		if notice.err != nil || !strings.Contains(string(notice.data), `"close_code":1001`) {
			t.Fatalf("phone close notice: %s %v", notice.data, notice.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing phone close notice")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		fresh, resp, err := dial(a, "/v1/client", "source-a")
		if err == nil {
			t.Cleanup(func() { fresh.CloseNow() })
			sendAndExpectAtBinary(t, fresh, reads, `{"v":2,"t":"recovered"}`)
			break
		}
		if resp == nil || resp.StatusCode != 503 || time.Now().After(deadline) {
			t.Fatalf("recovery: %v %v", resp, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if defaultMaxConnectionsPerSource != 8 {
		t.Fatalf("source default=%s", strconv.Itoa(defaultMaxConnectionsPerSource))
	}
}
