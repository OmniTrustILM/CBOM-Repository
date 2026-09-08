package service_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/cbom-repository/internal/service"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"
)

// decodeCursor opens the token the way a curious client could: base64url of
// `v1|<unixMillis>|<key>`. Tests use it to check what a cursor points at.
func decodeCursor(t *testing.T, c service.Cursor) (millis int64, key string) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(c.String())
	require.NoError(t, err)
	parts := strings.SplitN(string(raw), "|", 3)
	require.Len(t, parts, 3)
	millis, err = strconv.ParseInt(parts[1], 10, 64)
	require.NoError(t, err)
	return millis, parts[2]
}

// pairAfterCursor reports whether (lastModified, key) sorts strictly after the cursor —
// the candidate rule, evaluated at the cursor's millisecond precision.
func pairAfterCursor(t *testing.T, c service.Cursor, lastModified time.Time, key string) bool {
	t.Helper()
	millis, ckey := decodeCursor(t, c)
	if lastModified.UnixMilli() != millis {
		return lastModified.UnixMilli() > millis
	}
	return key > ckey
}

func searchAfter(t *testing.T, svc service.Service, after int64, limit int) service.SearchPage {
	t.Helper()
	page, err := svc.Search(context.Background(), service.SearchRequest{After: after, Limit: limit})
	require.NoError(t, err)
	return page
}

func searchCursor(t *testing.T, svc service.Service, c service.Cursor, limit int) service.SearchPage {
	t.Helper()
	// Round-trip through the token, as the HTTP layer does.
	parsed, err := service.ParseCursor(c.String())
	require.NoError(t, err)
	page, err := svc.Search(context.Background(), service.SearchRequest{Cursor: &parsed, Limit: limit})
	require.NoError(t, err)
	return page
}

// iterateCursor runs the cursor protocol once: open the run with after+limit, follow
// Next until it is absent. It returns every entry yielded, the page sizes and the cursor
// each page was fetched with (nil for the opening page), and asserts the page-shape
// rules: a page is sorted by (LastModified ms, key) as the fake lists it; every entry of
// a continued page sorts strictly after the cursor that fetched it; a page shorter than
// `limit` has no Next; a continued page that has a Next holds exactly `limit` entries
// (the hard limit); the opening page with a Next holds at least `limit` (the soft limit).
func iterateCursor(t *testing.T, svc service.Service, s3 *fakeS3, after int64, limit int) ([]service.SearchRes, []int, []*service.Cursor) {
	t.Helper()
	var all []service.SearchRes
	var sizes []int
	var cursors []*service.Cursor
	page := searchAfter(t, svc, after, limit)
	var cursor *service.Cursor
	for {
		sizes = append(sizes, len(page.Entries))
		cursors = append(cursors, cursor)
		require.Less(t, len(sizes), 10_000, "the run must terminate")
		for i, e := range page.Entries {
			key := entryID(e)
			stamp := s3.objects[key].lastModified
			if i > 0 {
				prev := entryID(page.Entries[i-1])
				require.True(t, pairLess(s3.objects[prev].lastModified, prev, stamp, key), "page must be ordered by (LastModified ms, key): %s before %s", prev, key)
			}
			if cursor != nil {
				require.True(t, pairAfterCursor(t, *cursor, stamp, key), "%s must sort strictly after the cursor that fetched its page", key)
			}
		}
		if len(page.Entries) > 0 {
			require.Greater(t, createdAtUnix(t, page.Entries[0]), after, "entries must be strictly after `after`")
		}
		all = append(all, page.Entries...)
		if len(page.Entries) < limit {
			require.Nil(t, page.Next, "a page shorter than limit is the last page")
		}
		if page.Next == nil {
			return all, sizes, cursors
		}
		if cursor == nil {
			require.GreaterOrEqual(t, len(page.Entries), limit, "the opening page is soft: at least limit when more remains")
		} else {
			require.Equal(t, limit, len(page.Entries), "a continued page is hard: exactly limit when more remains")
		}
		cursor = page.Next
		page = searchCursor(t, svc, *cursor, limit)
	}
}

// pairLess is the page order: LastModified at millisecond precision, then key.
func pairLess(aStamp time.Time, aKey string, bStamp time.Time, bKey string) bool {
	if aStamp.UnixMilli() != bStamp.UnixMilli() {
		return aStamp.UnixMilli() < bStamp.UnixMilli()
	}
	return aKey < bKey
}

// Deterministic walk through the cursor protocol with concurrent uploads landing between
// pages. Same seed as the deliverable-1 walk (TestSearch_Paged_ProtocolWithConcurrentUploads):
// 5 objects at +0, 3 at +1, 1 at +2, 8 at +3 (odd ones with millisecond offsets, as
// MinIO reports them), 8 at +10; limit = 4. The opening page keeps the soft limit; every
// continued page holds exactly 4.
func TestSearch_Cursor_ProtocolWithConcurrentUploads(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(7) // small LIST pages so pagination is exercised
	seed := map[string]time.Duration{}
	add := func(n int, offset time.Duration, label string) {
		for i := 0; i < n; i++ {
			d := offset
			if i%2 == 1 {
				d += time.Duration(100*(i+1)) * time.Millisecond
			}
			key := fmt.Sprintf("urn:uuid:%s-%02d-1", label, i)
			seed[key] = d
			s3.putWithStats(key, base.Add(d))
		}
	}
	add(5, 0, "s00")
	add(3, 1*time.Second, "s01")
	add(1, 2*time.Second, "s02")
	add(8, 3*time.Second, "s03")
	add(8, 10*time.Second, "s10")

	// Three uploads land while the run is in progress. Before listing 2, one into a
	// second still ahead of the cursor (+7 s). Before listing 4, one into second +3 at
	// +3.9 s — page 3 has just yielded the whole-second part of +3, but the cursor sits
	// at (+3.000, s03-06), so this object is still ahead of it and the run picks it up:
	// the millisecond cursor is finer than the whole-second `after`. Before listing 5,
	// one at +3.5 s — behind the cursor by then (+3.800, s03-07), so this run can never
	// see it; only the next run, thanks to the overlap.
	s3.beforeListing = func(listing int) {
		switch listing {
		case 2:
			s3.putWithStats("urn:uuid:late-later-1", base.Add(7*time.Second))
		case 4:
			s3.putWithStats("urn:uuid:late-boundary-1", base.Add(3900*time.Millisecond))
		case 5:
			s3.putWithStats("urn:uuid:late-behind-1", base.Add(3500*time.Millisecond))
		}
	}
	svc := newSvc(t, s3)

	run1, sizes, _ := iterateCursor(t, svc, s3, base.Unix()-1, 4)
	// page 1 (after, soft): +0 in (ms, key) order is 00, 02, 04, 01(.2), 03(.4); limit hit
	//        at 01 → the second is completed → 5; cursor = (+0.400, s00-03)
	// page 2 (cursor, hard): s01-00, s01-02, s01-01(.2), s02-00 → exactly 4; cursor = (+2.000, s02-00)
	// page 3: s03-00, 02, 04, 06 → 4; cursor = (+3.000, s03-06) — the rest of second +3 waits
	// page 4: s03-01(.2), 03(.4), 05(.6), 07(.8) → 4; cursor = (+3.800, s03-07);
	//         late-boundary (+3.9) was injected before this listing and sorts after 07
	// page 5: late-boundary(+3.9), late-later(+7), s10-00, s10-02 → 4; late-behind (+3.5)
	//         was injected before this listing but sits behind the cursor
	// page 6: s10-04, s10-06, s10-01(.2), s10-03(.4) → 4
	// page 7: s10-05(.6), s10-07(.8) → 2 → last page, no Next
	require.Equal(t, []int{5, 4, 4, 4, 4, 4, 2}, sizes)

	// Cursor mode HEADs exactly the candidates it walks: one per yielded entry here, since
	// nothing vanishes and no candidate is examined twice.
	require.Equal(t, len(run1), s3.headCallCount(), "every HEAD call in run 1 corresponds to a yielded entry")

	seen := idSet(run1)
	for key := range seed {
		require.Equal(t, 1, seen[key], "seeded object %s must be yielded exactly once within one run", key)
	}
	require.Equal(t, 1, seen["urn:uuid:late-later-1"], "an upload into a later second is yielded by the same run")
	require.Equal(t, 1, seen["urn:uuid:late-boundary-1"], "an upload into the boundary second but ahead of the millisecond cursor is yielded by the same run")
	require.Equal(t, 0, seen["urn:uuid:late-behind-1"], "an upload behind the cursor is deferred to the next run")

	// The documented rule: the next run starts from (run start − overlap). Run 1
	// conceptually started at base+10.5 s and the overlap must cover the longest upload
	// duration — 8 s here, so run 2 opens with `after` = base+2 and picks the late upload
	// up. That `after` sits in the middle of the data on purpose: run 2 cannot see
	// seconds +0..+2, so the union assertion only holds if run 1 delivered them.
	s3.beforeListing = nil
	runStart := base.Add(10500 * time.Millisecond)
	run2, sizes2, _ := iterateCursor(t, svc, s3, runStart.Unix()-8, 4)
	// page 1 (after, soft): the whole of second +3 — 8 seeded + late-boundary + late-behind = 10
	// then late-later + the 8 of +10 in hard pages of 4: 4, 4, 1
	require.Equal(t, []int{10, 4, 4, 1}, sizes2)
	require.Equal(t, 1, idSet(run2)["urn:uuid:late-behind-1"])
	assertNoEntryAtOrBefore(t, run2, base.Add(2*time.Second).Unix())
	assertEveryObjectYielded(t, s3, append(run1, run2...))
}

// An object overwritten in place while a run is in progress moves to a new stamp and is
// yielded again under it — at-least-once, never a skip — and the run still terminates.
// Unlike the `after` protocol, the run ends on the page that carries no Next rather than
// on an empty page.
func TestSearch_Cursor_OverwriteDuringRunYieldsTwice(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	for i, key := range []string{"urn:uuid:a-1", "urn:uuid:b-1", "urn:uuid:c-1", "urn:uuid:d-1"} {
		s3.putWithStats(key, base.Add(time.Duration(i+1)*time.Second))
	}
	// Page 1 yields `a` at +1 and the cursor points at it. Before listing 2, `a` is
	// overwritten and its LIST and HEAD clocks both move to +5 — ahead of the cursor, so
	// the same object becomes a candidate again.
	s3.beforeListing = func(listing int) {
		if listing == 2 {
			s3.overwrite("urn:uuid:a-1", base.Add(5*time.Second))
		}
	}
	svc := newSvc(t, s3)

	run, sizes, _ := iterateCursor(t, svc, s3, base.Unix(), 1)
	require.Equal(t, []int{1, 1, 1, 1, 1}, sizes, "one entry per page; the last page carries no Next")

	seen := idSet(run)
	require.Equal(t, 2, seen["urn:uuid:a-1"], "the overwritten object is yielded once per stamp")
	for _, id := range []string{"urn:uuid:b-1", "urn:uuid:c-1", "urn:uuid:d-1"} {
		require.Equal(t, 1, seen[id], "%s must be yielded exactly once", id)
	}
	assertEveryObjectYielded(t, s3, run)

	var stamps []int64
	for _, e := range run {
		if entryID(e) == "urn:uuid:a-1" {
			stamps = append(stamps, createdAtUnix(t, e))
		}
	}
	require.Equal(t, []int64{base.Add(1 * time.Second).Unix(), base.Add(5 * time.Second).Unix()}, stamps,
		"once under the stamp it had when the run started, once under the stamp of the overwrite")
}

// Randomised: many same-second objects, random limit, uploads injected during the first
// run, with millisecond stamps (MinIO) and with whole-second stamps (AWS S3), where
// every object of a second ties on time and the key alone orders the page boundary.
// Within a run every object present when the run started is yielded exactly once; a
// second run started from (run start − overlap) yields every object at least once.
func TestSearch_Cursor_AtLeastOnceRandomised(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	styles := map[string]func(rng *rand.Rand) time.Duration{
		"millisecond stamps": func(rng *rand.Rand) time.Duration {
			return time.Duration(rng.Intn(40))*time.Second + time.Duration(rng.Intn(1000))*time.Millisecond
		},
		"whole-second stamps": func(rng *rand.Rand) time.Duration {
			return time.Duration(rng.Intn(40)) * time.Second
		},
	}
	for style, stamp := range styles {
		for seed := int64(1); seed <= 5; seed++ {
			t.Run(fmt.Sprintf("%s/seed-%d", style, seed), func(t *testing.T) {
				rng := rand.New(rand.NewSource(seed))
				s3 := newFakeS3(1000)
				seeded := make([]string, 0, 300)
				for i := 0; i < 300; i++ {
					key := fmt.Sprintf("urn:uuid:obj-%03d-1", i)
					seeded = append(seeded, key)
					s3.putWithStats(key, base.Add(stamp(rng)))
				}
				limit := 1 + rng.Intn(12)
				landed := map[string]int{}
				s3.beforeListing = injectLateUploads(s3, rng, base, landed)
				svc := newSvc(t, s3)

				run1, _, cursors := iterateCursor(t, svc, s3, base.Unix()-1, limit)
				assertExactlyOnce(t, run1)
				seen := idSet(run1)
				for _, key := range seeded {
					require.Equal(t, 1, seen[key], "%s was listable when the run started and must be yielded exactly once", key)
				}
				// An upload that lands during the run is yielded by that run exactly when it
				// sorts after the cursor of the page built right after it landed — it then
				// stays ahead of every later cursor until it is consumed — and never when it
				// lands behind that cursor. Listing L builds page L, fetched with cursors[L-1].
				for key, listing := range landed {
					want := 0
					if pairAfterCursor(t, *cursors[listing-1], s3.objects[key].lastModified, key) {
						want = 1
					}
					require.Equal(t, want, seen[key], "%s landed before listing %d", key, listing)
				}

				s3.beforeListing = nil
				// Run 1 conceptually started right after the seeded objects existed (base+40 s);
				// the injected uploads have LastModified between +30 s and +44 s, so an overlap
				// of 20 s covers all of them — and leaves run 2 blind to everything at or before
				// +20 s, so the union assertion can only pass if run 1 delivered those itself.
				runStart := base.Add(40 * time.Second)
				run2, _, _ := iterateCursor(t, svc, s3, runStart.Unix()-20, limit)
				assertNoEntryAtOrBefore(t, run2, base.Add(20*time.Second).Unix())
				assertEveryObjectYielded(t, s3, append(run1, run2...))
			})
		}
	}
}

// The limit is hard in cursor mode: exactly `limit` entries while candidates remain,
// fewer on the last page, and Next present exactly while candidates remain.
func TestSearch_Cursor_HardLimitAndNext(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	for i, key := range []string{"urn:uuid:a-1", "urn:uuid:b-1", "urn:uuid:c-1", "urn:uuid:d-1", "urn:uuid:e-1"} {
		s3.putWithStats(key, base.Add(time.Duration(i+1)*time.Second))
	}
	svc := newSvc(t, s3)

	opening := searchAfter(t, svc, base.Unix(), 2)
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1"}, ids(opening.Entries))
	require.NotNil(t, opening.Next)

	second := searchCursor(t, svc, *opening.Next, 2)
	require.Equal(t, []string{"urn:uuid:c-1", "urn:uuid:d-1"}, ids(second.Entries))
	require.NotNil(t, second.Next, "e remains")

	last := searchCursor(t, svc, *second.Next, 2)
	require.Equal(t, []string{"urn:uuid:e-1"}, ids(last.Entries))
	require.Nil(t, last.Next, "fewer than limit: last page")
}

// Next is absent when the page fills on the very last candidate — a page of exactly
// `limit` entries can be the last one, which is why Next, not the page size, says
// whether more remains.
func TestSearch_Cursor_NextAbsentWhenPageFillsAtLastCandidate(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(2*time.Second))
	svc := newSvc(t, s3)

	page := searchCursor(t, svc, service.NewCursor(base, "urn:uuid:0"), 2)
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1"}, ids(page.Entries))
	require.Nil(t, page.Next)

	empty := searchCursor(t, svc, service.NewCursor(base.Add(2*time.Second), "urn:uuid:b-1"), 2)
	require.Equal(t, []service.SearchRes{}, empty.Entries, "empty, non-nil slice encodes as []")
	require.Nil(t, empty.Next)
}

// AWS S3 lists whole seconds, so every object of a busy second ties on LastModified and
// the key alone draws the page boundary. Continuing from a cursor inside such a second
// must yield the rest of that second — the store pre-filter must not drop the cursor's
// own second — and never the cursor's object or anything before it.
func TestSearch_Cursor_SameSecondWholeSecondStamps(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		s3.putWithStats("urn:uuid:"+k+"-1", base.Add(1*time.Second))
	}
	s3.putWithStats("urn:uuid:f-1", base.Add(2*time.Second))
	svc := newSvc(t, s3)

	page := searchCursor(t, svc, service.NewCursor(base.Add(1*time.Second), "urn:uuid:b-1"), 2)
	require.Equal(t, []string{"urn:uuid:c-1", "urn:uuid:d-1"}, ids(page.Entries))
	require.NotNil(t, page.Next)
	millis, key := decodeCursor(t, *page.Next)
	require.Equal(t, base.Add(1*time.Second).UnixMilli(), millis)
	require.Equal(t, "urn:uuid:d-1", key)

	page = searchCursor(t, svc, *page.Next, 2)
	require.Equal(t, []string{"urn:uuid:e-1", "urn:uuid:f-1"}, ids(page.Entries))
	require.Nil(t, page.Next)
}

// MinIO lists milliseconds. Candidates are the objects whose (LastModified ms, key) is
// strictly greater than the cursor pair: an earlier millisecond is out, the cursor's
// own object is out, the same millisecond with a greater key is in.
func TestSearch_Cursor_MillisecondStampsStrictlyGreater(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 1, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(100*time.Millisecond))
	s3.putWithStats("urn:uuid:b-1", base.Add(200*time.Millisecond))
	s3.putWithStats("urn:uuid:c-1", base.Add(200*time.Millisecond))
	s3.putWithStats("urn:uuid:d-1", base.Add(300*time.Millisecond))
	s3.putWithStats("urn:uuid:e-1", base.Add(200*time.Millisecond+700*time.Microsecond)) // same ms as b and c once truncated
	svc := newSvc(t, s3)

	cursor := service.NewCursor(base.Add(200*time.Millisecond), "urn:uuid:b-1")
	page := searchCursor(t, svc, cursor, 10)
	require.Equal(t, []string{"urn:uuid:c-1", "urn:uuid:e-1", "urn:uuid:d-1"}, ids(page.Entries),
		"same millisecond ordered by key (c, e), then the later millisecond (d)")
	for _, e := range page.Entries {
		key := entryID(e)
		require.True(t, pairAfterCursor(t, cursor, s3.objects[key].lastModified, key), "%s must sort strictly after the cursor", key)
	}
	require.Nil(t, page.Next)
}

// The cursor a page hands out is the LIST position of its last entry — the LIST
// LastModified at millisecond precision, not the HEAD Last-Modified, which is a whole
// second and can even disagree with LIST.
func TestSearch_Paged_NextIsListStampOfLastEntry(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(2*time.Second+340*time.Millisecond))
	s3.putWithStats("urn:uuid:c-1", base.Add(3*time.Second))
	s3.headTime["urn:uuid:b-1"] = base.Add(9 * time.Second) // HEAD disagrees with LIST
	svc := newSvc(t, s3)

	opening := searchAfter(t, svc, base.Unix(), 2)
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1"}, ids(opening.Entries))
	require.NotNil(t, opening.Next)
	require.Equal(t, service.NewCursor(base.Add(2*time.Second+340*time.Millisecond), "urn:uuid:b-1"), *opening.Next)

	continued := searchCursor(t, svc, *opening.Next, 2)
	require.Equal(t, []string{"urn:uuid:c-1"}, ids(continued.Entries))
	require.Nil(t, continued.Next)
}

// The opening page (after+limit) says whether more remains exactly like a continued
// page: Next is present when unexamined candidates remain, absent when the page consumed
// everything — even when it filled exactly at the end of its second.
func TestSearch_Paged_NextAbsentWhenNothingRemains(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(1*time.Second))
	svc := newSvc(t, s3)

	require.Nil(t, searchAfter(t, svc, base.Unix(), 5).Next, "short page")
	require.Nil(t, searchAfter(t, svc, base.Unix(), 2).Next, "page filled on the last candidate")
	require.Nil(t, searchAfter(t, svc, base.Add(time.Second).Unix(), 2).Next, "empty page")

	s3.putWithStats("urn:uuid:c-1", base.Add(2*time.Second))
	require.NotNil(t, searchAfter(t, svc, base.Unix(), 2).Next, "a candidate in the next second remains")
}

// Legacy mode has no pages, so it never hands out a cursor.
func TestSearch_Legacy_NeverHasNext(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	for i := 0; i < 5; i++ {
		s3.putWithStats(fmt.Sprintf("urn:uuid:%d-1", i), base.Add(time.Duration(i+1)*time.Second))
	}
	svc := newSvc(t, s3)

	page := searchAfter(t, svc, base.Unix(), 0)
	require.Len(t, page.Entries, 5)
	require.Nil(t, page.Next)
}

// A candidate the opening page examined and skipped inside its boundary second (a foreign
// key here) sorts after the last yielded entry, so the cursor re-examines it on the next
// page — skipped again, never yielded, never counted — and the run loses nothing.
func TestSearch_Cursor_SkippedTailOfBoundarySecondIsHarmless(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(1*time.Second))
	s3.putWithStats("zzznodash", base.Add(1*time.Second)) // foreign object, sorts last in the second
	s3.putWithStats("urn:uuid:c-1", base.Add(2*time.Second))
	svc := newSvc(t, s3)

	run, sizes, _ := iterateCursor(t, svc, s3, base.Unix(), 2)
	require.Equal(t, []int{2, 1}, sizes)
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1", "urn:uuid:c-1"}, ids(run))
}

// Candidates skipped in cursor mode — vanished between LIST and HEAD, a key without a
// '-', a key whose version suffix is not one this service writes — do not count toward
// the limit and do not fail the call. Only the vanished object costs a HEAD.
func TestSearch_Cursor_SkipsDoNotCount(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("README", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:gone-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:x-foo", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:a-1", base.Add(2*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(3*time.Second))
	s3.putWithStats("urn:uuid:c-1", base.Add(4*time.Second))
	s3.headErr["urn:uuid:gone-1"] = &types.NotFound{}
	svc := newSvc(t, s3)

	page := searchCursor(t, svc, service.NewCursor(base, "urn:uuid:0"), 2)
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1"}, ids(page.Entries), "skipped candidates do not count toward the limit")
	require.NotNil(t, page.Next, "c remains")
	require.Equal(t, 3, s3.headCallCount(), "gone, a and b are HEADed; README and x-foo are skipped before HEAD")
}

// A HEAD failure other than not-found still fails the call (the page would be wrong).
func TestSearch_Cursor_HeadErrorFailsCall(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.headErr["urn:uuid:a-1"] = errors.New("boom")
	svc := newSvc(t, s3)

	c := service.NewCursor(base, "urn:uuid:0")
	_, err := svc.Search(context.Background(), service.SearchRequest{Cursor: &c, Limit: 5})
	require.Error(t, err)
}

// A cursor without a page size is a programming error at the call site (the handler
// rejects the request before it gets here); the service refuses it rather than guessing.
func TestSearch_Cursor_WithoutLimitIsRejected(t *testing.T) {
	s3 := newFakeS3(1000)
	svc := newSvc(t, s3)

	c := service.NewCursor(time.Now(), "urn:uuid:a-1")
	_, err := svc.Search(context.Background(), service.SearchRequest{Cursor: &c})
	require.Error(t, err)
}

// The order pages are cut in and the order a cursor resumes in must agree down to the
// millisecond the cursor records. Two objects in one millisecond whose sub-millisecond
// order is the reverse of their key order: a page cut at full precision would yield b
// then a and hand out a cursor at a, from which b — greater by key — would be yielded a
// second time. No S3 listing carries sub-millisecond stamps; this pins the invariant the
// seam rests on.
func TestSearch_Cursor_SeamIgnoresSubMillisecondOrder(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 1, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:b-1", base.Add(200*time.Millisecond))
	s3.putWithStats("urn:uuid:a-1", base.Add(200*time.Millisecond+700*time.Microsecond))
	s3.putWithStats("urn:uuid:c-1", base.Add(2*time.Second))
	svc := newSvc(t, s3)

	run, sizes, _ := iterateCursor(t, svc, s3, base.Unix()-1, 1)
	require.Equal(t, []int{2, 1}, sizes, "the opening page completes the second; the cursor page holds the rest")
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1", "urn:uuid:c-1"}, ids(run), "key order within the millisecond, each object once")
}

// A foreign key is skipped with a warning that names it, so an operator can find the
// object that does not belong in the bucket; the page itself is unaffected.
func TestSearch_Cursor_ForeignKeyIsSkippedWithWarningLog(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("README", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:a-1", base.Add(2*time.Second))
	svc := newSvc(t, s3)

	page := searchCursor(t, svc, service.NewCursor(base, "urn:uuid:0"), 5)
	require.Equal(t, []string{"urn:uuid:a-1"}, ids(page.Entries))
	require.Contains(t, logs.String(), `"level":"WARN"`)
	require.Contains(t, logs.String(), `"key":"README"`)
	require.Contains(t, logs.String(), "Skipping from paged result set")
}

func ids(entries []service.SearchRes) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryID(e))
	}
	return out
}
