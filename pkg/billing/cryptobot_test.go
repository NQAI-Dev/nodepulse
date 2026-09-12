package billing

import "testing"

func TestCryptoBotMock(t *testing.T) {
	c := NewCryptoBot("")
	inv, err := c.CreateInvoice("5.00", "USDT", "Pro Plan", "1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !inv.Ok || inv.Result.PayURL == "" {
		t.Fatalf("invalid invoice result")
	}
}
