package goalwatch

import (
	"testing"

	"github.com/0xjuanma/golazo/internal/api"
)

// fakeNotifier is a replaceable notify.Notifier that records every call in
// order, so tests can assert exactly-once Goal/GoalRevoked delivery without
// touching the OS notification stack.
type fakeNotifier struct {
	goals       []api.MatchEvent
	revocations []api.MatchEvent
}

func (f *fakeNotifier) Goal(event api.MatchEvent, _, _ api.Team, _, _ int) error {
	f.goals = append(f.goals, event)
	return nil
}

func (f *fakeNotifier) GoalRevoked(event api.MatchEvent, _, _ api.Team, _, _ int) error {
	f.revocations = append(f.revocations, event)
	return nil
}

func (f *fakeNotifier) goalIDs() []int {
	ids := make([]int, len(f.goals))
	for i, ev := range f.goals {
		ids[i] = ev.ID
	}
	return ids
}

func (f *fakeNotifier) revocationIDs() []int {
	ids := make([]int, len(f.revocations))
	for i, ev := range f.revocations {
		ids[i] = ev.ID
	}
	return ids
}

var (
	homeTeam = api.Team{ID: 10, Name: "Home FC", ShortName: "HOM"}
	awayTeam = api.Team{ID: 20, Name: "Away FC", ShortName: "AWY"}
)

func goalEvent(id, minute int, team api.Team, scorer string) api.MatchEvent {
	return api.MatchEvent{ID: id, Minute: minute, Type: "goal", Team: team, Player: &scorer}
}

func cardEvent(id, minute int, team api.Team) api.MatchEvent {
	return api.MatchEvent{ID: id, Minute: minute, Type: "card", Team: team}
}

// snapshot builds a live-match snapshot with the given scoreboard and events.
func snapshot(matchID, homeScore, awayScore int, events ...api.MatchEvent) *api.MatchDetails {
	h, a := homeScore, awayScore
	return &api.MatchDetails{
		Match: api.Match{
			ID:        matchID,
			HomeTeam:  homeTeam,
			AwayTeam:  awayTeam,
			HomeScore: &h,
			AwayScore: &a,
			Status:    api.MatchStatusLive,
		},
		Events: events,
	}
}

func assertInts(t *testing.T, label string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}
}

// TestBaselineZeroZeroArmsFirstGoal reproduces the original bug: an opener
// observed as 0-0 with an empty event list must still establish a baseline,
// so the first goal arriving on a later poll is announced exactly once.
func TestBaselineZeroZeroArmsFirstGoal(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 23, homeTeam, "Striker")))

	assertInts(t, "goal alerts", fake.goalIDs(), []int{101})
	assertInts(t, "revocations", fake.revocationIDs(), nil)
}

// TestBaselineWithExistingGoalsIsSilent ensures goals already on the board
// when the match is first loaded are treated as history, not announced.
func TestBaselineWithExistingGoalsIsSilent(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 1, 0,
		cardEvent(90, 10, awayTeam),
		goalEvent(101, 12, homeTeam, "Striker"),
	))
	assertInts(t, "baseline alerts", fake.goalIDs(), nil)

	tr.Observe(snapshot(1, 2, 0,
		goalEvent(101, 12, homeTeam, "Striker"),
		goalEvent(102, 41, homeTeam, "Winger"),
	))
	assertInts(t, "follow-up alerts", fake.goalIDs(), []int{102})
}

// TestMultipleGoalsInOnePoll confirms every goal landing between two polls
// is announced individually and in chronological minute order, even when
// the feed lists them out of order.
func TestMultipleGoalsInOnePoll(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	// Feed delivers the later goal first; notifications must reorder to 70', 80'.
	tr.Observe(snapshot(1, 2, 1,
		goalEvent(102, 80, homeTeam, "Winger"),
		goalEvent(201, 75, awayTeam, "Away Striker"),
		goalEvent(101, 70, homeTeam, "Striker"),
	))

	assertInts(t, "goal alerts", fake.goalIDs(), []int{101, 201, 102})
}

// TestStableOrderForEqualMinutes checks the tie-breaker: equal-minute events
// keep their original feed order instead of being shuffled.
func TestStableOrderForEqualMinutes(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 2, 0,
		goalEvent(102, 60, homeTeam, "Second"),
		goalEvent(101, 60, homeTeam, "First"),
	))

	assertInts(t, "goal alerts", fake.goalIDs(), []int{102, 101})
}

// TestScoreFirstEventLater retains the scoreboard increase as a pending
// confirmation and announces it once the goal event catches up.
func TestScoreFirstEventLater(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))

	// Scoreboard moves first, event list has not caught up yet.
	tr.Observe(snapshot(1, 1, 0))
	assertInts(t, "alerts before event arrives", fake.goalIDs(), nil)
	if got := tr.matches[1].pendingHome; got != 1 {
		t.Fatalf("pendingHome = %d, want 1", got)
	}

	// Event arrives on a later poll while the score stays 1-0.
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 30, homeTeam, "Striker")))
	assertInts(t, "alerts once event arrives", fake.goalIDs(), []int{101})
	if got := tr.matches[1].pendingHome; got != 0 {
		t.Fatalf("pendingHome = %d, want 0 after confirmation", got)
	}

	// Replaying the same snapshot must not notify a second time.
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 30, homeTeam, "Striker")))
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 30, homeTeam, "Striker")))
	assertInts(t, "alerts after replay", fake.goalIDs(), []int{101})
}

// TestMultiGoalScoreFirstThenSplitEvents covers a two-goal scoreboard jump
// with no events, confirmed by events arriving on separate later polls.
func TestMultiGoalScoreFirstThenSplitEvents(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 2, 0)) // two pending home goals
	if got := tr.matches[1].pendingHome; got != 2 {
		t.Fatalf("pendingHome = %d, want 2", got)
	}

	tr.Observe(snapshot(1, 2, 0, goalEvent(101, 30, homeTeam, "Striker")))
	assertInts(t, "first confirmation", fake.goalIDs(), []int{101})
	if got := tr.matches[1].pendingHome; got != 1 {
		t.Fatalf("pendingHome = %d, want 1", got)
	}

	tr.Observe(snapshot(1, 2, 0,
		goalEvent(101, 30, homeTeam, "Striker"),
		goalEvent(102, 55, homeTeam, "Winger"),
	))
	assertInts(t, "both confirmations", fake.goalIDs(), []int{101, 102})
	if got := tr.matches[1].pendingHome; got != 0 {
		t.Fatalf("pendingHome = %d, want 0", got)
	}
}

// TestDuplicateSnapshotIsIdempotent feeds identical snapshots repeatedly.
func TestDuplicateSnapshotIsIdempotent(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	snap := snapshot(1, 1, 0, goalEvent(101, 20, homeTeam, "Striker"))
	for i := 0; i < 4; i++ {
		tr.Observe(snap)
	}
	assertInts(t, "goal alerts", fake.goalIDs(), []int{101})
	assertInts(t, "revocations", fake.revocationIDs(), nil)
}

// TestRevocationSendsSingleCorrection exercises a VAR disallow where the
// score drops and the event vanishes: exactly one correction is sent, and
// repeated revoked snapshots never produce a second one.
func TestRevocationSendsSingleCorrection(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 40, homeTeam, "Striker")))
	tr.Observe(snapshot(1, 0, 0)) // revoked: score back to 0-0, event removed

	assertInts(t, "goal alerts", fake.goalIDs(), []int{101})
	assertInts(t, "corrections", fake.revocationIDs(), []int{101})

	tr.Observe(snapshot(1, 0, 0)) // duplicate revoked snapshot
	assertInts(t, "corrections stay idempotent", fake.revocationIDs(), []int{101})
}

// TestRestoreDoesNotRealert verifies that a goal restored after review is
// recognized by its event ID: no new Goal alert and no duplicate correction,
// while genuinely new goals afterwards still notify once.
func TestRestoreDoesNotRealert(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 40, homeTeam, "Striker")))
	tr.Observe(snapshot(1, 0, 0))                                          // VAR revokes
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 40, homeTeam, "Striker"))) // restored

	assertInts(t, "goal alerts after restore", fake.goalIDs(), []int{101})
	assertInts(t, "corrections after restore", fake.revocationIDs(), []int{101})
	if got := tr.matches[1].pendingHome; got != 0 {
		t.Fatalf("pendingHome = %d, want 0 after restore", got)
	}

	// A brand-new goal after the restore must still be announced once.
	tr.Observe(snapshot(1, 2, 0,
		goalEvent(101, 40, homeTeam, "Striker"),
		goalEvent(102, 70, homeTeam, "Winger"),
	))
	assertInts(t, "new goal after restore", fake.goalIDs(), []int{101, 102})
}

// TestPendingGoalRevokedBeforeEvent arrives: a score-first increase that is
// rolled back before any event was seen was never announced, so no
// correction may be emitted.
func TestPendingGoalRevokedBeforeEvent(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 1, 0)) // pending, no event
	tr.Observe(snapshot(1, 0, 0)) // rolled back

	assertInts(t, "goal alerts", fake.goalIDs(), nil)
	assertInts(t, "revocations", fake.revocationIDs(), nil)
	if got := tr.matches[1].pendingHome; got != 0 {
		t.Fatalf("pendingHome = %d, want 0", got)
	}

	// A later, real goal still works normally.
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 50, homeTeam, "Striker")))
	assertInts(t, "later real goal", fake.goalIDs(), []int{101})
}

// TestScoreDropWhileEventLags covers feeds where the scoreboard correction
// arrives before the event is removed: the most recent announced goal is
// corrected, and the delayed event removal does not correct it twice.
func TestScoreDropWhileEventLags(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 40, homeTeam, "Striker")))
	tr.Observe(snapshot(1, 0, 0, goalEvent(101, 40, homeTeam, "Striker"))) // score down, event lags

	assertInts(t, "corrections", fake.revocationIDs(), []int{101})

	tr.Observe(snapshot(1, 0, 0)) // event finally removed
	assertInts(t, "no second correction", fake.revocationIDs(), []int{101})
}

// TestAwayGoalsAndBothTeams ensures attribution works for the away side and
// for both teams scoring in the same poll.
func TestAwayGoalsAndBothTeams(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 0, 1, goalEvent(201, 15, awayTeam, "Away Striker")))
	assertInts(t, "away goal", fake.goalIDs(), []int{201})

	tr.Observe(snapshot(1, 1, 2,
		goalEvent(201, 15, awayTeam, "Away Striker"),
		goalEvent(101, 30, homeTeam, "Home Striker"),
		goalEvent(202, 35, awayTeam, "Away Winger"),
	))
	assertInts(t, "both teams", fake.goalIDs(), []int{201, 101, 202})
}

// TestMatchSwitchingKeepsIndependentState interleaves two matches: switching
// away and back must neither cross-notify nor replay historical goals, and
// re-establishing a baseline on reload stays silent.
func TestMatchSwitchingKeepsIndependentState(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	// Open match 1 at 0-0, then match 2 at 0-0.
	tr.Establish(snapshot(1, 0, 0))
	tr.Establish(snapshot(2, 0, 0))

	// Goal in match 1.
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 20, homeTeam, "M1 Striker")))
	assertInts(t, "match 1 goal", fake.goalIDs(), []int{101})

	// User switches away and back: match 1 reloads with the goal already on
	// the board. Re-establishing the baseline must not replay it.
	tr.Establish(snapshot(1, 1, 0, goalEvent(101, 20, homeTeam, "M1 Striker")))
	assertInts(t, "no replay after reload", fake.goalIDs(), []int{101})

	// Match 2 scores independently.
	tr.Observe(snapshot(2, 0, 1, goalEvent(201, 25, awayTeam, "M2 Striker")))
	assertInts(t, "match 2 goal", fake.goalIDs(), []int{101, 201})

	// Back to match 1: a fresh goal there is still detected.
	tr.Observe(snapshot(1, 2, 0,
		goalEvent(101, 20, homeTeam, "M1 Striker"),
		goalEvent(102, 60, homeTeam, "M1 Winger"),
	))
	assertInts(t, "match 1 second goal", fake.goalIDs(), []int{101, 201, 102})

	// The two per-match states are independent.
	if tr.matches[1] == tr.matches[2] {
		t.Fatal("match states must be tracked independently per match ID")
	}
	assertInts(t, "no corrections throughout", fake.revocationIDs(), nil)
}

// TestReplayAfterPollReissue delivers the same payload several polls in a
// row, including non-goal events, and expects a single alert overall.
func TestReplayAfterPollReissue(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	snap := snapshot(1, 1, 1,
		cardEvent(90, 11, homeTeam),
		goalEvent(101, 20, homeTeam, "Striker"),
		goalEvent(201, 22, awayTeam, "Away Striker"),
	)
	for i := 0; i < 3; i++ {
		tr.Observe(snap)
	}
	assertInts(t, "goal alerts", fake.goalIDs(), []int{101, 201})
	assertInts(t, "revocations", fake.revocationIDs(), nil)
}

// TestFlatScoreWithReplacedEvent documents the conservative rule: a missing
// announced event whose scoreboard value did not change does not trigger a
// correction; the replacement goal is still announced once.
func TestFlatScoreWithReplacedEvent(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(snapshot(1, 0, 0))
	tr.Observe(snapshot(1, 1, 0, goalEvent(101, 40, homeTeam, "Striker")))

	// Same 1-0 score: old event gone, replacement event present.
	tr.Observe(snapshot(1, 1, 0, goalEvent(102, 41, homeTeam, "Replacement")))
	assertInts(t, "replacement goal announced", fake.goalIDs(), []int{101, 102})
	assertInts(t, "no correction without score drop", fake.revocationIDs(), nil)
}

// TestObserveWithoutBaselineEstablishesOne guards the defensive path where a
// poll snapshot arrives before any explicit Establish.
func TestObserveWithoutBaselineEstablishesOne(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Observe(snapshot(1, 2, 1,
		goalEvent(101, 10, homeTeam, "Striker"),
		goalEvent(201, 20, awayTeam, "Away Striker"),
		goalEvent(102, 30, homeTeam, "Winger"),
	))
	assertInts(t, "first snapshot is a silent baseline", fake.goalIDs(), nil)

	tr.Observe(snapshot(1, 3, 1,
		goalEvent(101, 10, homeTeam, "Striker"),
		goalEvent(201, 20, awayTeam, "Away Striker"),
		goalEvent(102, 30, homeTeam, "Winger"),
		goalEvent(103, 70, homeTeam, "Sub"),
	))
	assertInts(t, "later goal detected", fake.goalIDs(), []int{103})
}

// TestNilSnapshotsAreIgnored ensures defensive nil handling never panics.
func TestNilSnapshotsAreIgnored(t *testing.T) {
	fake := &fakeNotifier{}
	tr := New(fake)

	tr.Establish(nil)
	tr.Observe(nil)
	tr.Reset(1)
	if len(tr.matches) != 0 {
		t.Fatalf("Reset should drop match state, got %d matches", len(tr.matches))
	}
	assertInts(t, "no alerts", fake.goalIDs(), nil)
	assertInts(t, "no corrections", fake.revocationIDs(), nil)
}
