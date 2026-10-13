package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Explicitly synthetic, predictable credentials: never use for provisioning.
var admissionOld = strings.Repeat("1", 64)
var admissionNew = strings.Repeat("2", 64)
var admissionOther = strings.Repeat("3", 64)

func admissionEnv(mode, raw string) map[string]string {
	return map[string]string{envDaemonAdmissionMode: mode, envDaemonAdmissionCredentials: raw}
}
func admissionLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}
func admissionFixture(t *testing.T, mode string) *DaemonAdmission {
	t.Helper()
	a, err := NewDaemonAdmissionFromEnv(admissionLookup(admissionEnv(mode,
		`{"s1":["`+admissionOld+`","`+admissionNew+`"],"s2":["`+admissionOther+`"],"empty":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func admissionHeaders(id, token string) http.Header {
	h := validHeaders(id)
	if token != "missing" {
		h.Set("X-Pyrycode-Admission-Token", token)
	}
	return h
}

func TestDaemonAdmission_Config(t *testing.T) {
	valid := `{"s1":["` + admissionOld + `"]}`
	for _, mode := range []string{"off", "observe", "enforce"} {
		for _, raw := range []string{"", "null", "[]", "1", `"secret-parser-excerpt"`, `{"s1":null}`, `{"s1":{}}`,
			`{"s1":[7]}`, `{"s1":[null]}`, `{"s1":["secret-parser-excerpt"]}`, `{"s1":["` + strings.ToUpper(strings.Repeat("a", 64)) + `"]}`,
			`{"s1":[],"s1":[]}`, `{"s1":[],"\u00731":[]}`, `{"s1":["` + admissionOld + `"],"s2":["` + admissionOld + `"]}`, valid + " {}", valid + " garbage"} {
			t.Run(mode+"/"+raw[:min(12, len(raw))], func(t *testing.T) {
				env := admissionEnv(mode, raw)
				for _, err := range []error{CheckEnvConfig(admissionLookup(env)), func() error { _, err := NewDaemonAdmissionFromEnv(admissionLookup(env)); return err }()} {
					var cfg *ErrInvalidConfig
					if !errors.Is(err, ErrInvalidConfigSentinel) || !errors.As(err, &cfg) || cfg.Key != envDaemonAdmissionCredentials {
						t.Fatalf("wrong config error: %v", err)
					}
					if strings.Contains(err.Error(), admissionOld) || strings.Contains(err.Error(), "secret-parser-excerpt") || strings.Contains(err.Error(), "s1") {
						t.Fatalf("leaked input: %v", err)
					}
				}
			})
		}
	}
	for _, env := range []map[string]string{nil, {envDaemonAdmissionMode: "off"}, {envDaemonAdmissionMode: "observe"},
		admissionEnv("enforce", "{}"), admissionEnv("enforce", `{"s1":[]}`), admissionEnv("enforce", valid), admissionEnv("off", `{"s1":["`+admissionOld+`","`+admissionOld+`"]}`)} {
		if err := CheckEnvConfig(admissionLookup(env)); err != nil {
			t.Fatal(err)
		}
		if _, err := NewDaemonAdmissionFromEnv(admissionLookup(env)); err != nil {
			t.Fatal(err)
		}
	}
	for _, env := range []map[string]string{{envDaemonAdmissionMode: "enforce"}, {envDaemonAdmissionMode: ""}, {envDaemonAdmissionMode: "Observe"}, {envDaemonAdmissionMode: " off"}} {
		if err := CheckEnvConfig(admissionLookup(env)); !errors.Is(err, ErrInvalidConfigSentinel) {
			t.Fatalf("accepted bad mode/presence: %v", err)
		}
		if _, err := NewDaemonAdmissionFromEnv(admissionLookup(env)); !errors.Is(err, ErrInvalidConfigSentinel) {
			t.Fatalf("constructor accepted bad mode/presence: %v", err)
		}
	}
}

func admissionCases() []struct {
	name    string
	h       http.Header
	outcome string
} {
	rows := []struct {
		name    string
		h       http.Header
		outcome string
	}{
		{"valid", admissionHeaders("s1", admissionOld), "valid"},
		{"overlap", admissionHeaders("s1", admissionNew), "valid"},
		{"missing", admissionHeaders("s1", "missing"), "missing"},
		{"empty", admissionHeaders("s1", ""), "malformed"},
		{"short", admissionHeaders("s1", "abc"), "malformed"},
		{"upper", admissionHeaders("s1", strings.Repeat("A", 64)), "malformed"},
		{"nonhex", admissionHeaders("s1", strings.Repeat("g", 64)), "malformed"},
		{"space", admissionHeaders("s1", admissionOld[:32]+" "+admissionOld[32:]), "malformed"},
		{"coalesced token", admissionHeaders("s1", admissionOld+","+admissionNew), "malformed"},
		{"unknown", admissionHeaders("s1", strings.Repeat("4", 64)), "unknown"},
		{"mismatch", admissionHeaders("s2", admissionOld), "mismatch"},
		{"absent identity", admissionHeaders("absent", admissionOld), "unknown"},
		{"empty array", admissionHeaders("empty", admissionOld), "unknown"},
		{"empty identity", admissionHeaders("", admissionOld), "malformed"},
		{"coalesced identity", admissionHeaders("s1,s2", admissionOld), "malformed"},
	}
	missingID := admissionHeaders("s1", admissionOld)
	missingID.Del("X-Pyrycode-Server")
	rows = append(rows, struct {
		name    string
		h       http.Header
		outcome string
	}{"missing identity", missingID, "malformed"})
	for _, field := range []string{"X-Pyrycode-Server", "X-Pyrycode-Admission-Token"} {
		for _, same := range []bool{true, false} {
			h := admissionHeaders("s1", admissionOld)
			value := h.Get(field)
			if !same {
				value = "different"
			}
			h.Add(field, value)
			rows = append(rows, struct {
				name    string
				h       http.Header
				outcome string
			}{field + value[:min(3, len(value))], h, "malformed"})
		}
	}
	return rows
}

func TestDaemonAdmission_DecisionsAndTelemetry(t *testing.T) {
	for _, mode := range []string{"off", "observe", "enforce"} {
		metrics := NewMetricsRegistry()
		a := admissionFixture(t, mode)
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
		handler := a.Wrap(next, metrics)
		counts := map[string]int{}
		for _, row := range admissionCases() {
			t.Run(mode+"/"+row.name, func(t *testing.T) {
				req := httptest.NewRequest("GET", "/", nil)
				req.Header = row.h
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)
				if mode == "enforce" && row.outcome != "valid" {
					assertAdmissionRejection(t, rr.Result())
				} else if rr.Code != http.StatusNoContent {
					t.Fatalf("status = %d", rr.Code)
				}
			})
			if mode != "off" {
				counts[row.outcome]++
			}
		}
		families, err := metrics.Gather()
		if err != nil {
			t.Fatal(err)
		}
		if len(families) != 1 {
			t.Fatalf("admission metric families = %d", len(families))
		}
		for _, f := range families {
			if f.GetName() == "pyrycode_relay_daemon_admission_total" {
				if len(f.Metric) != 5 {
					t.Fatalf("unbounded metric cells: %d", len(f.Metric))
				}
				for _, m := range f.Metric {
					if len(m.Label) != 1 || m.Label[0].GetName() != "outcome" {
						t.Fatal("unexpected labels")
					}
					label := m.Label[0].GetValue()
					switch label {
					case "valid", "missing", "malformed", "unknown", "mismatch":
					default:
						t.Fatalf("unknown outcome %q", label)
					}
					if m.Counter.GetValue() != float64(counts[label]) {
						t.Fatalf("%s count: %v", label, m.Counter.GetValue())
					}
				}
			}
		}
		rr := httptest.NewRecorder()
		NewMetricsHandler(metrics).ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
		if strings.Contains(rr.Body.String(), admissionOld) || strings.Contains(rr.Body.String(), "s1") {
			t.Fatal("credential/identity metric leak")
		}
	}
}
func assertAdmissionRejection(t *testing.T, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 401 || resp.Header.Get("Cache-Control") != "no-store" || string(body) != "daemon admission rejected\n" {
		t.Fatalf("nonuniform rejection: status=%d headers=%v body=%q err=%v", resp.StatusCode, resp.Header, body, err)
	}
}

type admissionIncumbent struct {
	fakeConn
	probes atomic.Int32
}

func (c *admissionIncumbent) Ping(context.Context) error { c.probes.Add(1); return nil }

func TestDaemonAdmission_RejectedStateUntouched(t *testing.T) {
	for _, state := range []string{"empty", "occupied", "grace"} {
		t.Run(state, func(t *testing.T) {
			reg := NewRegistry()
			incumbent := &admissionIncumbent{fakeConn: fakeConn{id: "incumbent"}}
			phone := &fakeConn{id: "phone"}
			if state != "empty" {
				if err := reg.ClaimServer("s1", incumbent); err != nil {
					t.Fatal(err)
				}
				if err := reg.RegisterPhone("s1", phone); err != nil {
					t.Fatal(err)
				}
			}
			var timer *graceEntry
			if state == "grace" {
				reg.ScheduleReleaseServer("s1", time.Hour)
				reg.mu.RLock()
				timer = reg.timers["s1"]
				reg.mu.RUnlock()
				defer reg.ReleaseServer("s1")
			}
			logger, logs := captureLogger()
			sender := newFakePushSender()
			waker := NewPushWaker(sender, logger)
			defer waker.Close()
			reg.SetPushWaker(waker)
			srv := httptest.NewServer(admissionFixture(t, "enforce").Wrap(ServerHandler(reg, logger, time.Second, 256*1024, nil), NewMetricsRegistry()))
			defer srv.Close()
			for _, row := range admissionCases() {
				if row.outcome == "valid" {
					continue
				}
				req, err := http.NewRequest("GET", srv.URL, strings.NewReader(`{"push_wake":{"platform":"fcm","token":"synthetic"}}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header = row.h
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				assertAdmissionRejection(t, resp)
			}
			if incumbent.probes.Load() != 0 || incumbent.isClosed() || phone.isClosed() || len(sender.sentTokens()) != 0 || logs.String() != "" {
				t.Fatal("rejected request caused side effects")
			}
			reg.mu.RLock()
			defer reg.mu.RUnlock()
			if reg.timers["s1"] != timer || (state == "empty" && len(reg.binaries) != 0) || (state != "empty" && (reg.binaries["s1"] != incumbent || len(reg.phones["s1"]) != 1)) {
				t.Fatal("registry changed")
			}
		})
	}
}

func admissionServer(t *testing.T, a *DaemonAdmission, reg *Registry) string {
	t.Helper()
	srv := httptest.NewServer(a.Wrap(ServerHandler(reg, discardLogger(), time.Hour, 256*1024, nil), NewMetricsRegistry()))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}
func admissionDial(t *testing.T, url, id, token string) *websocket.Conn {
	t.Helper()
	c, _, err := dialWith(t, url, admissionHeaders(id, token))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func TestDaemonAdmission_AuthorizedConflict(t *testing.T) {
	reg := NewRegistry()
	incumbent := &admissionIncumbent{fakeConn: fakeConn{id: "incumbent"}}
	if err := reg.ClaimServer("s1", incumbent); err != nil {
		t.Fatal(err)
	}
	c := admissionDial(t, admissionServer(t, admissionFixture(t, "enforce"), reg), "s1", admissionOld)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != 4409 || incumbent.probes.Load() != 1 || incumbent.isClosed() {
		t.Fatalf("authorized conflict: %v", err)
	}
}

func TestDaemonAdmission_ConcurrentReconnects(t *testing.T) {
	reg := NewRegistry()
	incumbent := &admissionIncumbent{fakeConn: fakeConn{id: "incumbent"}}
	if err := reg.ClaimServer("s1", incumbent); err != nil {
		t.Fatal(err)
	}
	url := admissionServer(t, admissionFixture(t, "enforce"), reg)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := admissionOld
			if i%2 == 0 {
				token = admissionNew
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: admissionHeaders("s1", token)})
			if err != nil {
				t.Error(err)
				return
			}
			defer c.CloseNow()
			_, _, err = c.Read(ctx)
			if websocket.CloseStatus(err) != 4409 {
				t.Errorf("reconnect: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if incumbent.probes.Load() != 12 {
		t.Fatal("reconnect did not reach conflict path")
	}
}

func TestDaemonAdmission_SnapshotRestart(t *testing.T) {
	env := admissionEnv("enforce", `{"s1":["`+admissionOld+`","`+admissionNew+`"],"s2":["`+admissionOther+`"]}`)
	old, err := NewDaemonAdmissionFromEnv(admissionLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	env[envDaemonAdmissionCredentials] = `{"s1":["` + admissionNew + `"]}`
	fresh, err := NewDaemonAdmissionFromEnv(admissionLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	for _, tc := range []struct {
		a         *DaemonAdmission
		id, token string
		want      int
	}{{old, "s1", admissionOld, 204}, {old, "s1", admissionNew, 204}, {old, "s2", admissionOther, 204}, {fresh, "s1", admissionOld, 401}, {fresh, "s1", admissionNew, 204}, {fresh, "s2", admissionOther, 401}} {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Header = admissionHeaders(tc.id, tc.token)
		tc.a.Wrap(next, NewMetricsRegistry()).ServeHTTP(rr, r)
		if rr.Code != tc.want {
			t.Fatalf("snapshot decision = %d, want %d", rr.Code, tc.want)
		}
	}
	reg := NewRegistry()
	c := admissionDial(t, admissionServer(t, old, reg), "s1", admissionOld)
	awaitBinary(t, reg, "s1")
	read := make(chan error, 1)
	go func() { _, _, err := c.Read(context.Background()); read <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Shutdown(ctx, discardLogger(), reg); err != nil {
		t.Fatal(err)
	}
	if err := <-read; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("restart close: %v", err)
	}
	newReg := NewRegistry()
	url := admissionServer(t, fresh, newReg)
	_, resp, err := dialWith(t, url, admissionHeaders("s1", admissionOld))
	if err == nil {
		t.Fatal("revoked credential upgraded")
	}
	assertAdmissionRejection(t, resp)
	admissionDial(t, url, "s1", admissionNew)
	awaitBinary(t, newReg, "s1")
	newReg.mu.RLock()
	defer newReg.mu.RUnlock()
	if len(newReg.timers) != 0 || len(newReg.phones) != 0 {
		t.Fatal("restart inherited grace/phones")
	}
}

func TestDaemonAdmission_ReconnectForwardPush(t *testing.T) {
	reg := NewRegistry()
	waker, sender, _ := budgetWaker(t, pushWakeRefillEvery, pushWakeBurst, pushWakeMaxInFlight)
	defer waker.Close()
	reg.SetPushWaker(waker)
	url := admissionServer(t, admissionFixture(t, "enforce"), reg)
	first := admissionDial(t, url, "s1", admissionOld)
	awaitBinary(t, reg, "s1")
	wake := `{"push_wake":{"platform":"fcm","token":"` + testWakeToken + `"}}`
	if err := first.Write(context.Background(), websocket.MessageText, []byte(wake)); err != nil {
		t.Fatal(err)
	}
	sender.waitStarted(t, 1)
	phoneSrv := httptest.NewServer(ClientHandler(reg, discardLogger(), 256*1024, 16, nil))
	defer phoneSrv.Close()
	phoneURL := "ws" + strings.TrimPrefix(phoneSrv.URL, "http")
	phone, _, err := dialWithClient(t, phoneURL, validClientHeaders("s1"))
	if err != nil {
		t.Fatal(err)
	}
	defer phone.CloseNow()
	waitForPhones(t, reg, "s1", 1, time.Second)
	first.CloseNow()
	deadline := time.Now().Add(time.Second)
	for {
		reg.mu.RLock()
		gracing := reg.timers["s1"] != nil
		reg.mu.RUnlock()
		if gracing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no grace")
		}
		time.Sleep(time.Millisecond)
	}
	second := admissionDial(t, url, "s1", admissionNew)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = phone.Read(ctx)
	if websocket.CloseStatus(err) != 4404 {
		t.Fatalf("grace reclaim did not evict phone: %v", err)
	}
	phone, _, err = dialWithClient(t, phoneURL, validClientHeaders("s1"))
	if err != nil {
		t.Fatal(err)
	}
	defer phone.CloseNow()
	phones := waitForPhones(t, reg, "s1", 1, time.Second)
	opaque := []byte(`{"type":"noise_msg","payload":"synthetic-ciphertext"}`)
	env, err := Marshal(phones[0].ConnID(), opaque)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Write(ctx, websocket.MessageText, env); err != nil {
		t.Fatal(err)
	}
	_, got, err := phone.Read(ctx)
	if err != nil || string(got) != string(opaque) {
		t.Fatalf("opaque forward: %q, %v", got, err)
	}
	wake = strings.Replace(wake, testWakeToken, testWakeToken+"-other", 1) // A distinct device job without refilling debt.
	if err := second.Write(ctx, websocket.MessageText, []byte(wake)); err != nil {
		t.Fatal(err)
	}
	sender.waitStarted(t, 1)
	waker.mu.Lock()
	globalTokens := waker.global.tokens
	daemonTokens := waker.daemons[sha256.Sum256([]byte("s1"))].tokens
	deviceTokens := waker.devices[sha256.Sum256([]byte(testWakeToken))].tokens
	otherDeviceTokens := waker.devices[sha256.Sum256([]byte(testWakeToken+"-other"))].tokens
	waker.mu.Unlock()
	if globalTokens != 30 || daemonTokens != 4 || deviceTokens != 5 || otherDeviceTokens != 5 {
		t.Fatal("reconnect reset push allowances")
	}
}

func TestDaemonAdmission_OffObserveKeepLegacyHandler(t *testing.T) {
	for _, mode := range []string{"off", "observe"} {
		t.Run(mode, func(t *testing.T) {
			reg := NewRegistry()
			url := admissionServer(t, admissionFixture(t, mode), reg)
			h := admissionHeaders("legacy", "missing")
			h.Add("X-Pyrycode-Server", "legacy")
			h.Add("X-Pyrycode-Admission-Token", "malformed")
			c, _, err := dialWith(t, url, h)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			awaitBinary(t, reg, "legacy")
			h = admissionHeaders("different", "missing")
			h.Del("X-Pyrycode-Version")
			_, resp, err := dialWith(t, url, h)
			if err == nil || resp.StatusCode != 400 {
				t.Fatalf("legacy header validation: %v", err)
			}
			resp.Body.Close()
		})
	}
}
