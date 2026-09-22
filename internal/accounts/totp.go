package accounts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters understood by every authenticator app (RFC 6238 defaults).
const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1 // accept one step either side for clock drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate 2FA secret: %w", err)
	}
	return b32.EncodeToString(b), nil
}

// totpCode computes the code for a time step (RFC 4226 dynamic truncation).
func totpCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", fmt.Errorf("bad 2FA secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) //nolint:gosec // step counts periods since 1970, never negative
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000), nil
}

// matchTOTP returns the time step code matches, or 0. Steps at or before
// lastStep are refused so a code can't be replayed.
func matchTOTP(secret, code string, now time.Time, lastStep int64) int64 {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0
	}
	current := now.Unix() / totpPeriod
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := current + d
		if step <= lastStep {
			continue
		}
		want, err := totpCode(secret, step)
		if err == nil && hmac.Equal([]byte(want), []byte(code)) {
			return step
		}
	}
	return 0
}

// totpURI is what the QR code encodes for authenticator apps.
func totpURI(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// newRecoveryCode returns a one-time code like "k3jd-9x2m-p4qa".
func newRecoveryCode() (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return sb.String(), nil
}

func normalizeRecoveryCode(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "")
}

// TOTPCode returns the authenticator code for secret at time t. It exists
// for tests and operational tools; sign-in uses the checks above.
func TOTPCode(secret string, t time.Time) (string, error) {
	return totpCode(secret, t.Unix()/totpPeriod)
}
