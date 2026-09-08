package service_test

import (
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/cbom-repository/internal/service"

	"github.com/stretchr/testify/require"
)

// A cursor round-trips through its token for every key shape this service can list,
// including a key containing the field separator: the key is the last field, so a `|`
// inside it cannot break parsing.
func TestCursor_RoundTrip(t *testing.T) {
	when := time.Date(2026, 9, 2, 10, 0, 0, 123_456_789, time.UTC)
	for _, key := range []string{
		"urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79-1",
		"urn:uuid:3e671687-395b-41f5-a30f-a58921a69b79-original",
		"weird|key|with|pipes-1",
		"ünïcödé-1",
		"README",
	} {
		t.Run(key, func(t *testing.T) {
			c := service.NewCursor(when, key)
			token := c.String()
			require.Regexp(t, `^[A-Za-z0-9_-]+$`, token, "base64url without padding: safe in a query string as-is")

			parsed, err := service.ParseCursor(token)
			require.NoError(t, err)
			require.Equal(t, c, parsed)
		})
	}
}

// The token is the documented, versioned triple — a client (or a test) can decode it,
// even though it must treat it as opaque. Sub-millisecond nanoseconds are dropped: the
// cursor's precision is the millisecond, the finest an S3 listing carries.
func TestCursor_TokenIsBase64URLOfVersionedTriple(t *testing.T) {
	when := time.UnixMilli(1788451200123).Add(700 * time.Microsecond).UTC() // truncates to .123, would round to .124
	c := service.NewCursor(when, "urn:uuid:a-1")

	raw, err := base64.RawURLEncoding.DecodeString(c.String())
	require.NoError(t, err)
	require.Equal(t, "v1|1788451200123|urn:uuid:a-1", string(raw))
	require.Equal(t, service.NewCursor(when.Truncate(time.Millisecond), "urn:uuid:a-1"), c,
		"two stamps in the same millisecond are the same cursor")
}

// ParseCursor accepts exactly the tokens this service issues and nothing else: the
// canonical v1 triple, base64url without padding. Everything below is a 400 at the
// HTTP layer.
func TestParseCursor_Malformed(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	cases := map[string]string{
		"empty":                            "",
		"not base64":                       "!!!",
		"standard alphabet (+ and /)":      base64.RawStdEncoding.EncodeToString([]byte{0xfb, 0xff, 0xbf}),
		"padded base64url":                 base64.URLEncoding.EncodeToString([]byte("v1|1788451200123|urn:uuid:a-1")),
		"wrong version tag":                enc("v2|1788451200123|urn:uuid:a-1"),
		"no separators":                    enc("v1"),
		"version only":                     enc("v1|"),
		"missing key field":                enc("v1|1788451200123"),
		"empty key":                        enc("v1|1788451200123|"),
		"empty timestamp":                  enc("v1||urn:uuid:a-1"),
		"non-numeric timestamp":            enc("v1|abc|urn:uuid:a-1"),
		"negative timestamp":               enc("v1|-1|urn:uuid:a-1"),
		"timestamp with explicit sign":     enc("v1|+1|urn:uuid:a-1"),
		"timestamp with leading zeros":     enc("v1|0123|urn:uuid:a-1"),
		"timestamp overflowing int64":      enc("v1|99999999999999999999|urn:uuid:a-1"),
		"key that is not UTF-8":            enc("v1|1|\xff\xfe-1"),
		"token longer than any key allows": enc("v1|1|" + strings.Repeat("k", 3000)),
		// encoding/base64 skips newlines and, unless told to be strict, accepts padding
		// bits that are not zero; neither appears in a token String produces.
		"newline inside the token":         enc("v1|1|urn:uuid:a-1")[:4] + "\n" + enc("v1|1|urn:uuid:a-1")[4:],
		"carriage return inside the token": enc("v1|1|urn:uuid:a-1")[:4] + "\r" + enc("v1|1|urn:uuid:a-1")[4:],
		"non-canonical trailing bits":      nonCanonicalTrailingBits(enc("v1|1|urn:uuid:ab")),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := service.ParseCursor(token)
			require.ErrorIs(t, err, service.ErrInvalidCursor)
		})
	}
}

// The epoch is a legitimate position (an `after=0` run can only produce later stamps,
// but nothing in the format forbids it).
func TestParseCursor_EpochIsValid(t *testing.T) {
	c, err := service.ParseCursor(base64.RawURLEncoding.EncodeToString([]byte("v1|0|urn:uuid:a-1")))
	require.NoError(t, err)
	require.Equal(t, service.NewCursor(time.UnixMilli(0), "urn:uuid:a-1"), c)
}

// nonCanonicalTrailingBits flips the lowest padding bit of the token's last character:
// a lenient decoder yields the same bytes from it, but no encoder produces it.
func nonCanonicalTrailingBits(token string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, token[len(token)-1])
	return token[:len(token)-1] + string(alphabet[last+1])
}

// LogValue renders the two fields a continued page's log lines need.
func TestCursor_LogValue(t *testing.T) {
	c := service.NewCursor(time.UnixMilli(1788451200123), "urn:uuid:a-1")
	v := c.LogValue()
	require.Equal(t, slog.KindGroup, v.Kind())
	require.Equal(t, []slog.Attr{slog.Int64("millis", 1788451200123), slog.String("key", "urn:uuid:a-1")}, v.Group())
}
