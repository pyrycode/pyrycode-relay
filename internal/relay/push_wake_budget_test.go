package relay

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPushWaker_SharedAllowances(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      int
		device func(int) string
	}{
		{"device across identities", 6, func(int) string { return testWakeToken }},
		{"global across devices and identities", 32, func(i int) string { return fmt.Sprint("device-", i) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s, _ := budgetWaker(t, pushWakeRefillEvery, pushWakeBurst, pushWakeMaxInFlight)
			defer w.Close()
			for i := 0; i < tc.n; i++ {
				if err := w.Request(fmt.Sprint("daemon-", i), wakeJSON("fcm", tc.device(i))); err != nil {
					t.Fatal(err)
				}
				s.waitStarted(t, 1)
				w.wg.Wait() // Free slots without resetting accounting or advancing time.
			}
			if err := w.Request("new-identity", wakeJSON("fcm", tc.device(tc.n))); !errors.Is(err, ErrPushWakeRateLimited) {
				t.Fatalf("wake over shared burst = %v", err)
			}
			if got := len(s.sentTokens()); got != tc.n {
				t.Fatalf("sends = %d, want %d", got, tc.n)
			}
		})
	}
}

func budgetWaker(t *testing.T, refill time.Duration, burst, slots int) (*PushWaker, *fakePushSender, func(time.Duration)) {
	t.Helper()
	s := newFakePushSender()
	w := newPushWaker(s, refill, burst, slots, discardLogger())
	now := time.Unix(1000, 0)
	w.mu.Lock()
	w.now = func() time.Time { return now }
	w.global.refillLast = now
	w.mu.Unlock()
	t.Cleanup(w.Close)
	return w, s, func(d time.Duration) { w.mu.Lock(); now = now.Add(d); w.mu.Unlock() }
}

func budgetSend(t *testing.T, w *PushWaker, s *fakePushSender, daemon, device string) {
	t.Helper()
	if err := w.Request(daemon, wakeJSON("fcm", device)); err != nil {
		t.Fatal(err)
	}
	s.waitStarted(t, 1)
	w.wg.Wait()
}

func TestPushWaker_PairedDeviceBurstAndCompleteKeys(t *testing.T) {
	w, s, _ := budgetWaker(t, pushWakeRefillEvery, pushWakeBurst, pushWakeMaxInFlight)
	for daemon := 0; daemon < 4; daemon++ {
		for device := 0; device < 3; device++ {
			for event := 0; event < 2; event++ {
				budgetSend(t, w, s, fmt.Sprint("daemon-", daemon), fmt.Sprintf("device-%d-%d", daemon, device))
			}
		}
	}
	if got := len(s.sentTokens()); got != 24 {
		t.Fatalf("paired burst = %d", got)
	}
	// Complete tokens sharing a long prefix or a log fingerprint remain separate.
	collision := []string{"synthetic-collision-26125", "synthetic-collision-102129"}
	if tokenFingerprint(collision[0]) != tokenFingerprint(collision[1]) {
		t.Fatal("collision fixture changed")
	}
	prefix := strings.Repeat("synthetic-", 1000)
	for _, tokens := range [][]string{{prefix + "0", prefix + "1"}, collision} {
		w2, s2, _ := budgetWaker(t, pushWakeRefillEvery, pushWakeBurst, pushWakeMaxInFlight)
		for i := 0; i < 12; i++ {
			budgetSend(t, w2, s2, fmt.Sprint(i), tokens[i%2])
		}
		if len(w2.devices) != 2 {
			t.Fatal("complete device tokens were conflated")
		}
	}
}

func TestPushWaker_RefillBoundaries(t *testing.T) {
	for _, kind := range []string{"daemon", "device", "global"} {
		t.Run(kind, func(t *testing.T) {
			w, s, advance := budgetWaker(t, pushWakeRefillEvery, pushWakeBurst, pushWakeMaxInFlight)
			request := func(i int) error {
				d, token := fmt.Sprint("daemon-", i), fmt.Sprint("device-", i)
				if kind == "daemon" {
					d = "same-daemon"
				}
				if kind == "device" {
					token = testWakeToken
				}
				return w.Request(d, wakeJSON("fcm", token))
			}
			n, interval := 6, 10*time.Second
			if kind == "global" {
				n, interval = 32, time.Second
			}
			for i := 0; i < n; i++ {
				if err := request(i); err != nil {
					t.Fatal(err)
				}
				s.waitStarted(t, 1)
				w.wg.Wait()
			}
			advance(interval - time.Nanosecond)
			if err := request(n); !errors.Is(err, ErrPushWakeRateLimited) {
				t.Fatalf("before refill = %v", err)
			}
			advance(time.Nanosecond)
			if err := request(n); err != nil {
				t.Fatalf("exact refill = %v", err)
			}
			s.waitStarted(t, 1)
			w.wg.Wait()
			if err := request(n + 1); !errors.Is(err, ErrPushWakeRateLimited) {
				t.Fatalf("extra refill token = %v", err)
			}
			advance(time.Hour)
			for i := 0; i < n; i++ {
				if err := request(100 + i); err != nil {
					t.Fatal(err)
				}
				s.waitStarted(t, 1)
				w.wg.Wait()
			}
			if err := request(200); !errors.Is(err, ErrPushWakeRateLimited) {
				t.Fatalf("refill exceeded burst = %v", err)
			}
		})
	}
}

// snapshot ignores timestamps, which must advance on a refusal, but pins every
// retained token count and slot so partial charges and temporary inserts fail.
func budgetSnapshot(w *PushWaker) [4]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	copyTokens := func(buckets map[[32]byte]*pushWakeBucket) map[[32]byte]int {
		result := make(map[[32]byte]int)
		for k, b := range buckets {
			result[k] = b.tokens
		}
		return result
	}
	return [4]any{copyTokens(w.daemons), copyTokens(w.devices), w.global.tokens, len(w.slots)}
}

func TestPushWaker_AtomicRefusals(t *testing.T) {
	for _, kind := range []string{"daemon", "device", "global", "concurrency", "daemon capacity", "device capacity"} {
		t.Run(kind, func(t *testing.T) {
			w, s, _ := budgetWaker(t, time.Hour, 1, 1)
			d, token := "push-sid-new", testWakeToken
			dk, tk := sha256.Sum256([]byte(d)), sha256.Sum256([]byte(token))
			want := ErrPushWakeRateLimited
			w.mu.Lock()
			now := w.now()
			w.daemons[dk] = &pushWakeBucket{tokens: 1, refillLast: now, pollLast: now}
			w.devices[tk] = &pushWakeBucket{tokens: 6, refillLast: now, pollLast: now}
			switch kind {
			case "daemon":
				w.daemons[dk].tokens = 0
			case "device":
				w.devices[tk].tokens = 0
			case "global":
				w.global.tokens = 0
			case "concurrency":
				w.slots <- struct{}{}
				want = ErrPushWakeInFlightCap
			default:
				buckets := w.daemons
				key := dk
				if kind == "device capacity" {
					buckets, key = w.devices, tk
				}
				delete(buckets, key)
				for i := 0; i < pushWakeMaxBuckets; i++ {
					buckets[sha256.Sum256([]byte(fmt.Sprint(i)))] = &pushWakeBucket{tokens: 1, refillLast: now, pollLast: now}
				}
				want = ErrPushWakeStateCap
			}
			w.mu.Unlock()
			before := budgetSnapshot(w)
			h := newPushMetricsBundle(w)
			err := w.Request(d, wakeJSON("fcm", token))
			if !errors.Is(err, want) {
				t.Fatalf("refusal = %v, want %v", err, want)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), d) {
				t.Fatal("secret in refusal")
			}
			assertPushWakeOutcomes(t, h, map[string]int{"dropped": 1})
			if len(s.sentTokens()) != 0 {
				t.Fatal("refusal dispatched")
			}
			if !reflect.DeepEqual(before, budgetSnapshot(w)) {
				t.Fatal("refusal changed tokens, state keys or slots")
			}
			w.mu.Lock()
			// Remove only the blocking fixture, then prove every allowance is usable.
			switch kind {
			case "daemon":
				w.daemons[dk].tokens = 1
			case "device":
				w.devices[tk].tokens = 6
			case "global":
				w.global.tokens = 1
			case "concurrency":
				<-w.slots
			case "daemon capacity":
				w.daemons = make(map[[32]byte]*pushWakeBucket)
			case "device capacity":
				w.devices = make(map[[32]byte]*pushWakeBucket)
			}
			w.mu.Unlock()
			budgetSend(t, w, s, d, token)
			w.mu.Lock()
			dt, tt := w.daemons[dk].tokens, w.devices[tk].tokens
			w.mu.Unlock()
			if dt != 0 || tt != 5 {
				t.Fatal("admission did not charge both keyed allowances")
			}
			assertPushWakeOutcomes(t, h, map[string]int{"dropped": 1, "sent": 1})
		})
	}
}

func TestPushWaker_FailedSendStaysCharged(t *testing.T) {
	w, s, _ := budgetWaker(t, time.Hour, 1, 32)
	s.err = ErrFCMSend
	budgetSend(t, w, s, "daemon", testWakeToken)
	if err := w.Request("daemon", wakeJSON("fcm", "other-device")); !errors.Is(err, ErrPushWakeRateLimited) {
		t.Fatalf("failure refunded daemon: %v", err)
	}
	if w.global.tokens != 31 || w.devices[sha256.Sum256([]byte(testWakeToken))].tokens != 5 {
		t.Fatal("failure refunded shared allowance")
	}
}

func TestPushWaker_StateBoundsAndExpiry(t *testing.T) {
	if pushWakeMaxBuckets != 1024 {
		t.Fatal("production keyed capacity changed")
	}
	for _, kind := range []string{"daemon", "device"} {
		t.Run(kind, func(t *testing.T) {
			w, s, advance := budgetWaker(t, pushWakeRefillEvery, pushWakeBurst, 32)
			buckets := w.daemons
			if kind == "device" {
				buckets = w.devices
			}
			w.mu.Lock()
			for i := 0; i < pushWakeMaxBuckets; i++ {
				buckets[sha256.Sum256([]byte(fmt.Sprint("key-", i)))] = &pushWakeBucket{tokens: 6, refillLast: w.now(), pollLast: w.now()}
			}
			w.mu.Unlock()
			d, token := "new-daemon", "new-device"
			if kind == "daemon" {
				d = "key-0"
			} else {
				token = "key-0"
			}
			budgetSend(t, w, s, d, token) // Existing keys work at capacity.
			d, token = "new-daemon", "new-device"
			advance(5*time.Minute - time.Nanosecond)
			if err := w.Request(d, wakeJSON("fcm", token)); !errors.Is(err, ErrPushWakeStateCap) {
				t.Fatalf("premature reclamation: %v", err)
			}
			if len(buckets) != 1024 {
				t.Fatal("capacity changed before expiry")
			}
			advance(time.Nanosecond)
			budgetSend(t, w, s, d, token) // New-key admission reclaims eligible state.
			if len(buckets) != 1 {
				t.Fatalf("idle state retained: %d", len(buckets))
			}
			if kind == "daemon" {
				d = "key-0"
			} else {
				token = "key-0"
			}
			budgetSend(t, w, s, d, token) // Reuse of a fully refilled evicted key is safe.
		})
	}
	t.Run("refusals renew idle timestamp", func(t *testing.T) {
		w, s, advance := budgetWaker(t, pushWakeRefillEvery, 6, 1)
		s.block = make(chan struct{})
		if err := w.Request("daemon", wakeJSON("fcm", testWakeToken)); err != nil {
			t.Fatal(err)
		}
		s.waitStarted(t, 1)
		advance(4 * time.Minute)
		if err := w.Request("daemon", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushWakeInFlightCap) {
			t.Fatal(err)
		}
		advance(time.Minute)
		w.sweep()
		if len(w.daemons) != 1 || len(w.devices) != 1 {
			t.Fatal("refused active state expired")
		}
		advance(4*time.Minute - time.Nanosecond)
		w.sweep()
		if len(w.daemons) != 1 || len(w.devices) != 1 {
			t.Fatal("idle state expired early")
		}
		advance(time.Nanosecond)
		w.sweep()
		if len(w.daemons) != 0 || len(w.devices) != 0 {
			t.Fatal("idle state not swept")
		}
	})
	t.Run("depleted state survives expiry and capacity pressure", func(t *testing.T) {
		w, _, advance := budgetWaker(t, time.Hour, 1, 32)
		w.mu.Lock()
		for i := 0; i < 1024; i++ {
			w.daemons[sha256.Sum256([]byte(fmt.Sprint(i)))] = &pushWakeBucket{refillLast: w.now(), pollLast: w.now()}
		}
		w.mu.Unlock()
		advance(5 * time.Minute)
		w.sweep()
		if len(w.daemons) != 1024 {
			t.Fatal("debt evicted")
		}
		if err := w.Request("new-identity", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushWakeStateCap) {
			t.Fatal(err)
		}
		if err := w.Request("0", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushWakeRateLimited) {
			t.Fatalf("reused depleted identity = %v", err)
		}
		advance(55 * time.Minute)
		w.sweep()
		if len(w.daemons) != 0 {
			t.Fatal("fully refilled idle state not reclaimed")
		}
	})
}

func TestPushWaker_ConcurrentAdmission(t *testing.T) {
	for _, kind := range []string{"daemon", "device", "global", "concurrency"} {
		t.Run(kind, func(t *testing.T) {
			slots, want := 32, 6
			if kind == "global" {
				want = 32
			}
			if kind == "concurrency" {
				slots, want = 2, 2
			}
			w, s, _ := budgetWaker(t, pushWakeRefillEvery, 6, slots)
			s.block = make(chan struct{})
			var admitted atomic.Int32
			var requests sync.WaitGroup
			for i := 0; i < 96; i++ {
				requests.Add(1)
				go func(i int) {
					defer requests.Done()
					d, token := fmt.Sprint("daemon-", i), fmt.Sprint("device-", i)
					if kind == "daemon" {
						d = "same"
					}
					if kind == "device" {
						token = testWakeToken
					}
					err := w.Request(d, wakeJSON("fcm", token))
					if err == nil {
						admitted.Add(1)
					} else if !errors.Is(err, ErrPushWakeRateLimited) && !errors.Is(err, ErrPushWakeInFlightCap) {
						t.Error(err)
					}
				}(i)
			}
			requests.Wait()
			s.waitStarted(t, want)
			if int(admitted.Load()) != want || len(s.sentTokens()) != want {
				t.Fatalf("admitted %d, want %d", admitted.Load(), want)
			}
			w.Close()
		})
	}
}

func TestPushWaker_AdmissionRacesClose(t *testing.T) {
	w, s, _ := budgetWaker(t, pushWakeRefillEvery, 6, 32)
	s.block = make(chan struct{})
	h := newPushMetricsBundle(w)
	var admitted atomic.Int32
	if err := w.Request("initial", wakeJSON("fcm", "initial-device")); err != nil {
		t.Fatal(err)
	}
	admitted.Add(1)
	s.waitStarted(t, 1)
	var work sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 48; i++ {
		work.Add(1)
		go func(i int) {
			defer work.Done()
			<-start
			err := w.Request(fmt.Sprint("daemon-", i), wakeJSON("fcm", fmt.Sprint("device-", i)))
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrPushOff) && !errors.Is(err, ErrPushWakeRateLimited) && !errors.Is(err, ErrPushWakeInFlightCap) {
				t.Error(err)
			}
		}(i)
	}
	for i := 0; i < 3; i++ {
		work.Add(1)
		go func() {
			defer work.Done()
			<-start
			w.Close()
			if len(w.slots) != 0 {
				t.Error("Close returned before sends joined")
			}
		}()
	}
	close(start)
	work.Wait()
	if len(s.sentTokens()) != int(admitted.Load()) {
		t.Fatal("admitted work not joined")
	}
	if err := w.Request("late", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushOff) {
		t.Fatal(err)
	}
	assertCounter(t, h, "pyrycode_relay_push_wakes_total", `outcome="send_failed"`, int(admitted.Load()))
}
