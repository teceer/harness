package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	"github.com/teceer/harness/internal/config"
)

// VoIP pushes ring the phone through CallKit. Expo's push service cannot
// send them, so harness talks to APNs itself with the team's .p8 key
// (token auth). The phone registers its PushKit token and bundle id.

type voipDevice struct {
	Device  string    `json:"device,omitempty"`
	Bundle  string    `json:"bundle"`
	Sandbox bool      `json:"sandbox,omitempty"` // learnt from APNs: dev builds use the sandbox
	Added   time.Time `json:"added"`
}

type voipTokens struct {
	mu   gosync.Mutex
	path string
}

func newVoipTokens(cfg *config.Config) *voipTokens {
	return &voipTokens{path: filepath.Join(cfg.Home, "voip-tokens.json")}
}

func (v *voipTokens) load() map[string]voipDevice {
	m := map[string]voipDevice{}
	if b, err := os.ReadFile(v.path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (v *voipTokens) save(m map[string]voipDevice) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v.path)
}

func (v *voipTokens) all() map[string]voipDevice {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.load()
}

func (v *voipTokens) update(fn func(m map[string]voipDevice)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	m := v.load()
	fn(m)
	return v.save(m)
}

func validVoipToken(t string) bool {
	if len(t) < 32 || len(t) > 200 {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

func validBundle(b string) bool {
	return strings.HasPrefix(b, "com.teceer.harness") && len(b) < 80 && !strings.ContainsAny(b, " /")
}

func (s *webServer) voipRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token, Device, Bundle string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.Token = strings.ToLower(body.Token)
	if !validVoipToken(body.Token) || !validBundle(body.Bundle) {
		http.Error(w, "not a PushKit token / harness bundle id", http.StatusBadRequest)
		return
	}
	err := s.huddles.voip.update(func(m map[string]voipDevice) {
		m[body.Token] = voipDevice{Device: trim(body.Device, 80), Bundle: body.Bundle, Added: time.Now()}
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logAction(r, "voip register %s (%s)", trim(body.Device, 80), body.Bundle)
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *webServer) voipUnregister(w http.ResponseWriter, r *http.Request) {
	var body struct{ Token string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.huddles.voip.update(func(m map[string]voipDevice) { delete(m, strings.ToLower(body.Token)) })
	writeJSON(w, map[string]string{"ok": "1"})
}

// apnsClient signs requests with an ES256 JWT, renewed every 40 minutes
// (APNs refuses tokens older than an hour and ones renewed too often).
type apnsClient struct {
	keyID, teamID string
	key           *ecdsa.PrivateKey
	http          *http.Client

	mu     gosync.Mutex
	jwt    string
	issued time.Time
}

var (
	apnsProduction = "https://api.push.apple.com"
	apnsSandbox    = "https://api.sandbox.push.apple.com"
)

func newAPNs(h config.Huddle) (*apnsClient, error) {
	if h.APNsKey == "" || h.APNsKeyID == "" || h.APNsTeamID == "" {
		return nil, errors.New("[huddle] apns_key, apns_key_id and apns_team_id are not set in config.toml")
	}
	raw, err := os.ReadFile(h.APNsKey)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM key", h.APNsKey)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an EC key", h.APNsKey)
	}
	return &apnsClient{keyID: h.APNsKeyID, teamID: h.APNsTeamID, key: ec,
		http: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (a *apnsClient) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Since(a.issued) < 40*time.Minute {
		return a.jwt, nil
	}
	enc := base64.RawURLEncoding
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": a.keyID})
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iss": a.teamID, "iat": now.Unix()})
	unsigned := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, a.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	pad := func(b *big.Int, dst []byte) { bs := b.Bytes(); copy(dst[len(dst)-len(bs):], bs) }
	pad(r, sig[:32])
	pad(s, sig[32:])
	a.jwt, a.issued = unsigned+"."+enc.EncodeToString(sig), now
	return a.jwt, nil
}

// apnsError is APNs' reason ("BadDeviceToken", "Unregistered", …).
type apnsError struct {
	Status int
	Reason string
}

func (e *apnsError) Error() string { return fmt.Sprintf("apns %d %s", e.Status, e.Reason) }

func (a *apnsClient) send(base, token, topic string, payload []byte) error {
	jwt, err := a.token()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/3/device/"+token, bytes.NewReader(payload))
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", topic)
	req.Header.Set("apns-push-type", "voip")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("apns-expiration", "0") // a call that cannot ring now is not worth ringing later
	res, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	var out struct{ Reason string }
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	json.Unmarshal(b, &out)
	return &apnsError{Status: res.StatusCode, Reason: out.Reason}
}

// ring sends the call to every registered phone. A token APNs production
// does not know is retried on the sandbox (development builds) and the
// environment remembered; tokens APNs reports as gone are dropped.
func (m *huddleManager) ring(h *huddle) error {
	if m.apns == nil {
		c, err := newAPNs(m.cfg.Huddle)
		if err != nil {
			return err
		}
		m.apns = c
	}
	devices := m.voip.all()
	if len(devices) == 0 {
		return errors.New("no phone registered for calls: open the Harness app (signed in) first")
	}
	payload, _ := json.Marshal(map[string]string{"huddleId": h.ID, "caller": h.Caller, "topic": h.Topic})
	var sent int
	var lastErr error
	for tok, d := range devices {
		bases := []string{apnsProduction, apnsSandbox}
		if d.Sandbox {
			bases = []string{apnsSandbox, apnsProduction}
		}
		for i, base := range bases {
			err := m.apns.send(base, tok, d.Bundle+".voip", payload)
			if err == nil {
				sent++
				if sandbox := base == apnsSandbox; sandbox != d.Sandbox {
					m.voip.update(func(all map[string]voipDevice) {
						if x, ok := all[tok]; ok {
							x.Sandbox = sandbox
							all[tok] = x
						}
					})
				}
				break
			}
			lastErr = err
			var ae *apnsError
			if errors.As(err, &ae) && ae.Reason == "BadDeviceToken" && i == 0 {
				continue // the other environment
			}
			if errors.As(err, &ae) && (ae.Status == http.StatusGone || ae.Reason == "BadDeviceToken" || ae.Reason == "DeviceTokenNotForTopic") {
				m.voip.update(func(all map[string]voipDevice) { delete(all, tok) })
			}
			break
		}
	}
	if sent == 0 {
		return fmt.Errorf("could not ring the phone: %v", lastErr)
	}
	return nil
}
