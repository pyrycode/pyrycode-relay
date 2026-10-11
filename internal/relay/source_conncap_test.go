package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func sourceEntries(c *SourceConnCap) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.counts)
}

func TestNewSourceConnCap(t *testing.T) {
	for _, max := range []int{-1, 0, 1, 8} {
		_, err := NewSourceConnCap(max, discardLogger(), false)
		if max <= 0 && !errors.Is(err, ErrInvalidSourceConnCap) {
			t.Fatalf("max=%d: %v", max, err)
		}
		if max > 0 && err != nil {
			t.Fatal(err)
		}
	}
}

func TestSourceConnCap_DenialAndCleanup(t *testing.T) {
	logger, logs := captureLogger()
	c, _ := NewSourceConnCap(1, logger, true)
	global, _ := NewConnCap(3, logger)
	entered, cleanup, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := c.Wrap(global.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { close(cleanup); <-release }()
		close(entered)
	})))
	req := func() *http.Request {
		r := httptest.NewRequest("GET", "/v1/client", nil)
		r.Header.Set("X-Forwarded-For", "host\n\"injection")
		r.Header.Set("X-Pyrycode-Token", "secret-token")
		r.Header.Set("X-Pyrycode-Server", "secret-routing")
		return r
	}
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), req()); close(done) }()
	<-entered
	<-cleanup
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req())
	if rr.Code != 503 || rr.Body.String() != "\n" || global.inUse() != 1 || sourceEntries(c) != 1 {
		t.Fatalf("denial: status=%d body=%q global=%d entries=%d", rr.Code, rr.Body.String(), global.inUse(), sourceEntries(c))
	}
	out := logs.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "source_conn_cap_reached") || !strings.Contains(out, strconv.Quote(strconv.Quote("host\n\"injection"))) {
		t.Fatalf("deny log: %q", out)
	}
	if strings.Contains(out, "secret-") {
		t.Fatalf("leaked header: %q", out)
	}
	close(release)
	<-done
	if sourceEntries(c) != 0 || global.inUse() != 0 {
		t.Fatal("cleanup leaked reservation")
	}
	for i := 0; i < 2; i++ {
		c.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), req())
		if sourceEntries(c) != 0 {
			t.Fatal("reused source retained entry")
		}
	}
	// A full global cap must return its temporary source reservation.
	global.slots <- struct{}{}
	global.slots <- struct{}{}
	global.slots <- struct{}{}
	c.Wrap(global.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("global deny called handler") }))).ServeHTTP(httptest.NewRecorder(), req())
	if sourceEntries(c) != 0 {
		t.Fatal("global refusal leaked source")
	}
	// Defers also release when net/http would recover a handler panic.
	func() {
		defer func() { _ = recover() }()
		c.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test") })).ServeHTTP(httptest.NewRecorder(), req())
	}()
	if sourceEntries(c) != 0 {
		t.Fatal("panic leaked source")
	}
}

func TestSourceConnCap_ConcurrentSharedAdmission(t *testing.T) {
	c, _ := NewSourceConnCap(8, discardLogger(), false)
	global, _ := NewConnCap(20, discardLogger())
	entered, refused, release := make(chan struct{}, 40), make(chan struct{}, 40), make(chan struct{})
	defer close(release)
	mux := http.NewServeMux()
	for _, path := range []string{"/v1/server", "/v1/client"} {
		mux.Handle(path, c.Wrap(global.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { entered <- struct{}{}; <-release }))))
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/v1/server"
			if i%2 == 1 {
				path = "/v1/client"
			}
			r := httptest.NewRequest("GET", path, nil)
			r.RemoteAddr = "203.0.113.1:" + strconv.Itoa(10000+i)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, r)
			if rr.Code == 503 {
				refused <- struct{}{}
			}
		}(i)
	}
	for i := 0; i < 40; i++ {
		select {
		case <-entered:
		case <-refused:
		case <-time.After(3 * time.Second):
			t.Fatal("admission blocked")
		}
	}
	if global.inUse() != 8 {
		t.Fatalf("global occupancy=%d, want 8", global.inUse())
	}
	other := httptest.NewRequest("GET", "/v1/client", nil)
	other.RemoteAddr = "203.0.113.2:10000"
	otherDone := make(chan struct{})
	go func() { mux.ServeHTTP(httptest.NewRecorder(), other); close(otherDone) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("different source not admitted")
	}
	// Join after releasing, then check deletion (including denied attempts).

	t.Cleanup(func() {
		wg.Wait()
		<-otherDone
		if sourceEntries(c) != 0 {
			t.Error("entries retained")
		}
	})
}

func TestSourceConnCap_ClientIP(t *testing.T) {
	for _, tc := range []struct {
		name, remote, xff, otherRemote, otherXFF string
		trust, same                              bool
	}{
		{"ports", "192.0.2.1:1", "", "192.0.2.1:2", "", false, true},
		{"ignore XFF", "192.0.2.1:1", "a", "192.0.2.1:2", "b", false, true},
		{"leftmost trimmed", "192.0.2.1:1", " a , b", "192.0.2.2:2", "a,c", true, true},
		{"different XFF", "192.0.2.1:1", "a", "192.0.2.1:2", "b", true, false},
		{"empty leftmost fallback", "192.0.2.1:1", ",b", "192.0.2.1:2", "", true, true},
		{"raw IPv6 preserved", "[2001:DB8::1]:1", "", "[2001:db8::1]:2", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := NewSourceConnCap(1, discardLogger(), tc.trust)
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-For", tc.xff)
			inner := c.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			c.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				other := httptest.NewRequest("GET", "/", nil)
				other.RemoteAddr = tc.otherRemote
				other.Header.Set("X-Forwarded-For", tc.otherXFF)
				rr := httptest.NewRecorder()
				inner.ServeHTTP(rr, other)
				want := 200
				if tc.same {
					want = 503
				}
				if rr.Code != want {
					t.Fatalf("status=%d want=%d", rr.Code, want)
				}
			})).ServeHTTP(httptest.NewRecorder(), r)
			if sourceEntries(c) != 0 {
				t.Fatal("retained entry")
			}
		})
	}
}

func TestSourceConnCap_RateLimitOutside(t *testing.T) {
	c, _ := NewSourceConnCap(1, discardLogger(), false)
	global, _ := NewConnCap(2, discardLogger())
	limiter := NewIPRateLimiter(time.Hour, 1, time.Hour)
	defer limiter.Close()
	limiter.Allow("192.0.2.1")
	h := NewRateLimitMiddleware(limiter, discardLogger(), false)(c.Wrap(global.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("denial reached endpoint") }))))
	for _, remote := range []string{"192.0.2.1:1", "invalid"} {
		r := httptest.NewRequest("GET", "/v1/client", nil)
		r.RemoteAddr = remote
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != 429 || sourceEntries(c) != 0 || global.inUse() != 0 {
			t.Fatal("rate denial reserved capacity")
		}
	}
}

func TestSourceConnCap_FailureAndTeardown(t *testing.T) {
	for _, mode := range []string{"headers", "handshake", "abrupt", "heartbeat", "4409", "4429", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			reg := NewRegistry()
			c, _ := NewSourceConnCap(3, discardLogger(), false)
			mux := http.NewServeMux()
			mux.Handle("/v1/server", c.Wrap(ServerHandler(reg, discardLogger(), time.Millisecond, 256*1024, nil)))
			mux.Handle("/v1/client", c.Wrap(ClientHandler(reg, discardLogger(), 256*1024, 1, nil)))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			base := "ws" + strings.TrimPrefix(srv.URL, "http")
			if mode == "headers" || mode == "handshake" {
				r := httptest.NewRequest("GET", "/v1/server", nil)
				if mode == "handshake" {
					r.Header = validHeaders("s1")
				}
				rr := httptest.NewRecorder()
				mux.ServeHTTP(rr, r)
				if rr.Code < 400 {
					t.Fatalf("failure accepted: %d", rr.Code)
				}
			} else {
				bin := mustDial(t, base+"/v1/server", validHeaders("s1"))
				awaitBinary(t, reg, "s1")
				switch mode {
				case "4409":
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					go func() {
						for {
							if _, _, err := bin.Read(ctx); err != nil {
								return
							}
						}
					}()
					rejected := mustDial(t, base+"/v1/server", validHeaders("s1"))
					if readClose(t, rejected) != 4409 {
						t.Fatal("want conflict")
					}
				case "4429":
					phone := mustDial(t, base+"/v1/client", validClientHeaders("s1"))
					waitForPhones(t, reg, "s1", 1, time.Second)
					rejected := mustDial(t, base+"/v1/client", validClientHeaders("s1"))
					if readClose(t, rejected) != 4429 {
						t.Fatal("want phone cap")
					}
					phone.CloseNow()
				case "heartbeat":
					conn, _ := reg.BinaryFor("s1")
					w := conn.(*WSConn)
					go runHeartbeat(context.Background(), w, time.Millisecond, 5*time.Millisecond)
					// Wait for timeout-driven close to begin before reading, so
					// the peer cannot answer the heartbeat in time.
					select {
					case <-w.closeCtx.Done():
					case <-time.After(time.Second):
						t.Fatal("heartbeat did not close")
					}
					if readClose(t, bin) != 1011 {
						t.Fatal("want heartbeat timeout close")
					}
				case "shutdown":
					go func() {
						for {
							if _, _, err := bin.Read(context.Background()); err != nil {
								return
							}
						}
					}()
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := Shutdown(ctx, discardLogger(), reg); err != nil {
						t.Fatal(err)
					}
				}
				bin.CloseNow()
			}
			if !pollUntil(time.Now().Add(3*time.Second), func() bool { return sourceEntries(c) == 0 }) {
				t.Fatal("exit retained source")
			}
			// Reuse the same source after every exit, not merely inspect the map.
			fresh := mustDial(t, base+"/v1/server", validHeaders("fresh"))
			awaitBinary(t, reg, "fresh")
			fresh.CloseNow()
			if !pollUntil(time.Now().Add(3*time.Second), func() bool { return sourceEntries(c) == 0 }) {
				t.Fatal("reconnect retained source")
			}
		})
	}
}
