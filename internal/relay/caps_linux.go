package relay

import (
	"fmt"
	"os"
)

// CheckCapabilities reads /proc/self/status's CapEff line and returns
// ErrUnexpectedCapability (wrapped) if the effective capability set
// contains any bit outside AllowedCapabilities. Returns nil otherwise.
//
// Intended to be called from main after flag parse, before any listener
// is started. Read errors on /proc/self/status are returned wrapped
// (not as ErrUnexpectedCapability) so callers can distinguish "kernel
// /proc gone weird" from "operator handed us extra caps."
//
// Only CapEff is consulted — CapPrm/CapBnd/CapInh would broaden the
// false-positive surface (legitimate Kubernetes default policies grant
// a wide CapBnd) without adding load-bearing protection: the relay
// never calls capset(2), so CapPrm bits not in CapEff are inert.
func CheckCapabilities() error {
	return checkCapabilitiesWithReader(readProcSelfStatus)
}

func readProcSelfStatus() (string, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", fmt.Errorf("relay: reading /proc/self/status: %w", err)
	}
	return string(data), nil
}

// checkCapabilitiesWithReader is the test seam. Tests pass a closure
// returning canned /proc/self/status contents; production passes
// readProcSelfStatus.
func checkCapabilitiesWithReader(readStatus func() (string, error)) error {
	status, err := readStatus()
	if err != nil {
		return err
	}
	mask, err := parseCapEff(status)
	if err != nil {
		return err
	}
	return checkCapEffMask(mask)
}
