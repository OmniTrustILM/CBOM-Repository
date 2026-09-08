package service

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OmniTrustILM/cbom-repository/internal/store"
)

// ErrInvalidCursor is returned by ParseCursor for a token that is not in the canonical
// form this service issues (see ParseCursor).
var ErrInvalidCursor = errors.New("invalid cursor")

const (
	cursorVersion   = "v1"
	cursorSeparator = "|"
	// maxCursorTokenLength bounds the token ParseCursor is willing to decode. An S3 key
	// is at most 1024 bytes, so a genuine token — version tag, millisecond timestamp
	// (13 digits for the foreseeable future), two separators and the key — is under
	// 1400 characters once encoded. Anything longer cannot be a canonical token and is
	// refused before it is decoded.
	maxCursorTokenLength = 2048
)

// Cursor is a position in the paged search order: the LIST LastModified of an entry at
// millisecond precision, then its object key. Millisecond is the finest precision an
// S3 listing carries (AWS reports whole seconds, MinIO milliseconds), and the key is
// unique, so the position is exact. The wire form (String) is opaque to clients: the
// base64url encoding, without padding, of `v1|<unixMillis>|<key>`. The key is the last
// field, so a `|` inside a key cannot break parsing.
//
// A cursor is valid within a run only; between runs the consumer restarts from its
// watermark (`after`).
type Cursor struct {
	lastModifiedMillis int64
	key                string
}

// NewCursor is the position of the listed object stamped `lastModified` under `key`.
// The stamp must be the LIST LastModified, never the HEAD one: the boundary a cursor
// draws has to come from the same clock that orders the candidates.
func NewCursor(lastModified time.Time, key string) Cursor {
	return Cursor{lastModifiedMillis: lastModified.UnixMilli(), key: key}
}

// String renders the cursor as the token a client echoes back in `cursor=`.
func (c Cursor) String() string {
	raw := cursorVersion + cursorSeparator + strconv.FormatInt(c.lastModifiedMillis, 10) + cursorSeparator + c.key
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor decodes a token issued by String. It accepts exactly the canonical form —
// base64url without padding, the `v1` tag, a non-negative decimal millisecond
// timestamp without sign or leading zeros, and a non-empty UTF-8 key — and returns
// ErrInvalidCursor (wrapped) for anything else, so that a truncated, re-encoded or
// otherwise non-canonical token is a 400 rather than a page from a surprising position.
// The token is a position, not a credential: it is not signed, and a client that
// assembles a canonical token itself simply pages from that position — the same data
// `after` hands anyone.
func ParseCursor(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, fmt.Errorf("%w: empty", ErrInvalidCursor)
	}
	if len(token) > maxCursorTokenLength {
		return Cursor{}, fmt.Errorf("%w: token longer than %d characters", ErrInvalidCursor, maxCursorTokenLength)
	}
	if strings.ContainsAny(token, "\r\n") {
		return Cursor{}, fmt.Errorf("%w: not base64url without padding", ErrInvalidCursor)
	}
	// The decoder skips newlines (refused above) and, unless strict, accepts padding
	// bits that are not zero — both decode to bytes String never produced them from;
	// everything else outside the base64url alphabet it rejects itself.
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: not base64url without padding: %w", ErrInvalidCursor, err)
	}

	parts := strings.SplitN(string(raw), cursorSeparator, 3)
	if len(parts) != 3 {
		return Cursor{}, fmt.Errorf("%w: expected %d fields, got %d", ErrInvalidCursor, 3, len(parts))
	}
	if parts[0] != cursorVersion {
		return Cursor{}, fmt.Errorf("%w: unknown version %q", ErrInvalidCursor, parts[0])
	}
	millis, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || millis < 0 || strconv.FormatInt(millis, 10) != parts[1] {
		return Cursor{}, fmt.Errorf("%w: timestamp %q is not a canonical non-negative integer", ErrInvalidCursor, parts[1])
	}
	key := parts[2]
	if key == "" || !utf8.ValidString(key) {
		return Cursor{}, fmt.Errorf("%w: key is empty or not UTF-8", ErrInvalidCursor)
	}
	return Cursor{lastModifiedMillis: millis, key: key}, nil
}

// listFrom is the `after` second to ask store.Search for when continuing from the
// cursor. The store keeps objects modified strictly after a whole second, and the
// cursor's own second has to stay in — on AWS S3 every object of that second carries
// the cursor's very stamp — so the listing starts one second earlier and
// cursorCandidates applies the exact (LastModified, key) filter.
func (c Cursor) listFrom() int64 {
	return c.lastModifiedMillis/1000 - 1
}

// LogValue renders the cursor for slog as its two fields, so a continued page's log
// lines say where it resumed from.
func (c Cursor) LogValue() slog.Value {
	return slog.GroupValue(slog.Int64("millis", c.lastModifiedMillis), slog.String("key", c.key))
}

// position is the cursor an object is handed out as: its place in the paged order.
func position(obj store.ObjectInfo) Cursor {
	return NewCursor(obj.LastModified, obj.Key)
}

// compare orders two positions: LastModified at millisecond precision, then key. It is
// the one definition of the paged order — selectCandidates sorts by it and precedes
// filters by it — so the order pages are cut in and the order a cursor resumes in
// cannot drift apart.
func (c Cursor) compare(o Cursor) int {
	return cmp.Or(cmp.Compare(c.lastModifiedMillis, o.lastModifiedMillis), strings.Compare(c.key, o.key))
}

// precedes reports whether the cursor sorts strictly before obj — whether obj is a
// candidate for the page the cursor continues.
func (c Cursor) precedes(obj store.ObjectInfo) bool {
	return c.compare(position(obj)) < 0
}
