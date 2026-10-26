package ingest

import "testing"

func TestParseCoin(t *testing.T) {
	amt, denom := ParseCoin("1000000ugnot")
	if amt != 1000000 || denom != "ugnot" {
		t.Fatalf("got %d %q", amt, denom)
	}
	amt, denom = ParseCoin("2000000ugnot,1foo")
	if amt != 2000000 || denom != "ugnot" {
		t.Fatalf("got %d %q", amt, denom)
	}
}
