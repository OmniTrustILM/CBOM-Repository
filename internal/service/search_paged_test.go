package service_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/OmniTrustILM/cbom-repository/internal/service"
	"github.com/OmniTrustILM/cbom-repository/internal/store"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/require"
)

func newSvc(t *testing.T, s3 *fakeS3) service.Service {
	t.Helper()
	svc, err := service.New(store.New(store.Config{Bucket: "bucket"}, s3, nil), service.Config{})
	require.NoError(t, err)
	return svc
}

func entryID(e service.SearchRes) string { return e.SerialNumber + "-" + e.Version }

func createdAtUnix(t *testing.T, e service.SearchRes) int64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, e.Timestamp)
	require.NoError(t, err)
	return ts.Unix()
}

// iterate runs the documented client protocol once: fetch pages of `limit`, advance
// `after` to the last entry's created_at, stop at the first page shorter than `limit`.
// It returns every entry yielded and the page sizes, and asserts that consecutive pages
// never share a second (the server never splits a second).
func iterate(t *testing.T, svc service.Service, after int64, limit int) ([]service.SearchRes, []int) {
	t.Helper()
	var all []service.SearchRes
	var sizes []int
	for {
		page, err := svc.Search(context.Background(), after, limit)
		require.NoError(t, err)
		sizes = append(sizes, len(page))
		for i := 1; i < len(page); i++ {
			require.LessOrEqual(t, createdAtUnix(t, page[i-1]), createdAtUnix(t, page[i]), "page must be sorted by created_at")
		}
		if len(page) > 0 {
			require.Greater(t, createdAtUnix(t, page[0]), after, "entries must be strictly after `after`")
		}
		all = append(all, page...)
		if len(page) < limit {
			return all, sizes
		}
		after = createdAtUnix(t, page[len(page)-1])
	}
}

func idSet(entries []service.SearchRes) map[string]int {
	set := map[string]int{}
	for _, e := range entries {
		set[entryID(e)]++
	}
	return set
}

// assertExactlyOnce fails the test if any entry in `entries` was yielded more than once
// — within a single iteration of the documented protocol, every object must be yielded
// exactly once.
func assertExactlyOnce(t *testing.T, entries []service.SearchRes) {
	t.Helper()
	for id, n := range idSet(entries) {
		require.Equal(t, 1, n, "%s yielded %d times within one iteration", id, n)
	}
}

// assertEveryObjectYielded fails the test if any object currently in `s3` is missing
// from `entries` — used to confirm at-least-once delivery across one or more runs.
func assertEveryObjectYielded(t *testing.T, s3 *fakeS3, entries []service.SearchRes) {
	t.Helper()
	seen := idSet(entries)
	for key := range s3.objects {
		require.GreaterOrEqual(t, seen[key], 1, "missing %s", key)
	}
}

// assertNoEntryAtOrBefore fails the test if any entry's created_at second is at or
// before `ts`. Applied to a later run, it proves that run could not have delivered the
// objects of those seconds — which is what gives a union assertion across two runs its
// teeth: without it, a run whose `after` predates every object trivially covers them.
func assertNoEntryAtOrBefore(t *testing.T, entries []service.SearchRes, ts int64) {
	t.Helper()
	for _, e := range entries {
		require.Greater(t, createdAtUnix(t, e), ts, "%s cannot come from a run started at %d", entryID(e), ts)
	}
}

// injectLateUploads returns a fakeS3.beforeListing hook that, for listings 2 through 5,
// seeds three more objects landing between +30s and +44s of base — simulating uploads
// that complete while a run is already in progress.
func injectLateUploads(s3 *fakeS3, rng *rand.Rand, base time.Time) func(listing int) {
	return func(listing int) {
		if listing > 1 && listing < 6 {
			for j := 0; j < 3; j++ {
				s3.putWithStats(fmt.Sprintf("urn:uuid:late-%02d-%d-1", listing, j), base.Add(time.Duration(30+rng.Intn(15))*time.Second))
			}
		}
	}
}

// Deterministic walk through the soft-limit rules with concurrent uploads landing
// between pages. Seconds after base: 5 objects at +0, 3 at +1, 1 at +2, 8 at +3
// (some with millisecond offsets, as MinIO reports them), 8 at +10. limit = 4.
func TestSearch_Paged_ProtocolWithConcurrentUploads(t *testing.T) {
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

	// Two uploads land while the run is in progress. Before listing 2, one into a second
	// still ahead of the cursor (+7 s) — the run will see it. Before listing 4, one into
	// second +3, which page 3 has just finished (a multipart upload initiated at +3.9 s
	// that completed after page 3 was built): its second is behind the cursor by then, so
	// this run can never see it — only the next run, thanks to the overlap.
	s3.beforeListing = func(listing int) {
		switch listing {
		case 2:
			s3.putWithStats("urn:uuid:late-later-1", base.Add(7*time.Second))
		case 4:
			s3.putWithStats("urn:uuid:late-boundary-1", base.Add(3900*time.Millisecond))
		}
	}
	svc := newSvc(t, s3)

	run1, sizes := iterate(t, svc, base.Unix()-1, 4)
	// page 1: limit hit at the 4th object of second +0 → the second is completed → 5
	// page 2: 3 (+1) + 1 (+2) = 4; next candidate is +3 → stop at exactly 4
	// page 3: second +3 has 8 → 8
	// page 4: late-later (+7) + 3 of +10 reach the limit → second +10 completed → 9;
	//         late-boundary, injected just before this listing, sits in second +3 and
	//         the cursor is already at +3, so it is not a candidate and the sizes are
	//         the same as they would be without it
	// page 5: empty → done
	require.Equal(t, []int{5, 4, 8, 9, 0}, sizes)

	// Paged mode must not HEAD the whole bucket on every page: it HEADs exactly the
	// candidates it walks (here, one per yielded entry, since nothing vanishes in this
	// scenario), not every object in the bucket on every one of the 5 listings.
	headCallsInRun1 := s3.headCallCount()
	require.Equal(t, len(run1), headCallsInRun1, "every HEAD call in run 1 corresponds to a yielded entry (this scenario has no vanished objects)")
	require.Less(t, headCallsInRun1, len(sizes)*len(s3.objects), "paged mode must not HEAD the whole bucket on every listing")

	seen := idSet(run1)
	for key := range seed {
		require.Equal(t, 1, seen[key], "seeded object %s must be yielded exactly once within one iteration", key)
	}
	require.Equal(t, 1, seen["urn:uuid:late-later-1"], "an upload into a later second is yielded by the same iteration")
	require.Equal(t, 0, seen["urn:uuid:late-boundary-1"], "an upload into an already-passed second is deferred to the next iteration")

	// The pitfall the documented protocol avoids: a consumer that advanced its watermark
	// from the last created_at (minus a small overlap) would never see the late upload,
	// because it landed in a second the run had already passed.
	last := createdAtUnix(t, run1[len(run1)-1])
	naive, _ := iterate(t, svc, last-1, 4)
	require.Equal(t, 0, idSet(naive)["urn:uuid:late-boundary-1"], "advancing from the last created_at loses uploads that landed behind the cursor")

	// The documented rule: the next run starts from (run start − overlap). Run 1
	// conceptually started at base+10.5 s (after every seeded object existed) and the
	// overlap must cover the longest upload duration — 8 s here, so run 2 starts from
	// `after` = base+2 and picks the late upload up.
	//
	// That `after` sits in the middle of the data on purpose: run 2 cannot see anything
	// in seconds +0..+2, so the union assertion below only holds if run 1 really
	// delivered those objects.
	runStart := base.Add(10500 * time.Millisecond)
	run2, _ := iterate(t, svc, runStart.Unix()-8, 4)
	require.Equal(t, 1, idSet(run2)["urn:uuid:late-boundary-1"])
	assertNoEntryAtOrBefore(t, run2, base.Add(2*time.Second).Unix())
	assertEveryObjectYielded(t, s3, append(run1, run2...))
}

// An object overwritten in place while a run is in progress moves to a new second and is
// yielded again under that stamp — at-least-once delivery, never a skip. The run must
// still terminate: the object is delivered under each stamp exactly once, not chased
// forever.
func TestSearch_Paged_OverwriteDuringRunYieldsTwice(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	for i, key := range []string{"urn:uuid:a-1", "urn:uuid:b-1", "urn:uuid:c-1", "urn:uuid:d-1"} {
		s3.putWithStats(key, base.Add(time.Duration(i+1)*time.Second))
	}
	// Page 1 yields `a` at +1 and the cursor moves to +1. Before listing 2, `a` is
	// overwritten and its LIST and HEAD clocks both move to +5 — ahead of the cursor,
	// so the same object becomes a candidate again.
	s3.beforeListing = func(listing int) {
		if listing == 2 {
			s3.overwrite("urn:uuid:a-1", base.Add(5*time.Second))
		}
	}
	svc := newSvc(t, s3)

	run, sizes := iterate(t, svc, base.Unix(), 1)
	require.Equal(t, []int{1, 1, 1, 1, 1, 0}, sizes, "one entry per page, then an empty page ends the run")

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

// Randomised: many same-second objects, random limit, uploads injected during the
// first iteration. Within an iteration every object present when its page was built
// is yielded exactly once; a second iteration started from (run start − overlap)
// yields every object at least once.
func TestSearch_Paged_AtLeastOnceRandomised(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			s3 := newFakeS3(1000)
			for i := 0; i < 300; i++ {
				s3.putWithStats(fmt.Sprintf("urn:uuid:obj-%03d-1", i), base.Add(time.Duration(rng.Intn(40))*time.Second+time.Duration(rng.Intn(1000))*time.Millisecond))
			}
			limit := 1 + rng.Intn(12)
			s3.beforeListing = injectLateUploads(s3, rng, base)
			svc := newSvc(t, s3)

			run1, _ := iterate(t, svc, base.Unix()-1, limit)
			assertExactlyOnce(t, run1)
			s3.beforeListing = nil
			// Run 1 conceptually started right after the seeded objects existed (base+40 s);
			// the injected uploads have LastModified between +30 s and +44 s, so an overlap
			// of 20 s from the run start covers all of them — and leaves run 2 blind to
			// everything at or before +20 s, so the union assertion below can only pass if
			// run 1 delivered those objects itself.
			runStart := base.Add(40 * time.Second)
			run2, _ := iterate(t, svc, runStart.Unix()-20, limit)
			assertNoEntryAtOrBefore(t, run2, base.Add(20*time.Second).Unix())
			assertEveryObjectYielded(t, s3, append(run1, run2...))
		})
	}
}

// The boundary is evaluated at second granularity: an object whose LastModified is
// `after` plus 400 ms belongs to the `after` second and is excluded in paged mode (the
// legacy nanosecond compare would include it — that is what makes a consumer advancing
// by whole seconds safe on MinIO as well as on AWS).
func TestSearch_Paged_SecondGranularityBoundary(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:same-second-1", base.Add(400*time.Millisecond))
	s3.putWithStats("urn:uuid:next-second-1", base.Add(1*time.Second))
	svc := newSvc(t, s3)

	paged, err := svc.Search(context.Background(), base.Unix(), 10)
	require.NoError(t, err)
	require.Len(t, paged, 1)
	require.Equal(t, "urn:uuid:next-second", paged[0].SerialNumber)

	legacy, err := svc.Search(context.Background(), base.Unix(), 0)
	require.NoError(t, err)
	require.Len(t, legacy, 2, "legacy mode keeps the nanosecond compare")
}

func TestSearch_Paged_SortedByLastModifiedThenKey(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:c-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:a-1", base.Add(2*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:d-original", base.Add(1*time.Second))
	svc := newSvc(t, s3)

	res, err := svc.Search(context.Background(), base.Unix(), 10)
	require.NoError(t, err)
	ids := make([]string, 0, len(res))
	for _, e := range res {
		ids = append(ids, entryID(e))
	}
	require.Equal(t, []string{"urn:uuid:b-1", "urn:uuid:c-1", "urn:uuid:d-original", "urn:uuid:a-1"}, ids)
}

// Vanished objects (deleted between LIST and HEAD) and foreign keys do not count
// toward the limit and do not fail the paged call.
func TestSearch_Paged_SkipsVanishedAndForeignKeys(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("README", base.Add(1*time.Second)) // foreign object, no '-' in the key
	s3.putWithStats("urn:uuid:gone-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:a-1", base.Add(2*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(3*time.Second))
	// LIST still returns `gone`, but HEAD reports it deleted in the meantime.
	s3.headErr["urn:uuid:gone-1"] = &types.NotFound{}
	svc := newSvc(t, s3)

	res, err := svc.Search(context.Background(), base.Unix(), 2)
	require.NoError(t, err)
	require.Len(t, res, 2, "skipped objects do not count toward the limit")
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1"}, []string{entryID(res[0]), entryID(res[1])})
}

// A skipped candidate (vanished or foreign-key) that sorts INSIDE the boundary second,
// after the page has already filled, must not clobber the boundary or count toward the
// limit: the page must still stop at the end of that second, and must not reopen past
// it.
func TestSearch_Paged_SkipInsideBoundarySecondDoesNotClobberBoundary(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(5*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(5*time.Second))
	// Both sort after b-1 within the +5s second (see TestSearch_Paged_SortedByLastModifiedThenKey
	// for the sort rule): "gone" > "b" in the urn:uuid: namespace, and the dash-less
	// foreign key sorts last of all because it starts with 'z'.
	s3.putWithStats("urn:uuid:gone-1", base.Add(5*time.Second))
	s3.putWithStats("zzznodash", base.Add(5*time.Second)) // foreign object, no '-' in the key
	s3.putWithStats("urn:uuid:d-1", base.Add(6*time.Second))
	s3.headErr["urn:uuid:gone-1"] = &types.NotFound{}
	svc := newSvc(t, s3)

	res, err := svc.Search(context.Background(), base.Unix(), 2)
	require.NoError(t, err)
	require.Len(t, res, 2, "the vanished object and the foreign key inside the boundary second are skipped, not counted, and do not reopen the page into the +6s second")
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-1"}, []string{entryID(res[0]), entryID(res[1])})
}

// A HEAD failure other than not-found still fails the call (the page would be wrong).
func TestSearch_Paged_HeadErrorFailsCall(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.headErr["urn:uuid:a-1"] = errors.New("boom")
	svc := newSvc(t, s3)

	_, err := svc.Search(context.Background(), base.Unix(), 5)
	require.Error(t, err)
}

func TestSearch_Paged_EmptyAndExactLimit(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	svc := newSvc(t, s3)

	res, err := svc.Search(context.Background(), base.Unix(), 3)
	require.NoError(t, err)
	require.Equal(t, []service.SearchRes{}, res, "empty, non-nil slice encodes as []")

	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(2*time.Second))
	s3.putWithStats("urn:uuid:c-1", base.Add(3*time.Second))
	s3.putWithStats("urn:uuid:d-1", base.Add(4*time.Second))
	res, err = svc.Search(context.Background(), base.Unix(), 3)
	require.NoError(t, err)
	require.Len(t, res, 3, "limit reached exactly at a second boundary → no overshoot")
	require.Equal(t, "urn:uuid:c", res[2].SerialNumber)
}

// created_at must be derived from the same LIST LastModified that decides the page
// boundary, not from HEAD's: HEAD can report a different second — here simulating an
// unguarded overwrite landing between LIST and HEAD, or a store whose HTTP-date rounds
// differently from its listing. If created_at instead followed HEAD, a consumer
// advancing `after` to object a's created_at (+3 s, HEAD's view) would outrun the
// boundary (+1 s, LIST's view) and skip b and c on the next page.
func TestSearch_Paged_CreatedAtFollowsListingClock(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:a-1", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:b-1", base.Add(2*time.Second))
	s3.putWithStats("urn:uuid:c-1", base.Add(3*time.Second))
	s3.putWithStats("urn:uuid:d-1", base.Add(4*time.Second))
	s3.headTime["urn:uuid:a-1"] = base.Add(3 * time.Second)
	svc := newSvc(t, s3)

	run, _ := iterate(t, svc, base.Unix(), 1)
	seen := idSet(run)
	for _, id := range []string{"urn:uuid:a-1", "urn:uuid:b-1", "urn:uuid:c-1", "urn:uuid:d-1"} {
		require.Equal(t, 1, seen[id], "%s must be yielded exactly once in the run", id)
	}
	require.Equal(t, base.Add(1*time.Second).Unix(), createdAtUnix(t, run[0]), "created_at must follow the LIST second, not HEAD's")
}

// Paged mode is where warnings live: an object whose statistics cannot be read stays
// visible (with `cryptoStats` null and a code saying why), and an object whose
// statistics are readable but were counted shallowly or cut short by the depth bound is
// flagged without hiding the numbers. Codes come in a fixed order: missing|invalid
// first, then shallow, then truncated.
func TestSearch_Paged_StatsWarnings(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		metadata     map[string]string
		wantStats    bool
		wantWarnings []string
	}{
		"valid and versioned has no warnings": {
			metadata:  map[string]string{"version": "1", "crypto-stats": validStatsJSON, "crypto-stats-version": "2"},
			wantStats: true,
		},
		"missing statistics": {
			metadata:     map[string]string{"version": "1"},
			wantWarnings: []string{service.WarningCryptoStatsMissing},
		},
		"invalid statistics": {
			metadata:     map[string]string{"version": "1", "crypto-stats": "not json"},
			wantWarnings: []string{service.WarningCryptoStatsInvalid},
		},
		"valid statistics without a version key are a legacy shallow count": {
			metadata:     map[string]string{"version": "1", "crypto-stats": validStatsJSON},
			wantStats:    true,
			wantWarnings: []string{service.WarningCryptoStatsShallow},
		},
		"the depth bound fired at upload": {
			metadata:     map[string]string{"version": "1", "crypto-stats": validStatsJSON, "crypto-stats-version": "2", "crypto-stats-truncated": "true"},
			wantStats:    true,
			wantWarnings: []string{service.WarningCryptoStatsTruncated},
		},
		"shallow and truncated are reported in that order": {
			metadata:     map[string]string{"version": "1", "crypto-stats": validStatsJSON, "crypto-stats-truncated": "true"},
			wantStats:    true,
			wantWarnings: []string{service.WarningCryptoStatsShallow, service.WarningCryptoStatsTruncated},
		},
		"a truncated marker other than true is not a warning": {
			metadata:     map[string]string{"version": "1", "crypto-stats": validStatsJSON, "crypto-stats-version": "2", "crypto-stats-truncated": "false"},
			wantStats:    true,
			wantWarnings: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s3 := newFakeS3(1000)
			s3.put("urn:uuid:a-1", base.Add(1*time.Second), tc.metadata)
			svc := newSvc(t, s3)

			res, err := svc.Search(context.Background(), base.Unix(), 10)
			require.NoError(t, err)
			require.Len(t, res, 1, "an object is never dropped from a page because of its statistics")
			require.Equal(t, tc.wantWarnings, res[0].Warnings)
			if tc.wantStats {
				require.NotNil(t, res[0].CryptoStats)
				require.Equal(t, 1, res[0].CryptoStats.CryptoAsset.Total)
				return
			}
			require.Nil(t, res[0].CryptoStats)
		})
	}
}

// One object with unreadable statistics must not cost the page its other entries, and
// must not fail the call the way legacy mode does.
func TestSearch_Paged_UnreadableStatsDoNotAffectOtherEntries(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.put("urn:uuid:a-1", base.Add(1*time.Second), map[string]string{"version": "1", "crypto-stats": "not json"})
	s3.putWithStats("urn:uuid:b-1", base.Add(2*time.Second))
	svc := newSvc(t, s3)

	res, err := svc.Search(context.Background(), base.Unix(), 10)
	require.NoError(t, err)
	require.Len(t, res, 2)
	require.Nil(t, res[0].CryptoStats)
	require.Equal(t, []string{service.WarningCryptoStatsInvalid}, res[0].Warnings)
	require.NotNil(t, res[1].CryptoStats)
	require.Empty(t, res[1].Warnings)
}

// A key whose version suffix is neither a positive integer nor "original" is not a BOM
// version this service wrote. Paged mode skips it with a warning — the same treatment as
// a key without a '-' — so a consumer's page never carries an entry it cannot fetch.
// Legacy mode keeps returning such keys, byte for byte as before.
func TestSearch_Paged_SkipsInvalidVersionSuffix(t *testing.T) {
	base := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s3 := newFakeS3(1000)
	s3.putWithStats("urn:uuid:x-foo", base.Add(1*time.Second))
	s3.putWithStats("urn:uuid:x-0", base.Add(2*time.Second))
	s3.putWithStats("urn:uuid:x-01", base.Add(3*time.Second))
	s3.putWithStats("urn:uuid:a-1", base.Add(4*time.Second))
	s3.putWithStats("urn:uuid:b-original", base.Add(5*time.Second))
	svc := newSvc(t, s3)

	paged, err := svc.Search(context.Background(), base.Unix(), 10)
	require.NoError(t, err)
	ids := make([]string, 0, len(paged))
	for _, e := range paged {
		ids = append(ids, entryID(e))
	}
	require.Equal(t, []string{"urn:uuid:a-1", "urn:uuid:b-original"}, ids)

	legacy, err := svc.Search(context.Background(), base.Unix(), 0)
	require.NoError(t, err)
	require.Len(t, legacy, 5, "legacy mode returns every key with a '-', whatever its suffix")
}

func TestValidVersion(t *testing.T) {
	for _, valid := range []string{"1", "2", "10", "999999", "original"} {
		require.True(t, service.ValidVersion(valid), valid)
	}
	for _, invalid := range []string{"", "0", "01", "-1", "1.0", "1 ", " 1", "foo", "Original", "original-1", "1e3", "١"} {
		require.False(t, service.ValidVersion(invalid), invalid)
	}
}
