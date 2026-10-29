package ledger

import (
	"testing"

	"github.com/Natthyx/payment-gateway/internal/models"
)

func TestValidateBalance_Balanced(t *testing.T) {
	entries := []models.LedgerEntry{
		{Amount: -1050},
		{Amount: 1020},
		{Amount: 30},
	}
	if err := validateBalance(entries); err != nil {
		t.Fatalf("expected balanced, got %v", err)
	}
}

func TestValidateBalance_Imbalanced(t *testing.T) {
	entries := []models.LedgerEntry{
		{Amount: -1050},
		{Amount: 1020},
		// missing 30 fee credit
	}
	if err := validateBalance(entries); err == nil {
		t.Fatal("expected imbalance error")
	}
}

func TestLedgerPair_Validate(t *testing.T) {
	ok := &models.LedgerPair{
		Debit:  models.LedgerEntry{Amount: -1000},
		Credit: models.LedgerEntry{Amount: 1000},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("balanced pair: %v", err)
	}
	bad := &models.LedgerPair{
		Debit:  models.LedgerEntry{Amount: -1000},
		Credit: models.LedgerEntry{Amount: 999},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected imbalance")
	}
}
