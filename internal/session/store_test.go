package session

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIssueAndVerifyRoundtrip(t *testing.T) {
	s := NewStore()

	tok, entry, err := s.Issue("alice", "10.0.0.1:54321", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("Issue returned empty token")
	}
	if len(tok) != 64 {
		t.Errorf("token len = %d, want 64 hex chars from 32 random bytes", len(tok))
	}
	if entry.User != "alice" {
		t.Errorf("Entry.User = %q, want %q", entry.User, "alice")
	}
	if entry.ClientAddr != "10.0.0.1:54321" {
		t.Errorf("Entry.ClientAddr = %q", entry.ClientAddr)
	}
	if !entry.ExpiresAt.After(entry.IssuedAt) {
		t.Errorf("ExpiresAt %s must be after IssuedAt %s",
			entry.ExpiresAt, entry.IssuedAt)
	}

	got, ok := s.Verify(tok)
	if !ok {
		t.Fatal("Verify returned !ok for a token we just issued")
	}
	if got.User != entry.User || got.ClientAddr != entry.ClientAddr {
		t.Errorf("Verify returned mismatched entry: got=%+v want=%+v",
			got, entry)
	}
}

func TestIssueRejectsEmptyUser(t *testing.T) {
	s := NewStore()
	_, _, err := s.Issue("", "10.0.0.1:5", time.Minute)
	if err == nil {
		t.Fatal("Issue with empty user should fail")
	}
	if !strings.Contains(err.Error(), "user") {
		t.Errorf("error message %q should mention 'user'", err)
	}
}

func TestIssueAppliesDefaultTTL(t *testing.T) {
	s := NewStore()
	_, entry, err := s.Issue("u", "1:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := entry.IssuedAt.Add(DefaultTTL)
	delta := entry.ExpiresAt.Sub(want)
	if delta < -time.Second || delta > time.Second {
		t.Errorf("ExpiresAt-IssuedAt = %s, want ~%s (default TTL)",
			entry.ExpiresAt.Sub(entry.IssuedAt), DefaultTTL)
	}
}

func TestVerifyRejectsUnknownToken(t *testing.T) {
	s := NewStore()
	if _, ok := s.Verify("not-a-token"); ok {
		t.Fatal("Verify of unknown token must return !ok")
	}
}

func TestVerifyDropsExpiredEntryLazily(t *testing.T) {
	s := NewStore()
	tok, _, err := s.Issue("u", "1:1", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	// Sleep just enough to guarantee the timer crossed the boundary;
	// 10ms is far in excess of the 1ns TTL but small enough to keep
	// the test fast.
	time.Sleep(10 * time.Millisecond)

	if _, ok := s.Verify(tok); ok {
		t.Fatal("Verify must reject an expired token")
	}
	if s.Len() != 0 {
		t.Errorf("expired token must be removed from the store on Verify; "+
			"Len=%d, want 0", s.Len())
	}
}

func TestReapDropsExpiredEntries(t *testing.T) {
	s := NewStore()
	tokLive, _, _ := s.Issue("live", "1:1", time.Minute)
	_, _, _ = s.Issue("dead1", "1:1", time.Nanosecond)
	_, _, _ = s.Issue("dead2", "1:1", time.Nanosecond)

	time.Sleep(10 * time.Millisecond)

	if got := s.Reap(); got != 2 {
		t.Errorf("Reap dropped %d entries, want 2", got)
	}
	if s.Len() != 1 {
		t.Errorf("after Reap Len=%d, want 1", s.Len())
	}
	if _, ok := s.Verify(tokLive); !ok {
		t.Fatal("Reap must not drop the live token")
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	s := NewStore()
	tok, _, _ := s.Issue("u", "1:1", time.Minute)
	s.Revoke(tok)
	s.Revoke(tok) // second call should not panic
	if _, ok := s.Verify(tok); ok {
		t.Fatal("revoked token must not verify")
	}
}

func TestTokensAreUnique(t *testing.T) {
	s := NewStore()
	seen := make(map[Token]bool, 1024)
	for i := 0; i < 1024; i++ {
		tok, _, err := s.Issue("u", "1:1", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token issued after %d iterations: %s",
				i, tok)
		}
		seen[tok] = true
	}
}

// TestConcurrentIssueAndVerify exercises the mutex by hammering Issue and
// Verify from multiple goroutines. -race must stay quiet.
func TestConcurrentIssueAndVerify(t *testing.T) {
	s := NewStore()
	const workers = 8
	const perWorker = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tok, _, err := s.Issue("u", "1:1", time.Minute)
				if err != nil {
					t.Errorf("Issue: %v", err)
					return
				}
				if _, ok := s.Verify(tok); !ok {
					t.Errorf("Verify of just-issued token failed")
					return
				}
			}
		}()
	}
	wg.Wait()
	if s.Len() != workers*perWorker {
		t.Errorf("Len=%d, want %d", s.Len(), workers*perWorker)
	}
}
