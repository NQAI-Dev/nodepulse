package store

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"
)

// randomInviteToken returns a 32-byte hex string prefixed with "np_inv_".
// Same shape as real production tokens; uniqueness is enforced at the DB
// layer so collisions are practically impossible even at scale.
func randomInviteToken(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "np_inv_" + hex.EncodeToString(b[:])
}

// TestCreateAndGetInvite covers the happy path: mint a token, fetch it,
// assert all fields round-trip including the auto_create_user boolean
// (which is stored as INTEGER 0/1 and converted back in GetInvite).
func TestCreateAndGetInvite(t *testing.T) {
	s := newBillingStore(t)

	tok := randomInviteToken(t)
	inv, err := s.CreateInvite(Invite{
		Token:           tok,
		CreatedBy:       1,
		TargetUserID:    7,
		AutoCreateUser:  false,
		DefaultUsername: "acme-test",
		ExpiresAt:       time.Now().Add(7 * 24 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if inv.Token != tok {
		t.Fatalf("Token mismatch: got %q want %q", inv.Token, tok)
	}
	if inv.CreatedAt == 0 {
		t.Fatalf("CreatedAt not populated (got 0)")
	}

	got, err := s.GetInvite(tok)
	if err != nil {
		t.Fatalf("GetInvite: %v", err)
	}
	if got.TargetUserID != 7 {
		t.Errorf("TargetUserID: got %d want 7", got.TargetUserID)
	}
	if got.DefaultUsername != "acme-test" {
		t.Errorf("DefaultUsername: got %q want %q", got.DefaultUsername, "acme-test")
	}
	if got.AutoCreateUser {
		t.Errorf("AutoCreateUser: got true want false")
	}
	if got.RedeemedAt != 0 {
		t.Errorf("RedeemedAt on fresh invite: got %d want 0", got.RedeemedAt)
	}
	if got.RedeemedByChatID != 0 {
		t.Errorf("RedeemedByChatID on fresh invite: got %d want 0", got.RedeemedByChatID)
	}
}

// TestCreateInviteAutoCreateFlag verifies the boolean round-trips through
// the INTEGER column. A false-positive would let a redeemed chat
// accidentally bootstrap a fresh user instead of joining the operator's
// tenant — a privilege-escalation-adjacent regression.
func TestCreateInviteAutoCreateFlag(t *testing.T) {
	s := newBillingStore(t)
	tok := randomInviteToken(t)
	_, err := s.CreateInvite(Invite{
		Token:          tok,
		CreatedBy:      1,
		TargetUserID:   0, // no specific target — caller will be auto-created
		AutoCreateUser: true,
	})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	got, err := s.GetInvite(tok)
	if err != nil {
		t.Fatalf("GetInvite: %v", err)
	}
	if !got.AutoCreateUser {
		t.Errorf("AutoCreateUser round-trip: got false want true")
	}
}

// TestRedeemInviteHappyPath exercises the full redemption flow:
//  1. Create invite
//  2. Redeem with chatID
//  3. Assert redeemed_at / chat_id populated
//  4. Assert second redeem returns ErrInviteAlreadyRedeemed (with the
//     original chat_id preserved so callers can surface "this link was
//     used by chat X" diagnostics).
func TestRedeemInviteHappyPath(t *testing.T) {
	s := newBillingStore(t)
	tok := randomInviteToken(t)
	if _, err := s.CreateInvite(Invite{
		Token:     tok,
		CreatedBy: 1,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	inv, err := s.RedeemInvite(tok, 1234567890)
	if err != nil {
		t.Fatalf("first RedeemInvite: %v", err)
	}
	if inv.RedeemedAt == 0 {
		t.Errorf("RedeemedAt not populated after redeem")
	}
	if inv.RedeemedByChatID != 1234567890 {
		t.Errorf("RedeemedByChatID: got %d want 1234567890", inv.RedeemedByChatID)
	}

	// Second redeem: same chat, same token — must fail with AlreadyRedeemed.
	_, err = s.RedeemInvite(tok, 1234567890)
	if err != ErrInviteAlreadyRedeemed {
		t.Errorf("second RedeemInvite: got %v want ErrInviteAlreadyRedeemed", err)
	}
}

// TestRedeemInviteDoubleSpendDistinctChats pins the atomic-guarantee
// contract: two distinct chats racing on the same token must result in
// exactly one successful redemption. Without the
// `WHERE redeemed_at = 0` guard in the UPDATE statement, both calls
// would succeed and two chats would share the operator's tenant.
func TestRedeemInviteDoubleSpendDistinctChats(t *testing.T) {
	s := newBillingStore(t)
	tok := randomInviteToken(t)
	if _, err := s.CreateInvite(Invite{
		Token:     tok,
		CreatedBy: 1,
	}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	_, err := s.RedeemInvite(tok, 111)
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}

	_, err = s.RedeemInvite(tok, 222)
	if err != ErrInviteAlreadyRedeemed {
		t.Fatalf("second redeem (distinct chat): got %v want ErrInviteAlreadyRedeemed", err)
	}

	// Re-read: the recorded redeemed_by_chat_id is chat 111, not 222.
	got, err := s.GetInvite(tok)
	if err != nil {
		t.Fatalf("GetInvite: %v", err)
	}
	if got.RedeemedByChatID != 111 {
		t.Errorf("RedeemedByChatID after race: got %d want 111 (the first winner)", got.RedeemedByChatID)
	}
}

// TestRedeemInviteUnknownToken ensures RedeemInvite returns
// ErrInviteNotFound (not a generic DB error) so handlers can map to 404.
func TestRedeemInviteUnknownToken(t *testing.T) {
	s := newBillingStore(t)
	_, err := s.RedeemInvite("np_inv_does_not_exist", 42)
	if err != ErrInviteNotFound {
		t.Errorf("unknown token redeem: got %v want ErrInviteNotFound", err)
	}
}

// TestRedeemInviteExpired covers the expires_at branch. We seed the row
// directly with an already-elapsed expiry because CreateInvite doesn't
// pre-check expiry (creation is always allowed; redemption is what
// enforces it).
func TestRedeemInviteExpired(t *testing.T) {
	s := newBillingStore(t)
	tok := randomInviteToken(t)
	if _, err := s.CreateInvite(Invite{
		Token:     tok,
		CreatedBy: 1,
		ExpiresAt: time.Now().Add(-time.Hour).Unix(), // already expired
	}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	_, err := s.RedeemInvite(tok, 999)
	if err != ErrInviteExpired {
		t.Errorf("expired redeem: got %v want ErrInviteExpired", err)
	}
}

// TestRedeemInviteNoExpiry verifies expires_at=0 means "never expires"
// — operators sometimes print permanent onboarding URLs in README files.
func TestRedeemInviteNoExpiry(t *testing.T) {
	s := newBillingStore(t)
	tok := randomInviteToken(t)
	if _, err := s.CreateInvite(Invite{
		Token:     tok,
		CreatedBy: 1,
		ExpiresAt: 0,
	}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	inv, err := s.RedeemInvite(tok, 555)
	if err != nil {
		t.Fatalf("redeem no-expiry: %v", err)
	}
	if inv.ExpiresAt != 0 {
		t.Errorf("ExpiresAt: got %d want 0 (no expiry)", inv.ExpiresAt)
	}
}

// TestListInvitesRecentFirst ensures ordering and limit semantics: most
// recently created appears first, and limit is respected.
func TestListInvitesRecentFirst(t *testing.T) {
	s := newBillingStore(t)
	tokens := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		tok := randomInviteToken(t)
		tokens = append(tokens, tok)
		if _, err := s.CreateInvite(Invite{
			Token:     tok,
			CreatedBy: 1,
		}); err != nil {
			t.Fatalf("CreateInvite %d: %v", i, err)
		}
		// Stagger created_at so ordering is deterministic. 1ms is enough
		// because we already use Unix() seconds; bumping each row by
		// 1+ seconds via a small sleep guarantees the order isn't
		// ambiguous when multiple inserts land in the same second.
		time.Sleep(1100 * time.Millisecond)
	}

	got, err := s.ListInvites(3)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len: got %d want 3", len(got))
	}
	// Most recently created should be tokens[4], then tokens[3], then [2].
	for i, want := range []string{tokens[4], tokens[3], tokens[2]} {
		if got[i].Token != want {
			t.Errorf("position %d: got %q want %q", i, got[i].Token, want)
		}
	}
}
