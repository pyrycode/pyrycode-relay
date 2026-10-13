package relay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	envDaemonAdmissionMode        = "PYRYCODE_RELAY_DAEMON_ADMISSION_MODE"
	envDaemonAdmissionCredentials = "PYRYCODE_RELAY_DAEMON_ADMISSION_CREDENTIALS"
)

// ErrDaemonAdmissionCredentials deliberately carries no parser cause or input.
var ErrDaemonAdmissionCredentials = errors.New("relay: invalid daemon admission credentials (value withheld)")

// DaemonAdmission owns an immutable startup snapshot. Its credentials are never
// exposed, logged, forwarded or reloaded while requests are being served.
type DaemonAdmission struct {
	mode        string
	credentials map[string][]string
}

func validateDaemonAdmissionMode(mode string) error {
	switch mode {
	case "off", "observe", "enforce":
		return nil
	default:
		return errors.New("expected off, observe or enforce (value withheld)")
	}
}

func validAdmissionToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := range token {
		if !(token[i] >= '0' && token[i] <= '9' || token[i] >= 'a' && token[i] <= 'f') {
			return false
		}
	}
	return true
}

func parseDaemonAdmissionCredentials(raw string) (map[string][]string, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrDaemonAdmissionCredentials
	}
	credentials := make(map[string][]string)
	owners := make(map[string]string)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, ErrDaemonAdmissionCredentials
		}
		id, ok := key.(string)
		if !ok {
			return nil, ErrDaemonAdmissionCredentials
		}
		if _, duplicate := credentials[id]; duplicate {
			return nil, ErrDaemonAdmissionCredentials
		}
		var tokens []string
		if err := dec.Decode(&tokens); err != nil || tokens == nil {
			return nil, ErrDaemonAdmissionCredentials
		}
		for _, token := range tokens {
			if !validAdmissionToken(token) {
				return nil, ErrDaemonAdmissionCredentials
			}
			if owner, seen := owners[token]; seen && owner != id {
				return nil, ErrDaemonAdmissionCredentials
			}
			owners[token] = id
		}
		credentials[id] = tokens
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return nil, ErrDaemonAdmissionCredentials
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrDaemonAdmissionCredentials
	}
	return credentials, nil
}

// NewDaemonAdmissionFromEnv validates configuration even in off mode, copying
// decoded credentials into a private snapshot. All returned errors are static.
func NewDaemonAdmissionFromEnv(lookup func(string) (string, bool)) (*DaemonAdmission, error) {
	mode, present := lookup(envDaemonAdmissionMode)
	if !present {
		mode = "off"
	}
	if err := validateDaemonAdmissionMode(mode); err != nil {
		return nil, &ErrInvalidConfig{Key: envDaemonAdmissionMode, Reason: err.Error()}
	}
	raw, present := lookup(envDaemonAdmissionCredentials)
	if !present {
		if mode == "enforce" {
			return nil, &ErrInvalidConfig{Key: envDaemonAdmissionCredentials, Reason: "required in enforce mode"}
		}
		return &DaemonAdmission{mode: mode, credentials: map[string][]string{}}, nil
	}
	credentials, err := parseDaemonAdmissionCredentials(raw)
	if err != nil {
		return nil, &ErrInvalidConfig{Key: envDaemonAdmissionCredentials, Reason: ErrDaemonAdmissionCredentials.Error()}
	}
	return &DaemonAdmission{mode: mode, credentials: credentials}, nil
}

func (a *DaemonAdmission) decision(h http.Header) string {
	identities := h.Values("X-Pyrycode-Server")
	if len(identities) != 1 || identities[0] == "" || strings.Contains(identities[0], ",") {
		return "malformed"
	}
	tokens := h.Values("X-Pyrycode-Admission-Token")
	if len(tokens) == 0 {
		return "missing"
	}
	if len(tokens) != 1 || !validAdmissionToken(tokens[0]) {
		return "malformed"
	}
	// Neither absent identities nor empty arrays admit anyone. As specified in
	// the decision table, these are unknown, even if another identity owns token.
	if len(a.credentials[identities[0]]) == 0 {
		return "unknown"
	}
	valid, known := 0, 0
	for id, accepted := range a.credentials {
		for _, token := range accepted {
			match := subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(token))
			known |= match
			if id == identities[0] {
				valid |= match
			}
		}
	}
	if valid == 1 {
		return "valid"
	}
	if known == 1 {
		return "mismatch"
	}
	return "unknown"
}

// Wrap gates the existing daemon handler before it upgrades or changes registry
// state. It must remain inside the existing rate/source/global admission limits.
// Counters have only prebound aggregate outcomes, with no request-derived labels.
func (a *DaemonAdmission) Wrap(next http.Handler, reg prometheus.Registerer) http.Handler {
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pyrycode_relay_daemon_admission_total",
		Help: "Daemon admission decisions by fixed aggregate outcome. Off mode is unchecked.",
	}, []string{"outcome"})
	reg.MustRegister(vec)
	counters := make(map[string]prometheus.Counter, 5)
	for _, outcome := range []string{"valid", "missing", "malformed", "unknown", "mismatch"} {
		counters[outcome] = vec.WithLabelValues(outcome)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.mode != "off" {
			outcome := a.decision(r.Header)
			counters[outcome].Inc()
			if a.mode == "enforce" && outcome != "valid" {
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "daemon admission rejected", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
