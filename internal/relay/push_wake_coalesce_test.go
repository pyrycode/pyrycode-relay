package relay

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPushWaker_CoalescesConcurrentDuplicates(t *testing.T) {
	w, s, _ := budgetWaker(t, time.Hour, 1, 1)
	s.block = make(chan struct{})
	logger, logs := captureLogger()
	w.logger = logger
	h := newPushMetricsBundle(w)
	if err := w.Request("first-daemon", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatal(err)
	}
	s.waitStarted(t, 1)
	// Duplicates must bypass depleted allowances and saturated accounting maps.
	w.mu.Lock()
	w.global.tokens = 0
	w.devices[sha256.Sum256([]byte(testWakeToken))].tokens = 0
	for i := len(w.daemons); i < pushWakeMaxBuckets; i++ {
		w.daemons[sha256.Sum256([]byte(fmt.Sprint(i)))] = &pushWakeBucket{tokens: 0, refillLast: w.now(), pollLast: w.now()}
	}
	w.mu.Unlock()
	before := budgetSnapshot(w)
	var requests sync.WaitGroup
	for i := 0; i < 64; i++ {
		requests.Add(1)
		go func(i int) {
			defer requests.Done()
			daemon := "first-daemon"
			if i%2 == 0 {
				daemon = fmt.Sprint("other-daemon-", i)
			}
			if err := w.Request(daemon, wakeJSON("fcm", testWakeToken)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	requests.Wait()
	if !reflect.DeepEqual(before, budgetSnapshot(w)) {
		t.Fatal("duplicates changed accounting")
	}
	if len(s.sentTokens()) != 1 {
		t.Fatal("duplicates sent")
	}
	assertPushWakeOutcomes(t, h, nil)
	w.Close()
	assertPushWakeOutcomes(t, h, map[string]int{"send_failed": 1})
	lines := outcomeLines(logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "server_id=first-daemon") || strings.Contains(logs.String(), testWakeToken) {
		t.Fatal("duplicate changed attribution or leaked token")
	}
}

func TestPushWaker_CompletedWindow(t *testing.T) {
	for _, sendErr := range []error{nil, ErrFCMSend} {
		t.Run(fmt.Sprint(sendErr), func(t *testing.T) {
			w, s, advance := budgetWaker(t, time.Hour, 6, 2)
			s.err = sendErr
			budgetSend(t, w, s, "first", testWakeToken)
			if err := w.Request("duplicate", wakeJSON("apns", testWakeToken)); !errors.Is(err, ErrUnsupportedPushPlatform) {
				t.Fatal("duplicate bypassed structural validation")
			}
			before := budgetSnapshot(w)
			advance(10*time.Second - time.Nanosecond)
			if err := w.Request("duplicate", wakeJSON("fcm", testWakeToken)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, budgetSnapshot(w)) {
				t.Fatal("completed duplicate changed accounting")
			}
			advance(time.Nanosecond)
			budgetSend(t, w, s, "second", testWakeToken)
			if len(s.sentTokens()) != 2 {
				t.Fatal("exact boundary did not start new job")
			}
			// The duplicate just before the first boundary must not move either window.
			advance(10*time.Second - time.Nanosecond)
			if err := w.Request("duplicate-again", wakeJSON("fcm", testWakeToken)); err != nil {
				t.Fatal(err)
			}
			advance(time.Nanosecond)
			budgetSend(t, w, s, "third", testWakeToken)
			if len(s.sentTokens()) != 3 {
				t.Fatal("second window extended")
			}
		})
	}
}

func TestPushWaker_ActivePastWindow(t *testing.T) {
	w, s, advance := budgetWaker(t, time.Hour, 6, 2)
	release := make(chan struct{})
	s.block = release
	if err := w.Request("first", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatal(err)
	}
	s.waitStarted(t, 1)
	advance(20 * time.Second)
	w.sweep()
	before := budgetSnapshot(w)
	if err := w.Request("duplicate", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, budgetSnapshot(w)) || len(s.sentTokens()) != 1 {
		t.Fatal("active job expired")
	}
	close(release)
	w.wg.Wait()
	budgetSend(t, w, s, "replacement", testWakeToken)
	if len(s.sentTokens()) != 2 {
		t.Fatal("completed expired job blocked reuse")
	}
}

func TestPushWaker_RetainedCapacity(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint("active=", active), func(t *testing.T) {
			w, s, advance := budgetWaker(t, time.Hour, 6, pushWakeMaxInFlight)
			release := make(chan struct{})
			if active {
				s.block = release
			}
			h := newPushMetricsBundle(w)
			for i := 0; i < 32; i++ {
				if err := w.Request(fmt.Sprint("daemon-", i), wakeJSON("fcm", fmt.Sprint("device-", i))); err != nil {
					t.Fatal(err)
				}
			}
			s.waitStarted(t, 32)
			if !active {
				w.wg.Wait()
			}
			w.mu.Lock()
			retained := len(w.jobs)
			w.mu.Unlock()
			if retained != 32 {
				t.Fatalf("retained jobs = %d", retained)
			}
			before := budgetSnapshot(w)
			if err := w.Request("new", wakeJSON("fcm", "new-device")); !errors.Is(err, ErrPushWakeInFlightCap) {
				t.Fatalf("retained cap = %v", err)
			}
			if err := w.Request("duplicate", wakeJSON("fcm", "device-0")); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, budgetSnapshot(w)) {
				t.Fatal("capacity refusal or duplicate charged")
			}
			advance(10 * time.Second)
			if active {
				if err := w.Request("new", wakeJSON("fcm", "new-device")); !errors.Is(err, ErrPushWakeInFlightCap) {
					t.Fatal("active jobs reclaimed")
				}
				close(release)
				w.wg.Wait()
			}
			budgetSend(t, w, s, "new", "new-device")
			if len(s.sentTokens()) != 33 {
				t.Fatal("refused wake queued or expired capacity not reclaimed")
			}
			w.mu.Lock()
			oldCredit := w.devices[sha256.Sum256([]byte("device-0"))].tokens
			w.mu.Unlock()
			w.mu.Lock()
			retained = len(w.jobs)
			w.mu.Unlock()
			if retained != 1 {
				t.Fatalf("expired jobs retained = %d", retained)
			}
			if oldCredit != 5 {
				t.Fatal("job expiry erased accounting debt")
			}
			dropped := 1
			if active {
				dropped++
			}
			assertPushWakeOutcomes(t, h, map[string]int{"sent": 33, "dropped": dropped})
		})
	}
}

func TestPushWaker_CoalescingRacesClose(t *testing.T) {
	w, s, _ := budgetWaker(t, time.Hour, 6, 2)
	s.block = make(chan struct{})
	if err := w.Request("first", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatal(err)
	}
	s.waitStarted(t, 1)
	start := make(chan struct{})
	var work sync.WaitGroup
	for i := 0; i < 64; i++ {
		work.Add(1)
		go func(i int) {
			defer work.Done()
			<-start
			if i%8 == 0 {
				w.Close()
				return
			}
			if err := w.Request(fmt.Sprint(i), wakeJSON("fcm", testWakeToken)); err != nil && !errors.Is(err, ErrPushOff) {
				t.Error(err)
			}
		}(i)
	}
	close(start)
	work.Wait()
	if len(s.sentTokens()) != 1 || len(w.slots) != 0 || len(w.jobs) != 0 {
		t.Fatal("close did not join one shared job")
	}
	if err := w.Request("late", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushOff) {
		t.Fatal(err)
	}
}
