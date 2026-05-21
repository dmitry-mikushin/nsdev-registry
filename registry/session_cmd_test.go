package registry

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperatorSourceIP_FromSSHClient(t *testing.T) {
	t.Setenv("SSH_CLIENT", "192.0.2.7 51234 22")
	got, err := operatorSourceIP()
	if err != nil {
		t.Fatalf("operatorSourceIP: %v", err)
	}
	if got != "192.0.2.7" {
		t.Errorf("got %q, want 192.0.2.7", got)
	}
}

func TestOperatorSourceIP_FallsBackToLoopback(t *testing.T) {
	t.Setenv("SSH_CLIENT", "")
	got, err := operatorSourceIP()
	if err != nil {
		t.Fatalf("operatorSourceIP: %v", err)
	}
	if got != "127.0.0.1" {
		t.Errorf("got %q, want 127.0.0.1 fallback", got)
	}
}

func TestOperatorSourceIP_MalformedRejected(t *testing.T) {
	t.Setenv("SSH_CLIENT", "   ") // whitespace-only
	if _, err := operatorSourceIP(); err == nil {
		t.Fatal("must reject malformed SSH_CLIENT")
	}
}

// runSessionCmd invokes the SessionCmd RunE against a fresh cobra command
// state: resets the package-global flags so independent test cases don't
// see each other's values, then captures the command's stdout in a
// bytes.Buffer.
func runSessionCmd(t *testing.T, server, user string, probePort int) (string, error) {
	t.Helper()

	// Snapshot + restore the package-level flag vars so the test is
	// order-independent. (Cobra reads them via pointer.)
	prevServer, prevPort, prevUser, prevInsecure :=
		sessionServerURL, sessionProbePort, sessionUserFlag, sessionInsecure
	t.Cleanup(func() {
		sessionServerURL = prevServer
		sessionProbePort = prevPort
		sessionUserFlag = prevUser
		sessionInsecure = prevInsecure
	})

	sessionServerURL = server
	sessionProbePort = probePort
	sessionUserFlag = user
	sessionInsecure = true

	var out bytes.Buffer
	SessionCmd.SetOut(&out)
	SessionCmd.SetErr(io.Discard)
	err := SessionCmd.RunE(SessionCmd, nil)
	return out.String(), err
}

func TestSessionCmd_PostsExpectedBodyAndPipesResponse(t *testing.T) {
	t.Setenv("SSH_CLIENT", "192.0.2.99 65000 22")

	var got struct {
		User          string `json:"user"`
		ClientUDPAddr string `json:"client_udp_addr"`
	}
	var gotCT string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v3/sessions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("server got bad JSON: %v / %s", err, string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"deadbeef","probes_sent":10}`)
	}))
	defer srv.Close()

	stdout, err := runSessionCmd(t, srv.URL, "alice", 54321)
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if gotCT != "application/json" {
		t.Errorf("Content-Type sent = %q, want application/json", gotCT)
	}
	if got.User != "alice" {
		t.Errorf("posted user=%q, want alice", got.User)
	}
	if got.ClientUDPAddr != "192.0.2.99:54321" {
		t.Errorf("posted client_udp_addr=%q, want 192.0.2.99:54321",
			got.ClientUDPAddr)
	}
	// Subcommand pipes the response body verbatim to stdout so that
	// nsdev-push can parse it byte-identical from the ssh stdout pipe.
	if !strings.Contains(stdout, `"token":"deadbeef"`) {
		t.Errorf("stdout = %q, want server response piped through",
			stdout)
	}
}

func TestSessionCmd_FallbackUserFromUSEREnv(t *testing.T) {
	t.Setenv("SSH_USER", "")
	t.Setenv("USER", "fallback-user")
	t.Setenv("SSH_CLIENT", "")

	var got struct {
		User string `json:"user"`
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	if _, err := runSessionCmd(t, srv.URL, "", 12345); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if got.User != "fallback-user" {
		t.Errorf("user=%q, want fallback-user", got.User)
	}
}

func TestSessionCmd_RejectsBadProbePort(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be called when probe-port is invalid")
	}))
	defer srv.Close()

	for _, port := range []int{0, -1, 70000} {
		_, err := runSessionCmd(t, srv.URL, "u", port)
		if err == nil {
			t.Errorf("port=%d: expected error, got nil", port)
		}
	}
}

func TestSessionCmd_RequiresIdentifiableUser(t *testing.T) {
	t.Setenv("SSH_USER", "")
	t.Setenv("USER", "")
	t.Setenv("SSH_CLIENT", "")
	_, err := runSessionCmd(t, "https://127.0.0.1:1", "", 1234)
	if err == nil {
		t.Fatal("must fail when neither --user nor $SSH_USER/$USER is set")
	}
}

func TestSessionCmd_NonOKStatusPropagated(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "registry says no", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := runSessionCmd(t, srv.URL, "u", 1234)
	if err == nil {
		t.Fatal("must propagate non-2xx as error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should mention status code", err)
	}
}
