package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/internal/session"
)

// mockPuncher satisfies QuicPuncher with assertion-friendly fields. Tests
// can pre-load PunchErr to simulate write failures; PunchCalls records
// every Punch invocation for ordering / arg verification.
type mockPuncher struct {
	mu             sync.Mutex
	Advertised     string
	PunchErr       error
	PunchCalls     []punchCall
}

type punchCall struct {
	Dst     string
	Count   int
	Spacing time.Duration
}

func (m *mockPuncher) AdvertisedAddr() string { return m.Advertised }

func (m *mockPuncher) Punch(_ context.Context, dst string, count int, spacing time.Duration) error {
	m.mu.Lock()
	m.PunchCalls = append(m.PunchCalls, punchCall{dst, count, spacing})
	err := m.PunchErr
	m.mu.Unlock()
	return err
}

func newHandler() (*SessionHandler, *session.Store, *mockPuncher) {
	store := session.NewStore()
	puncher := &mockPuncher{Advertised: "registry.local:5000"}
	return NewSessionHandler(store, puncher), store, puncher
}

func postJSON(t *testing.T, h http.Handler, body string, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v3/sessions",
		strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		r.RemoteAddr = remoteAddr
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSession_GETReturns405(t *testing.T) {
	h, _, _ := newHandler()
	r := httptest.NewRequest(http.MethodGet, "/v3/sessions", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow header = %q, want %q", got, http.MethodPost)
	}
}

func TestSession_RejectsNonLoopbackCaller(t *testing.T) {
	h, _, puncher := newHandler()
	w := postJSON(t, h,
		`{"user":"a","client_udp_addr":"10.0.0.1:1"}`,
		"203.0.113.1:33000")
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d, want 403", w.Code)
	}
	if len(puncher.PunchCalls) != 0 {
		t.Errorf("must not punch when caller is rejected")
	}
}

func TestSession_AcceptsIPv4AndIPv6Loopback(t *testing.T) {
	for _, ra := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		t.Run(ra, func(t *testing.T) {
			h, _, _ := newHandler()
			w := postJSON(t, h,
				`{"user":"u","client_udp_addr":"127.0.0.1:1"}`, ra)
			if w.Code != http.StatusOK {
				t.Errorf("status=%d for RemoteAddr=%q, want 200",
					w.Code, ra)
			}
		})
	}
}

func TestSession_BadJSONReturns400(t *testing.T) {
	h, _, _ := newHandler()
	w := postJSON(t, h, `{not json`, "127.0.0.1:1")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestSession_MissingFieldsReturn400(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty user", `{"user":"","client_udp_addr":"127.0.0.1:1"}`},
		{"missing user", `{"client_udp_addr":"127.0.0.1:1"}`},
		{"empty addr", `{"user":"u","client_udp_addr":""}`},
		{"missing addr", `{"user":"u"}`},
		{"malformed addr", `{"user":"u","client_udp_addr":"not-an-addr"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := newHandler()
			w := postJSON(t, h, c.body, "127.0.0.1:1")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status=%d, want 400", w.Code)
			}
		})
	}
}

func TestSession_HappyPathIssuesTokenAndPunches(t *testing.T) {
	h, store, puncher := newHandler()

	w := postJSON(t, h,
		`{"user":"alice","client_udp_addr":"10.0.0.1:54321"}`,
		"127.0.0.1:1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}

	var resp SessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Response shape
	if resp.Token == "" {
		t.Error("response Token is empty")
	}
	if resp.IssuedToUser != "alice" {
		t.Errorf("IssuedToUser=%q, want alice", resp.IssuedToUser)
	}
	if resp.IssuedToClient != "10.0.0.1:54321" {
		t.Errorf("IssuedToClient=%q", resp.IssuedToClient)
	}
	if resp.ServerUDPAddr != puncher.Advertised {
		t.Errorf("ServerUDPAddr=%q, want %q",
			resp.ServerUDPAddr, puncher.Advertised)
	}
	if resp.ExpiresAt.Before(time.Now()) {
		t.Errorf("ExpiresAt=%s already in the past", resp.ExpiresAt)
	}
	if resp.ProbesSent != h.ProbeCount {
		t.Errorf("ProbesSent=%d, want %d", resp.ProbesSent, h.ProbeCount)
	}

	// Side effects: token must verify, puncher must have been hit once
	// at the operator's UDP endpoint.
	if _, ok := store.Verify(session.Token(resp.Token)); !ok {
		t.Error("returned token does not verify against the store")
	}
	if len(puncher.PunchCalls) != 1 {
		t.Fatalf("Punch called %d times, want 1", len(puncher.PunchCalls))
	}
	pc := puncher.PunchCalls[0]
	if pc.Dst != "10.0.0.1:54321" {
		t.Errorf("Punch dst=%q, want 10.0.0.1:54321", pc.Dst)
	}
	if pc.Count != h.ProbeCount {
		t.Errorf("Punch count=%d, want %d", pc.Count, h.ProbeCount)
	}
	if pc.Spacing != h.ProbeStep {
		t.Errorf("Punch spacing=%s, want %s", pc.Spacing, h.ProbeStep)
	}
}

func TestSession_PunchFailureSetsProbesSentZero(t *testing.T) {
	h, _, puncher := newHandler()
	puncher.PunchErr = errors.New("simulated write failure")

	w := postJSON(t, h,
		`{"user":"u","client_udp_addr":"10.0.0.1:1"}`,
		"127.0.0.1:1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	var resp SessionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" {
		t.Error("token must still be issued even when punch fails")
	}
	if resp.ProbesSent != 0 {
		t.Errorf("ProbesSent=%d, want 0 on punch failure", resp.ProbesSent)
	}
}

func TestSession_RequestedTTLClampedToMax(t *testing.T) {
	h, store, _ := newHandler()
	h.MaxTTL = 5 * time.Minute

	body, _ := json.Marshal(SessionRequest{
		User:          "u",
		ClientUDPAddr: "10.0.0.1:1",
		RequestedTTL:  24 * time.Hour, // way above MaxTTL
	})
	w := postJSON(t, h, string(body), "127.0.0.1:1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp SessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	entry, ok := store.Verify(session.Token(resp.Token))
	if !ok {
		t.Fatal("token must verify")
	}
	got := entry.ExpiresAt.Sub(entry.IssuedAt)
	if got > h.MaxTTL+time.Second {
		t.Errorf("TTL=%s, want clamped to <= MaxTTL=%s", got, h.MaxTTL)
	}
}

func TestSession_ResponseContentType(t *testing.T) {
	h, _, _ := newHandler()
	w := postJSON(t, h,
		`{"user":"u","client_udp_addr":"127.0.0.1:1"}`, "127.0.0.1:1")
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", got)
	}
	// Sanity: body parses as JSON.
	var v map[string]any
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&v); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
}
