package subscriptions

import (
	"fmt"
	"strings"
	"testing"
)

func TestFormatAmountDecimal(t *testing.T) {
	cases := map[int64]string{
		9900:  "99.00",
		99000: "990.00",
		19900: "199.00",
		100:   "1.00",
		5:     "0.05",
		0:     "0.00",
	}
	for minor, want := range cases {
		if got := formatAmountDecimal(minor); got != want {
			t.Errorf("formatAmountDecimal(%d) = %q, want %q", minor, got, want)
		}
	}
}

func TestNormalizeThaiMobile(t *testing.T) {
	// All three accepted spellings map to the same PromptPay 13-digit form.
	for _, in := range []string{"0812345678", "812345678", "66812345678"} {
		got, err := normalizeThaiMobile(in)
		if err != nil {
			t.Fatalf("normalizeThaiMobile(%q) error: %v", in, err)
		}
		if got != "0066812345678" {
			t.Errorf("normalizeThaiMobile(%q) = %q, want 0066812345678", in, got)
		}
	}
	if _, err := normalizeThaiMobile("123"); err == nil {
		t.Error("expected error for a too-short mobile number")
	}
}

func TestBuildPromptPayPayload_MobileStructureAndCRC(t *testing.T) {
	payload, err := buildPromptPayPayload("0812345678", "FICTIONTHAI", 9900)
	if err != nil {
		t.Fatalf("buildPromptPayPayload error: %v", err)
	}

	// EMVCo header: format indicator then dynamic point-of-initiation.
	if !strings.HasPrefix(payload, "000201010212") {
		t.Errorf("payload does not start with the EMVCo dynamic header: %q", payload)
	}
	// PromptPay AID inside tag 29.
	if !strings.Contains(payload, "0016A000000677010111") {
		t.Error("payload missing the PromptPay AID")
	}
	// Mobile account: tag 01, length 13, normalised number.
	if !strings.Contains(payload, "01130066812345678") {
		t.Error("payload missing the normalised mobile account TLV")
	}
	// Currency THB (764) and the amount.
	if !strings.Contains(payload, "5303764") {
		t.Error("payload missing the THB currency tag")
	}
	if !strings.Contains(payload, "540599.00") {
		t.Errorf("payload missing the amount tag 54 for 99.00: %q", payload)
	}

	// CRC self-consistency: the last 4 chars must be the CRC over everything
	// before them (including the "6304" tag+length).
	if len(payload) < 8 {
		t.Fatalf("payload too short: %q", payload)
	}
	body, crc := payload[:len(payload)-4], payload[len(payload)-4:]
	want := fmt.Sprintf("%04X", crc16CCITT(body))
	if crc != want {
		t.Errorf("CRC = %q, recomputed %q", crc, want)
	}
}

func TestBuildPromptPayPayload_NationalIDAndEWallet(t *testing.T) {
	// 13-digit national id → tag 02.
	nat, err := buildPromptPayPayload("1234567890123", "", 19900)
	if err != nil {
		t.Fatalf("national id payload error: %v", err)
	}
	if !strings.Contains(nat, "02131234567890123") {
		t.Error("national id payload missing tag 02 account")
	}
	// 15-digit e-wallet → tag 03.
	wallet, err := buildPromptPayPayload("123456789012345", "", 19900)
	if err != nil {
		t.Fatalf("e-wallet payload error: %v", err)
	}
	if !strings.Contains(wallet, "0315123456789012345") {
		t.Error("e-wallet payload missing tag 03 account")
	}
}

func TestBuildPromptPayPayload_Rejects(t *testing.T) {
	if _, err := buildPromptPayPayload("", "x", 9900); err == nil {
		t.Error("expected error for an empty target")
	}
	if _, err := buildPromptPayPayload("08x2345678", "x", 9900); err == nil {
		t.Error("expected error for a non-digit target")
	}
	if _, err := buildPromptPayPayload("0812345678", "x", 0); err == nil {
		t.Error("expected error for a non-positive amount")
	}
}
