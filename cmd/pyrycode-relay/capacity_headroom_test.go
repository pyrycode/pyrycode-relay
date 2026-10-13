package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRun_UpgradeRateValidation(t *testing.T) {
	for _, flags := range [][]string{
		{"--upgrade-refill-interval", "0s"},
		{"--upgrade-refill-interval", "-1s"},
		{"--upgrade-burst", "0"},
		{"--upgrade-burst", "-1"},
	} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			args := append([]string{"--insecure-listen", capAddr(t), "--metrics-listen", ""}, flags...)
			if got := run(args, ctx); got != 2 {
				t.Fatalf("invalid rate setting: exit=%d, want 2", got)
			}
		})
	}
}

func TestRun_CapacityHeadroomProfiles(t *testing.T) {
	for _, profile := range []struct {
		total, source, raw int
	}{
		{64, 48, 128},
		{128, 96, 256},
	} {
		t.Run(fmt.Sprintf("%d-total", profile.total), func(t *testing.T) {
			addr := capAddr(t)
			ctx, cancel := context.WithCancel(context.Background())
			exit := make(chan int, 1)
			go func() {
				exit <- run([]string{
					"--insecure-listen", addr, "--metrics-listen", "",
					"--trust-x-forwarded-for",
					"--max-connections", fmt.Sprint(profile.total),
					"--max-connections-per-source", fmt.Sprint(profile.source),
					"--max-listener-connections", fmt.Sprint(profile.raw),
					"--upgrade-refill-interval", "10ms", "--upgrade-burst", "200",
				}, ctx)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case got := <-exit:
					if got != 0 {
						t.Errorf("shutdown exit=%d", got)
					}
				case <-time.After(12 * time.Second):
					t.Error("relay did not finish shutdown")
				}
			})
			if err := waitForDial(addr, 3*time.Second); err != nil {
				t.Fatal(err)
			}

			dial := func(source string, n int) (*websocket.Conn, *http.Response, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				return websocket.Dial(ctx, "ws://"+addr+"/v1/server", &websocket.DialOptions{HTTPHeader: http.Header{
					"X-Forwarded-For":    {source},
					"X-Pyrycode-Server":  {fmt.Sprintf("capacity-%s-%d", source, n)},
					"X-Pyrycode-Version": {"0.0.0"}, "User-Agent": {"capacity-test"},
				}})
			}
			open := func(source string, n int) {
				c, _, err := dial(source, n)
				if err != nil {
					t.Fatalf("connection %s/%d refused: %v", source, n, err)
				}
				t.Cleanup(func() { c.CloseNow() })
				_ = readPump(c)
				pingCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				if err := c.Ping(pingCtx); err != nil {
					t.Fatalf("admission ping: %v", err)
				}
			}
			deny := func(source string, n int) {
				c, resp, err := dial(source, n)
				if err == nil {
					c.CloseNow()
					t.Fatal("connection exceeded configured allowance")
				}
				if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("want capacity 503, got response=%v error=%v", resp, err)
				}
				resp.Body.Close()
			}
			for i := range profile.source {
				open("192.0.2.10", i)
			}
			deny("192.0.2.10", profile.source)
			for i := range profile.total - profile.source {
				open("192.0.2.20", i)
			}
			deny("192.0.2.20", profile.total)
		})
	}
}
