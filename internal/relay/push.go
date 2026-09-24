package relay

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// envFCMCredentials holds the whole Google service-account JSON key the relay
// uses to send FCM wake messages. Unset means push is off.
const envFCMCredentials = "PYRYCODE_RELAY_FCM_CREDENTIALS" // #nosec G101 -- env var name, not a credential

const (
	// fcmProjectID is the Firebase project the Android app registers with.
	fcmProjectID = "pyrycode-mobile"
	// fcmDefaultBaseURL is the FCM HTTP v1 API origin.
	fcmDefaultBaseURL = "https://fcm.googleapis.com"
	// fcmScope is the OAuth scope FCM HTTP v1 requires.
	fcmScope = "https://www.googleapis.com/auth/firebase.messaging"
	// fcmDefaultTokenURL is used when the key carries no token_uri, matching
	// golang.org/x/oauth2/google's JWTTokenURL.
	fcmDefaultTokenURL = "https://oauth2.googleapis.com/token"
	// fcmSendTimeout bounds each HTTP call a send makes: the access-token
	// fetch and the FCM POST.
	fcmSendTimeout = 10 * time.Second
	// fcmMaxDrainBytes caps how much of an FCM reply is drained before close.
	fcmMaxDrainBytes = 4 << 10
)

var (
	// ErrFCMCredentials is returned when the credential JSON is not a usable
	// service-account key. It never carries any part of the value.
	ErrFCMCredentials = errors.New("relay: fcm credentials are not a service-account JSON key")
	// ErrFCMTokenFetch is returned when minting the OAuth access token fails.
	// It carries at most the token endpoint's HTTP status, never its body.
	ErrFCMTokenFetch = errors.New("relay: fcm access-token fetch failed")
	// ErrFCMSend is returned when the FCM request fails in transport or
	// with a non-2xx reply. It carries at most the HTTP status, never a body.
	ErrFCMSend = errors.New("relay: fcm send failed")
)

// FCMSender sends data-only, high-priority FCM messages that wake the
// Android app. It holds no logger and is safe for concurrent use.
type FCMSender struct {
	client  *http.Client
	tokens  oauth2.TokenSource
	sendURL string
}

// NewFCMSender builds a sender for the production FCM endpoint from a
// service-account JSON key.
func NewFCMSender(credentialsJSON []byte) (*FCMSender, error) {
	return newFCMSender(credentialsJSON, fcmDefaultBaseURL)
}

func newFCMSender(credentialsJSON []byte, baseURL string) (*FCMSender, error) {
	cfg, err := parseFCMCredentials(credentialsJSON)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: fcmSendTimeout,
		// A redirect would replay the bearer token and device token to a
		// second URL; surface it as a non-2xx reply instead.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// jwt.Config.TokenSource caches the token until shortly before expiry
	// and fetches it with the client stored under oauth2.HTTPClient.
	tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient, client)
	return &FCMSender{
		client:  client,
		tokens:  cfg.TokenSource(tokenCtx),
		sendURL: baseURL + "/v1/projects/" + fcmProjectID + "/messages:send",
	}, nil
}

// parseFCMCredentials maps a service-account JSON key onto a jwt.Config. It
// checks the private key parses so a bad key fails at boot rather than on
// the first send. Every failure returns bare ErrFCMCredentials: json and
// x509 errors can echo fragments of the input.
func parseFCMCredentials(raw []byte) (*jwt.Config, error) {
	var key struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &key); err != nil {
		return nil, ErrFCMCredentials
	}
	if key.Type != "service_account" || key.ClientEmail == "" || !isRSAPrivateKey([]byte(key.PrivateKey)) {
		return nil, ErrFCMCredentials
	}
	tokenURL := key.TokenURI
	if tokenURL == "" {
		tokenURL = fcmDefaultTokenURL
	}
	return &jwt.Config{
		Email:        key.ClientEmail,
		PrivateKey:   []byte(key.PrivateKey),
		PrivateKeyID: key.PrivateKeyID,
		Scopes:       []string{fcmScope},
		TokenURL:     tokenURL,
	}, nil
}

// isRSAPrivateKey reports whether key parses the way golang.org/x/oauth2's
// jwt signer parses it: optional PEM, then PKCS#8, then PKCS#1, RSA only.
func isRSAPrivateKey(key []byte) bool {
	if block, _ := pem.Decode(key); block != nil {
		key = block.Bytes
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(key); err == nil {
		_, ok := parsed.(*rsa.PrivateKey)
		return ok
	}
	_, err := x509.ParsePKCS1PrivateKey(key)
	return err == nil
}

type fcmSendRequest struct {
	Message fcmMessage `json:"message"`
}

// fcmMessage deliberately has no notification or data field: the app
// ignores every field, so nothing from a session reaches Google.
type fcmMessage struct {
	Token   string     `json:"token"`
	Android fcmAndroid `json:"android"`
}

type fcmAndroid struct {
	Priority string `json:"priority"`
}

// Send wakes the device holding deviceToken. Returned errors never contain
// the device token, the access token, the credentials or a reply body.
func (s *FCMSender) Send(ctx context.Context, deviceToken string) error {
	tok, err := s.tokens.Token()
	if err != nil {
		// oauth2.RetrieveError formats the token endpoint's body into its
		// Error(); keep only the status.
		var rErr *oauth2.RetrieveError
		if errors.As(err, &rErr) && rErr.Response != nil {
			return fmt.Errorf("%w: status %d", ErrFCMTokenFetch, rErr.Response.StatusCode)
		}
		return ErrFCMTokenFetch
	}

	body, err := json.Marshal(fcmSendRequest{Message: fcmMessage{
		Token:   deviceToken,
		Android: fcmAndroid{Priority: "HIGH"},
	}})
	if err != nil {
		return fmt.Errorf("%w: encoding request: %w", ErrFCMSend, err)
	}

	ctx, cancel := context.WithTimeout(ctx, fcmSendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.sendURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: building request: %w", ErrFCMSend, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		// *url.Error carries only sendURL and the transport error; the
		// device token travels in the body.
		return fmt.Errorf("%w: %w", ErrFCMSend, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, fcmMaxDrainBytes))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: status %d", ErrFCMSend, resp.StatusCode)
	}
	return nil
}
