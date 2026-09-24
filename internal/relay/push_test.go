package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testFCMDeviceToken = "device-token-SECRET-7f3a"
	testFCMAccessToken = "access-token-SECRET-91c2"
	testFCMClientEmail = "relay-push@pyrycode-mobile.iam.gserviceaccount.com"
	testFCMMarker      = "MARKER-must-not-leak-5d0e"
)

// testRSAKeyPEM is generated once per test binary; key material is never
// committed.
var testRSAKeyPEM = sync.OnceValue(func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
})

// testServiceAccountJSON builds a service-account key whose token_uri points
// at tokenURI.
func testServiceAccountJSON(t *testing.T, tokenURI string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "pyrycode-mobile",
		"private_key_id": "kid-1",
		"private_key":    testRSAKeyPEM(),
		"client_email":   testFCMClientEmail,
		"token_uri":      tokenURI,
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}
	return raw
}

// fakeOAuth serves an access token, or failStatus with a marker body when
// failStatus is non-zero. hits counts token requests.
func fakeOAuth(t *testing.T, failStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failStatus != 0 {
			w.WriteHeader(failStatus)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"`+testFCMMarker+` `+testFCMAccessToken+`"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`+testFCMAccessToken+`","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

type fcmRequest struct {
	method, path, auth, contentType string
	body                            []byte
}

// fakeFCM records each request and replies with status. Its error body
// echoes the marker, the device token and the bearer header.
func fakeFCM(t *testing.T, status int) (*httptest.Server, func() []fcmRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []fcmRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, fcmRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
		mu.Unlock()
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, testFCMMarker+" "+testFCMDeviceToken+" "+r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []fcmRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]fcmRequest(nil), reqs...)
	}
}

// assertNoSecrets fails when msg contains any credential, token or marker.
func assertNoSecrets(t *testing.T, msg string) {
	t.Helper()
	for _, s := range []string{testFCMDeviceToken, testFCMAccessToken, testFCMClientEmail, testFCMMarker, "PRIVATE KEY", "invalid_grant"} {
		if strings.Contains(msg, s) {
			t.Errorf("error %q contains secret or upstream body %q", msg, s)
		}
	}
}

func TestFCMSender_SendRequestShape(t *testing.T) {
	t.Parallel()
	oauth, _ := fakeOAuth(t, 0)
	fcm, reqs := fakeFCM(t, http.StatusOK)

	s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
	if err != nil {
		t.Fatalf("newFCMSender: %v", err)
	}
	if err := s.Send(context.Background(), testFCMDeviceToken); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := reqs()
	if len(got) != 1 {
		t.Fatalf("FCM requests = %d, want 1", len(got))
	}
	r := got[0]
	if r.method != http.MethodPost {
		t.Errorf("method = %q, want POST", r.method)
	}
	if want := "/v1/projects/pyrycode-mobile/messages:send"; r.path != want {
		t.Errorf("path = %q, want %q", r.path, want)
	}
	if want := "Bearer " + testFCMAccessToken; r.auth != want {
		t.Errorf("Authorization = %q, want %q", r.auth, want)
	}
	if r.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", r.contentType)
	}

	var body map[string]map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("decode body %s: %v", r.body, err)
	}
	if len(body) != 1 {
		t.Errorf("top-level keys = %v, want only message", body)
	}
	msg := body["message"]
	if len(msg) != 2 {
		t.Errorf("message keys = %v, want exactly token and android (no notification, no data)", msg)
	}
	var token string
	if err := json.Unmarshal(msg["token"], &token); err != nil || token != testFCMDeviceToken {
		t.Errorf("message.token = %s (err %v), want %q", msg["token"], err, testFCMDeviceToken)
	}
	var android map[string]string
	if err := json.Unmarshal(msg["android"], &android); err != nil {
		t.Fatalf("decode android %s: %v", msg["android"], err)
	}
	if len(android) != 1 || android["priority"] != "HIGH" {
		t.Errorf("message.android = %v, want exactly {priority: HIGH}", android)
	}
}

func TestNewFCMSender_TargetsProductionEndpoint(t *testing.T) {
	t.Parallel()
	s, err := NewFCMSender(testServiceAccountJSON(t, "https://oauth2.googleapis.com/token"))
	if err != nil {
		t.Fatalf("NewFCMSender: %v", err)
	}
	if want := "https://fcm.googleapis.com/v1/projects/pyrycode-mobile/messages:send"; s.sendURL != want {
		t.Errorf("sendURL = %q, want %q", s.sendURL, want)
	}
	if s.client.Timeout != fcmSendTimeout {
		t.Errorf("client timeout = %v, want fcmSendTimeout %v", s.client.Timeout, fcmSendTimeout)
	}
}

func TestFCMSender_ReusesAccessToken(t *testing.T) {
	t.Parallel()
	oauth, tokenHits := fakeOAuth(t, 0)
	fcm, reqs := fakeFCM(t, http.StatusOK)

	s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
	if err != nil {
		t.Fatalf("newFCMSender: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Send(context.Background(), testFCMDeviceToken); err != nil {
			t.Fatalf("Send #%d: %v", i+1, err)
		}
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}
	if n := len(reqs()); n != 2 {
		t.Errorf("FCM requests = %d, want 2", n)
	}
}

func TestFCMSender_NonSuccessReplyNamesOnlyStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			oauth, _ := fakeOAuth(t, 0)
			fcm, reqs := fakeFCM(t, status)

			s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
			if err != nil {
				t.Fatalf("newFCMSender: %v", err)
			}
			err = s.Send(context.Background(), testFCMDeviceToken)
			if !errors.Is(err, ErrFCMSend) {
				t.Fatalf("Send err = %v, want ErrFCMSend", err)
			}
			if want := "status " + strconv.Itoa(status); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
			assertNoSecrets(t, err.Error())
			if n := len(reqs()); n != 1 {
				t.Errorf("FCM requests = %d, want 1 (no redirect follow)", n)
			}
		})
	}
}

func TestFCMSender_TokenFetchFailureCarriesNoBody(t *testing.T) {
	t.Parallel()
	oauth, _ := fakeOAuth(t, http.StatusBadRequest)
	fcm, reqs := fakeFCM(t, http.StatusOK)

	s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
	if err != nil {
		t.Fatalf("newFCMSender: %v", err)
	}
	err = s.Send(context.Background(), testFCMDeviceToken)
	if !errors.Is(err, ErrFCMTokenFetch) {
		t.Fatalf("Send err = %v, want ErrFCMTokenFetch", err)
	}
	assertNoSecrets(t, err.Error())
	if n := len(reqs()); n != 0 {
		t.Errorf("FCM requests = %d, want 0 after a failed token fetch", n)
	}
}

func TestFCMSender_CancelledContext(t *testing.T) {
	t.Parallel()
	oauth, _ := fakeOAuth(t, 0)
	fcm, _ := fakeFCM(t, http.StatusOK)

	s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
	if err != nil {
		t.Fatalf("newFCMSender: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.Send(ctx, testFCMDeviceToken)
	if !errors.Is(err, ErrFCMSend) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Send err = %v, want ErrFCMSend wrapping context.Canceled", err)
	}
	assertNoSecrets(t, err.Error())
}

func TestParseFCMCredentials_Rejects(t *testing.T) {
	t.Parallel()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshal ecdsa key: %v", err)
	}
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))

	key := func(overrides map[string]string) string {
		fields := map[string]string{
			"type":         "service_account",
			"private_key":  testRSAKeyPEM(),
			"client_email": testFCMClientEmail,
		}
		for k, v := range overrides {
			if v == "" {
				delete(fields, k)
				continue
			}
			fields[k] = v
		}
		raw, _ := json.Marshal(fields)
		return string(raw)
	}

	cases := map[string]string{
		"not JSON":             testFCMMarker + " {",
		"wrong type":           key(map[string]string{"type": testFCMMarker}),
		"missing client_email": key(map[string]string{"client_email": ""}),
		"missing private_key":  key(map[string]string{"private_key": ""}),
		"garbage private_key":  key(map[string]string{"private_key": "-----BEGIN PRIVATE KEY-----\n" + testFCMMarker + "\n-----END PRIVATE KEY-----\n"}),
		"non-RSA private_key":  key(map[string]string{"private_key": ecPEM}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseFCMCredentials([]byte(raw))
			if !errors.Is(err, ErrFCMCredentials) {
				t.Fatalf("err = %v, want ErrFCMCredentials", err)
			}
			assertNoSecrets(t, err.Error())
			if _, err := NewFCMSender([]byte(raw)); !errors.Is(err, ErrFCMCredentials) {
				t.Errorf("NewFCMSender err = %v, want ErrFCMCredentials", err)
			}
		})
	}
}

func TestParseFCMCredentials_DefaultsTokenURL(t *testing.T) {
	t.Parallel()
	cfg, err := parseFCMCredentials(testServiceAccountJSON(t, ""))
	if err != nil {
		t.Fatalf("parseFCMCredentials: %v", err)
	}
	if cfg.TokenURL != fcmDefaultTokenURL {
		t.Errorf("TokenURL = %q, want %q", cfg.TokenURL, fcmDefaultTokenURL)
	}
	if len(cfg.Scopes) != 1 || cfg.Scopes[0] != fcmScope {
		t.Errorf("Scopes = %v, want [%s]", cfg.Scopes, fcmScope)
	}
}
