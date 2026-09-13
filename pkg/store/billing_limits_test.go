package store

import (
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// newBillingStore spins up a fresh in-memory PersistentStore with the
// billing schema applied. Reused by every test in this file to avoid
// SQLite "database is locked" cross-talk.
func newBillingStore(t *testing.T) *PersistentStore {
	t.Helper()
	s, err := NewPersistentStore(":memory:", "test", 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.InitBillingSchema(); err != nil {
		t.Fatalf("billing schema: %v", err)
	}
	return s
}

func TestCanAddNode_FreeUserRespectsLimit(t *testing.T) {
	s := newBillingStore(t)
	uidAlice, _, err := s.Register("alice", "secret123")
	if err != nil {
		t.Fatal(err)
	}

	if !s.CanAddNode(uidAlice) {
		t.Fatal("free user with zero nodes must be allowed to add one")
	}
	for i := 0; i < FreeNodeLimit; i++ {
		_ = s.BindNode("node-"+string(rune('a'+i)), uidAlice)
	}
	if s.CanAddNode(uidAlice) {
		t.Fatalf("free user with %d nodes must be blocked", FreeNodeLimit)
	}
}

func TestCanAddNode_ProIsUnlimited(t *testing.T) {
	s := newBillingStore(t)
	uid, _, _ := s.Register("bob", "secret123")
	s.db.Exec(`UPDATE users SET plan='pro', pro_until=datetime('now','+30 days') WHERE id=?`, uid)
	for i := 0; i < FreeNodeLimit+5; i++ {
		_ = s.BindNode("node-"+string(rune('a'+i)), uid)
	}
	if !s.CanAddNode(uid) {
		t.Fatal("pro user must never be blocked by node quota")
	}
}

func TestCanAddProbe_FreeUserBudgetsURLs(t *testing.T) {
	s := newBillingStore(t)
	uid, _, _ := s.Register("carol", "secret123")
	_ = s.BindNode("node-x", uid)

	// Seed probe_results so the URL-already-known path is exercised.
	// 3 known URLs leaves room for 2 more new URLs before the cap.
	now := int64(1_700_000_000)
	for i := 0; i < 3; i++ {
		url := "https://example.com/known-" + string(rune('a'+i))
		s.RecordProbeResults("node-x", []protocol.ProbeResult{{URL: url, Ts: now}})
	}

	// Two brand-new URLs still fit (3 known + 2 new = 5 ≤ FreeProbeLimit).
	if !s.CanAddProbe(uid, []string{"https://example.com/new-a", "https://example.com/new-b"}) {
		t.Fatal("free user must be allowed up to FreeProbeLimit distinct URLs")
	}

	// Three new URLs push past the cap (3 known + 3 new = 6 > 5).
	if s.CanAddProbe(uid, []string{"https://example.com/new-c", "https://example.com/new-d", "https://example.com/new-e"}) {
		t.Fatal("free user must be blocked once they cross FreeProbeLimit")
	}
}

func TestCanAddProbe_ProIsUnlimited(t *testing.T) {
	s := newBillingStore(t)
	uid, _, _ := s.Register("dave", "secret123")
	s.db.Exec(`UPDATE users SET plan='pro', pro_until=datetime('now','+30 days') WHERE id=?`, uid)
	urls := []string{}
	for i := 0; i < 20; i++ {
		urls = append(urls, "https://example.com/p-"+string(rune('a'+i)))
	}
	if !s.CanAddProbe(uid, urls) {
		t.Fatal("pro user must never be blocked by probe quota")
	}
}

func TestCanAddProbe_AlreadyKnownURLsDontCount(t *testing.T) {
	s := newBillingStore(t)
	uid, _, _ := s.Register("erin", "secret123")
	_ = s.BindNode("node-y", uid)

	// Saturate the quota with 5 known URLs.
	now := int64(1_700_000_000)
	known := []string{}
	for i := 0; i < FreeProbeLimit; i++ {
		u := "https://example.com/k-" + string(rune('a'+i))
		known = append(known, u)
		s.RecordProbeResults("node-y", []protocol.ProbeResult{{URL: u, Ts: now}})
	}

	// Re-sending the same URLs is fine — no new budget is consumed.
	if !s.CanAddProbe(uid, known) {
		t.Fatal("re-submitting already-known URLs must not consume quota")
	}
}

func TestGetPlanUsage_FreeShape(t *testing.T) {
	s := newBillingStore(t)
	uid, _, _ := s.Register("frank", "secret123")
	_ = s.BindNode("node-z", uid)
	s.RecordProbeResults("node-z", []protocol.ProbeResult{{URL: "https://x.test", Ts: 1}})

	u, err := s.GetPlanUsage(uid)
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "free" || u.IsPro {
		t.Fatalf("expected free plan, got %+v", u)
	}
	if u.NodesLimit != FreeNodeLimit || u.ProbesLimit != FreeProbeLimit || u.RetentionDay != FreeRetentionDay {
		t.Fatalf("free limits wrong: %+v", u)
	}
	if u.NodesUsed != 1 || u.ProbesUsed != 1 {
		t.Fatalf("usage counters wrong: %+v", u)
	}
}

func TestGetPlanUsage_ProShape(t *testing.T) {
	s := newBillingStore(t)
	uid, _, _ := s.Register("grace", "secret123")
	s.db.Exec(`UPDATE users SET plan='pro', pro_until='2099-01-01 00:00:00' WHERE id=?`, uid)

	u, _ := s.GetPlanUsage(uid)
	if !u.IsPro || u.Plan != "pro" {
		t.Fatalf("expected pro plan, got %+v", u)
	}
	if u.NodesLimit != -1 || u.ProbesLimit != -1 || u.RetentionDay != 90 {
		t.Fatalf("pro limits wrong: %+v", u)
	}
	if u.ProUntil == "" {
		t.Fatal("pro_until must be exposed for pro users")
	}
}
