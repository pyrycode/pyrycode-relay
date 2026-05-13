package relay

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnexpectedCapability is returned by CheckCapabilities when the
// process's effective Linux capability set (CapEff) contains a bit
// outside AllowedCapabilities. Stray capabilities (CAP_SYS_ADMIN,
// CAP_NET_ADMIN, etc.) usually mean a misconfigured container runtime
// — --cap-add in a Docker run, an over-broad bounding set, or a
// default profile granting more than the relay needs. The relay
// refuses to start so the misconfiguration fails the deploy's
// health check rather than running with elevated privilege.
//
// Branchable via errors.Is. The wrapped error message identifies
// each unexpected bit (numeric position plus symbolic name where
// known) and the current allowlist.
var ErrUnexpectedCapability = errors.New("relay: process has unexpected effective Linux capabilities")

// Capability is a Linux capability bit and its symbolic name.
type Capability struct {
	// Bit is the 0-based bit position as defined in <linux/capability.h>.
	Bit uint
	// Name is the kernel CAP_* macro (e.g. "CAP_NET_BIND_SERVICE").
	// Empty for unknown bits.
	Name string
}

// AllowedCapabilities is the explicit allowlist of effective Linux
// capabilities the relay legitimately needs.
//
// CAP_NET_BIND_SERVICE (bit 10) is included because the autocert mode
// binds :80 and :443 from uid 65532 inside the distroless image
// (Dockerfile, fly.toml). Hosts that drop this cap and instead lower
// net.ipv4.ip_unprivileged_port_start are fine — CapEff will be 0 and
// the check passes; hosts that grant it via Docker's default profile
// are also fine — CapEff has the bit set and it matches the allowlist.
//
// All other Linux capabilities are unexpected. To extend, add an entry
// and document the deployment shape that requires it.
var AllowedCapabilities = []Capability{
	{Bit: 10, Name: "CAP_NET_BIND_SERVICE"},
}

// capabilityNames maps a Linux capability bit position to its kernel
// CAP_* macro name. Indexed by bit position; tracks
// include/uapi/linux/capability.h through CAP_CHECKPOINT_RESTORE
// (bit 40, kernel 5.9). Bit positions are stable kernel ABI; new
// capabilities only ever append.
var capabilityNames = []string{
	0:  "CAP_CHOWN",
	1:  "CAP_DAC_OVERRIDE",
	2:  "CAP_DAC_READ_SEARCH",
	3:  "CAP_FOWNER",
	4:  "CAP_FSETID",
	5:  "CAP_KILL",
	6:  "CAP_SETGID",
	7:  "CAP_SETUID",
	8:  "CAP_SETPCAP",
	9:  "CAP_LINUX_IMMUTABLE",
	10: "CAP_NET_BIND_SERVICE",
	11: "CAP_NET_BROADCAST",
	12: "CAP_NET_ADMIN",
	13: "CAP_NET_RAW",
	14: "CAP_IPC_LOCK",
	15: "CAP_IPC_OWNER",
	16: "CAP_SYS_MODULE",
	17: "CAP_SYS_RAWIO",
	18: "CAP_SYS_CHROOT",
	19: "CAP_SYS_PTRACE",
	20: "CAP_SYS_PACCT",
	21: "CAP_SYS_ADMIN",
	22: "CAP_SYS_BOOT",
	23: "CAP_SYS_NICE",
	24: "CAP_SYS_RESOURCE",
	25: "CAP_SYS_TIME",
	26: "CAP_SYS_TTY_CONFIG",
	27: "CAP_MKNOD",
	28: "CAP_LEASE",
	29: "CAP_AUDIT_WRITE",
	30: "CAP_AUDIT_CONTROL",
	31: "CAP_SETFCAP",
	32: "CAP_MAC_OVERRIDE",
	33: "CAP_MAC_ADMIN",
	34: "CAP_SYSLOG",
	35: "CAP_WAKE_ALARM",
	36: "CAP_BLOCK_SUSPEND",
	37: "CAP_AUDIT_READ",
	38: "CAP_PERFMON",
	39: "CAP_BPF",
	40: "CAP_CHECKPOINT_RESTORE",
}

// capabilityName returns the symbolic name for a Linux capability bit
// (e.g. "CAP_SYS_ADMIN" for bit 21), or the empty string if unknown.
func capabilityName(bit uint) string {
	if int(bit) >= len(capabilityNames) {
		return ""
	}
	return capabilityNames[bit]
}

// allowedMask returns the bitwise OR of every Bit in AllowedCapabilities.
func allowedMask() uint64 {
	var m uint64
	for _, c := range AllowedCapabilities {
		m |= uint64(1) << c.Bit
	}
	return m
}

// parseCapEff extracts the CapEff: hex mask from /proc/self/status
// content. The expected line shape is:
//
//	CapEff:\t0000000000000400
//
// The whitespace separator may be tab or spaces; the value is 0–16 hex
// digits with no 0x prefix. Returns a wrapped error if the line is
// missing or the value does not parse as hex. Does NOT wrap
// ErrUnexpectedCapability — malformed input is a separate failure mode
// from "capabilities exceed allowlist".
func parseCapEff(procStatus string) (uint64, error) {
	for _, line := range strings.Split(procStatus, "\n") {
		rest, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		value := strings.TrimSpace(rest)
		mask, err := strconv.ParseUint(value, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("relay: parsing /proc/self/status CapEff %q: %w", value, err)
		}
		return mask, nil
	}
	return 0, fmt.Errorf("relay: /proc/self/status missing CapEff line")
}

// checkCapEffMask returns ErrUnexpectedCapability (wrapped with the
// offending bit list and the allowlist contents) if mask has any bit
// set outside AllowedCapabilities. Returns nil otherwise.
//
// All unexpected bits are reported, not just the first — a misconfigured
// manifest often grants several caps in one breath. Unknown bits (no
// entry in capabilityNames) are reported as "bit N".
func checkCapEffMask(mask uint64) error {
	unexpected := mask &^ allowedMask()
	if unexpected == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; allowlist: %s; drop with --cap-drop=ALL --cap-add=NET_BIND_SERVICE or equivalent",
		ErrUnexpectedCapability, formatBits(unexpected), formatAllowlist())
}

// formatBits renders the set bits of mask as a comma-separated list of
// "CAP_NAME (bit N)" or "bit N" if the bit has no entry in capabilityNames.
// Bits are listed in ascending order.
func formatBits(mask uint64) string {
	var parts []string
	for bit := uint(0); bit < 64; bit++ {
		if mask&(uint64(1)<<bit) == 0 {
			continue
		}
		if name := capabilityName(bit); name != "" {
			parts = append(parts, fmt.Sprintf("%s (bit %d)", name, bit))
		} else {
			parts = append(parts, fmt.Sprintf("bit %d", bit))
		}
	}
	return strings.Join(parts, ", ")
}

// formatAllowlist renders AllowedCapabilities as "[CAP_FOO (bit N), CAP_BAR (bit M)]"
// or "[]" if empty.
func formatAllowlist() string {
	parts := make([]string, 0, len(AllowedCapabilities))
	for _, c := range AllowedCapabilities {
		parts = append(parts, fmt.Sprintf("%s (bit %d)", c.Name, c.Bit))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
