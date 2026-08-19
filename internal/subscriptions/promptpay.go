package subscriptions

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// promptPayInstructions builds the checkout QR + manual payment details for a
// plan. When no platform PromptPay target is configured the QR payload is
// omitted (Available=false) and only the amount renders, so the app still runs
// in development without a real receiving account.
func (s *Service) promptPayInstructions(plan *Plan) PromptPayInstructions {
	inst := PromptPayInstructions{
		AmountMinor: plan.PriceMinor,
		Currency:    plan.Currency,
	}
	target := strings.TrimSpace(s.cfg.PromptPayTarget)
	if target == "" {
		return inst
	}
	inst.Target = target
	inst.DisplayName = s.cfg.PromptPayName

	payload, err := buildPromptPayPayload(target, s.cfg.PromptPayName, plan.PriceMinor)
	if err != nil {
		// A misconfigured target is not fatal - the reader can still pay manually
		// with the target and amount shown above.
		s.log.Warn("subscriptions: promptpay payload generation failed", slog.Any("error", err))
		return inst
	}
	inst.Payload = payload
	inst.Available = true
	return inst
}

// PromptPay QR payload generation (Thai QR Payment / EMVCo).
//
// This builds the STANDARD EMVCo TLV payload a PromptPay QR encodes. It is the
// Thai national payment-QR standard, NOT a payment provider or SDK - no vendor
// is selected (payment_provider stays OPEN, docs/MONETIZATION.md §6), and there
// is no external dependency. The QR pays the PLATFORM's PromptPay account so a
// reader can transfer the Premium price; it is a convenience for the Phase 1
// manual-verification flow, never the confirmation itself (brief §15–§16).

// promptPayAID is the Application Identifier for PromptPay (EMVCo tag 29 → 00).
const promptPayAID = "A000000677010111"

// errNoPromptPayTarget is returned when no platform PromptPay id is configured.
var errNoPromptPayTarget = errors.New("no promptpay target configured")

// buildPromptPayPayload returns the EMVCo payload string for a dynamic
// (amount-carrying) PromptPay QR that pays `target` the given amount. target is
// a phone number, 13-digit national id, or 15-digit e-wallet id (digits only).
// name is the merchant display name (may be empty).
func buildPromptPayPayload(target, name string, amountMinor int64) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errNoPromptPayTarget
	}
	account, err := promptPayAccountTLV(target)
	if err != nil {
		return "", err
	}
	if amountMinor <= 0 {
		return "", errors.New("amount must be positive")
	}

	var b strings.Builder
	b.WriteString(tlv("00", "01")) // Payload Format Indicator
	b.WriteString(tlv("01", "12")) // Point of Initiation: 12 = dynamic (amount present)
	// Tag 29: merchant account information for PromptPay.
	b.WriteString(tlv("29", tlv("00", promptPayAID)+account))
	b.WriteString(tlv("53", "764"))                            // Currency: THB (ISO 4217 numeric)
	b.WriteString(tlv("54", formatAmountDecimal(amountMinor))) // Transaction amount
	b.WriteString(tlv("58", "TH"))                             // Country

	if name = sanitizePromptPayName(name); name != "" {
		b.WriteString(tlv("59", name))      // Merchant name
		b.WriteString(tlv("60", "Bangkok")) // Merchant city (required alongside name)
	}

	// Tag 63 (CRC) is computed over everything up to and INCLUDING "6304".
	withCRCTag := b.String() + "6304"
	b.WriteString(fmt.Sprintf("6304%04X", crc16CCITT(withCRCTag)))
	return b.String(), nil
}

// promptPayAccountTLV chooses the right sub-tag for the target:
//
//	13 digits → national id / tax id (tag 02)
//	15 digits → e-wallet id          (tag 03)
//	otherwise → mobile number        (tag 01), normalised to 0066XXXXXXXXX
func promptPayAccountTLV(target string) (string, error) {
	for _, r := range target {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("promptpay target must be digits only")
		}
	}
	switch len(target) {
	case 13:
		return tlv("02", target), nil
	case 15:
		return tlv("03", target), nil
	default:
		mobile, err := normalizeThaiMobile(target)
		if err != nil {
			return "", err
		}
		return tlv("01", mobile), nil
	}
}

// normalizeThaiMobile turns a Thai phone number into the PromptPay mobile
// format: country code 66 + the subscriber number, zero-padded to 13 digits.
// Accepts "0812345678", "812345678", or "66812345678".
func normalizeThaiMobile(raw string) (string, error) {
	digits := strings.TrimPrefix(raw, "0")
	if !strings.HasPrefix(digits, "66") {
		digits = "66" + digits
	}
	// A valid Thai mobile normalises to "66" + 9 digits = 11 characters.
	if len(digits) != 11 {
		return "", fmt.Errorf("invalid thai mobile number")
	}
	for len(digits) < 13 {
		digits = "0" + digits
	}
	return digits, nil
}

// formatAmountDecimal renders integer satang as a THB decimal string with
// exactly two fraction digits (9900 → "99.00"). Money is never floating point;
// this is pure integer arithmetic on the way out.
func formatAmountDecimal(amountMinor int64) string {
	return fmt.Sprintf("%d.%02d", amountMinor/100, amountMinor%100)
}

// sanitizePromptPayName bounds the merchant name to EMVCo tag 59's 25-character
// limit and drops anything non-printable-ASCII, since the QR alphabet is ASCII.
func sanitizePromptPayName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 25 {
		out = out[:25]
	}
	return out
}

// tlv encodes one EMVCo field: 2-char id, 2-char zero-padded length, value.
func tlv(id, value string) string {
	return id + fmt.Sprintf("%02d", len(value)) + value
}

// crc16CCITT computes the CRC-16/CCITT-FALSE checksum EMVCo tag 63 requires:
// polynomial 0x1021, initial value 0xFFFF, no reflection.
func crc16CCITT(s string) uint16 {
	crc := uint16(0xFFFF)
	for i := 0; i < len(s); i++ {
		crc ^= uint16(s[i]) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
