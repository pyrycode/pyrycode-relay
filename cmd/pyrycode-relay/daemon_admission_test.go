package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRun_DaemonAdmissionInvalidConfigBeforeListeners(t *testing.T) {
	for _, tc := range []struct {
		name, mode, credentials string
		supplied                bool
	}{
		{"bad mode", "ENFORCE", "{}", true},
		{"bad credentials off", "off", "synthetic-parser-excerpt", true},
		{"bad credentials observe", "observe", "null", true},
		{"bad credentials enforce", "enforce", "[]", true},
		{"missing credentials enforce", "enforce", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PYRYCODE_RELAY_DAEMON_ADMISSION_MODE", tc.mode)
			t.Setenv("PYRYCODE_RELAY_DAEMON_ADMISSION_CREDENTIALS", tc.credentials)
			if !tc.supplied {
				if err := os.Unsetenv("PYRYCODE_RELAY_DAEMON_ADMISSION_CREDENTIALS"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if code := run([]string{"--insecure-listen", "127.0.0.1:0"}, ctx); code != 2 {
				t.Fatalf("startup exit = %d, want 2", code)
			}
		})
	}
}
