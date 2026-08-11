package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTP second factor (RFC 6238, standard authenticator apps). The secret is
// 20 random bytes, base32-encoded for QR enrollment; codes are 6 digits in a
// 30-second window. A one-window +/- skew tolerance absorbs clock drift.

const (
	totpDigits   = 6
	totpPeriod   = 30 * time.Second
	totpSecretLen = 20
	totpWindow   = 1 // +/- one period of skew tolerance
)

// newTOTPSecret returns a fresh base32 secret for enrollment.
func newTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("rand totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// otpauthURI renders the enrollment URI an authenticator app scans.
func otpauthURI(secret, account string) string {
	// issuer + account are display-only; the label may be any printable text.
	return fmt.Sprintf("otpauth://totp/nowhere-agent:%s?secret=%s&issuer=nowhere-agent&digits=%d&period=%d",
		account, secret, totpDigits, int(totpPeriod.Seconds()))
}

// totpCode computes the code for a Unix time (RFC 6238, SHA-1).
func totpCode(secret string, at time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", fmt.Errorf("totp: bad secret: %w", err)
	}
	counter := uint64(at.Unix() / int64(totpPeriod.Seconds()))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, code%mod), nil
}

// verifyTOTP checks code against secret with +/-totpWindow skew tolerance,
// constant-time per candidate.
func verifyTOTP(secret, code string, now time.Time) (bool, error) {
	for offset := -totpWindow; offset <= totpWindow; offset++ {
		want, err := totpCode(secret, now.Add(time.Duration(offset)*totpPeriod))
		if err != nil {
			return false, err
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true, nil
		}
	}
	return false, nil
}
