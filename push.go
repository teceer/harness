package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	"github.com/teceer/harness/internal/store"
)

// Push notifications for the native app (apps/harness-mobile in core11).
// The app hands us its Expo push token; notifications then go through the
// Expo Push Service, which talks to APNs. The Mac only needs outbound
// HTTPS for that, so nothing new is exposed.

// expoPushURL is a variable so tests can point it at a fake service.
var expoPushURL = "https://exp.host/--/api/v2/push/send"

type pushDevice struct {
	Device string    `json:"device,omitempty"`
	Added  time.Time `json:"added"`
}

// pushTokens is the set of registered devices, kept in ~/.harness/push-tokens.json.
type pushTokens struct {
	mu   gosync.Mutex
	path string
}

func (p *pushTokens) load() map[string]pushDevice {
	m := map[string]pushDevice{}
	if b, err := os.ReadFile(p.path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (p *pushTokens) save(m map[string]pushDevice) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

func (p *pushTokens) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for t := range p.load() {
		out = append(out, t)
	}
	return out
}

func (p *pushTokens) add(token, device string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.load()
	m[token] = pushDevice{Device: device, Added: time.Now()}
	return p.save(m)
}

func (p *pushTokens) remove(token string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.load()
	if _, ok := m[token]; !ok {
		return nil
	}
	delete(m, token)
	return p.save(m)
}

func (s *webServer) pushStore() *pushTokens {
	s.pushOnce.Do(func() { s.push = &pushTokens{path: filepath.Join(s.cfg.Home, "push-tokens.json")} })
	return s.push
}

// validPushToken accepts Expo tokens only: anything else would be sent to
// Expo on every notification for nothing.
func validPushToken(t string) bool {
	return (strings.HasPrefix(t, "ExponentPushToken[") || strings.HasPrefix(t, "ExpoPushToken[")) &&
		strings.HasSuffix(t, "]") && len(t) < 200
}

func (s *webServer) pushRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  string `json:"token"`
		Device string `json:"device"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validPushToken(body.Token) {
		http.Error(w, "not an Expo push token", http.StatusBadRequest)
		return
	}
	if err := s.pushStore().add(body.Token, trim(body.Device, 80)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logAction(r, "push register %s", trim(body.Device, 80))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *webServer) pushUnregister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.pushStore().remove(body.Token); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logAction(r, "push unregister")
	writeJSON(w, map[string]string{"ok": "1"})
}

type expoMessage struct {
	To         string            `json:"to"`
	Title      string            `json:"title"`
	Body       string            `json:"body,omitempty"`
	Sound      string            `json:"sound"`
	Priority   string            `json:"priority"`
	CategoryID string            `json:"categoryId,omitempty"`
	ThreadID   string            `json:"threadId,omitempty"` // groups a session's notifications
	Badge      *int              `json:"badge,omitempty"`    // unread sessions on the app icon
	Data       map[string]string `json:"data"`
}

// sendPush delivers a session event to every registered device (errors
// are only logged: the watcher keeps going).
func (s *webServer) sendPush(se store.Session, title, body string) {
	if err := s.deliverPush(se, title, body); err != nil && !errors.Is(err, errNoDevices) {
		log.Printf("push: %v", err)
	}
}

var errNoDevices = errors.New("no device registered for push")

// deliverPush sends one notification to every registered device and drops
// the ones Expo reports as gone (app deleted, push turned off). se may be
// empty (a message from an agent outside harness): no link then.
func (s *webServer) deliverPush(se store.Session, title, body string) error {
	tokens := s.pushStore().list()
	if len(tokens) == 0 {
		return errNoDevices
	}
	category := ""
	if se.Status == store.Waiting && se.TmuxPane != "" {
		category = "waiting" // the app offers Enter / 1 / 2 / Esc on it
	}
	data := map[string]string{"status": se.Status}
	thread := "harness"
	if se.ID != "" {
		data["sessionId"], thread = se.ID, se.ID
	}
	badge := s.unreadCount()
	msgs := make([]expoMessage, len(tokens))
	for i, t := range tokens {
		msgs[i] = expoMessage{To: t, Title: title, Body: trim(body, 300), Sound: "default", Priority: "high",
			CategoryID: category, ThreadID: thread, Badge: &badge, Data: data}
	}
	payload, _ := json.Marshal(msgs)
	req, _ := http.NewRequest(http.MethodPost, expoPushURL, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var out struct {
		Data []struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details struct {
				Error string `json:"error"`
			} `json:"details"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != http.StatusOK {
		return fmt.Errorf("expo answered %s", res.Status)
	}
	// Tickets come back in the order of the messages.
	failed := 0
	for i, t := range out.Data {
		if t.Status != "error" || i >= len(tokens) {
			continue
		}
		failed++
		log.Printf("push: %s", cmp(t.Message, t.Details.Error))
		if t.Details.Error == "DeviceNotRegistered" {
			s.pushStore().remove(tokens[i])
		}
	}
	if failed == len(tokens) {
		return errors.New("no device accepted the notification")
	}
	return nil
}

func pushSummary(n int) string {
	if n == 1 {
		return "1 device"
	}
	return fmt.Sprintf("%d devices", n)
}
