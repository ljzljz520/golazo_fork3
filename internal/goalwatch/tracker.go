// Package goalwatch reconciles successive match snapshots into idempotent
// goal notifications.
//
// The FotMob feed does not deliver goals atomically: the scoreboard can
// move before the goal event is listed, several goals can land in a single
// poll, events can be replayed or re-ordered, and a VAR review can revoke
// a goal (and occasionally restore it). The Tracker keeps one state record
// per match ID and turns that noisy stream into exactly-once Goal and
// GoalRevoked notifications:
//
//   - The first valid snapshot of a match establishes the baseline, even
//     when the score is 0-0 and the event list is empty; the first goal of
//     the match is therefore always detected on a later poll instead of
//     being mistaken for "uninitialized" state.
//   - Goal events are identified by their event ID, so duplicate snapshots,
//     replays and out-of-order delivery never trigger a second alert.
//   - A scoreboard increase without a matching event is held as a pending
//     confirmation and announced once the event arrives.
//   - A scoreboard decrease revokes pending (never-announced) changes
//     silently and emits a single correction per previously announced
//     goal. A restored goal is recognized by its event ID and never
//     re-announced.
package goalwatch

import (
	"sort"

	"github.com/0xjuanma/golazo/internal/api"
	"github.com/0xjuanma/golazo/internal/notify"
)

// matchState holds the reconciliation state for a single match.
type matchState struct {
	initialized bool
	homeID      int
	awayID      int

	homeScore int
	awayScore int

	// known maps every goal event ID ever observed to its payload. Event
	// identity is the ID, so re-ordered or replayed events are recognized
	// instead of being re-announced.
	known map[int]api.MatchEvent

	// present flags the goal event IDs contained in the latest snapshot.
	// It is rebuilt on every Observe; an ID flipping to absent means the
	// event vanished from the feed.
	present map[int]bool

	// alerted flags IDs for which a Goal notification has been sent.
	alerted map[int]bool

	// corrected flags IDs for which a GoalRevoked correction has been
	// sent, guaranteeing at most one correction per disallowed goal.
	corrected map[int]bool

	// pendingHome/pendingAway count scoreboard increases that are not yet
	// backed by a goal event in the feed ("score first, event later").
	pendingHome int
	pendingAway int
}

// Tracker reconciles poll snapshots per match and emits notifications
// through a replaceable notify.Notifier.
type Tracker struct {
	notifier notify.Notifier
	matches  map[int]*matchState
}

// New creates a Tracker backed by the given notifier. A fake notifier can
// be injected in tests to verify every notification exactly once.
func New(notifier notify.Notifier) *Tracker {
	return &Tracker{
		notifier: notifier,
		matches:  make(map[int]*matchState),
	}
}

// Reset drops all tracked state for a match. The next Observe for that
// match establishes a fresh baseline.
func (t *Tracker) Reset(matchID int) {
	delete(t.matches, matchID)
}

// Establish records the first valid snapshot of a match as the baseline.
// No notifications are emitted: goals already present are treated as
// history. Re-establishing a baseline (for example when the user switches
// back to a match) preserves the alerted/corrected history so a restored
// goal can never be re-announced, while re-baselining the scoreboard and
// discarding stale pending confirmations.
func (t *Tracker) Establish(d *api.MatchDetails) {
	if d == nil {
		return
	}
	st := t.stateFor(d.ID)
	st.initialized = true
	st.homeID = d.HomeTeam.ID
	st.awayID = d.AwayTeam.ID
	st.homeScore, st.awayScore = scoreOf(d)
	if st.known == nil {
		st.known = make(map[int]api.MatchEvent)
	}
	if st.alerted == nil {
		st.alerted = make(map[int]bool)
	}
	if st.corrected == nil {
		st.corrected = make(map[int]bool)
	}
	st.present = make(map[int]bool)
	for _, ev := range goalEvents(d) {
		if _, ok := st.known[ev.ID]; !ok {
			st.known[ev.ID] = ev
		}
		st.present[ev.ID] = true
	}
	st.pendingHome = 0
	st.pendingAway = 0
}

// Observe reconciles a new poll snapshot against the tracked baseline and
// emits idempotent notifications. When no baseline exists it behaves like
// Establish, so a 0-0 opener with an empty event list still arms first-goal
// detection for every subsequent poll.
func (t *Tracker) Observe(d *api.MatchDetails) {
	if d == nil {
		return
	}
	st := t.stateFor(d.ID)
	if !st.initialized {
		t.Establish(d)
		return
	}

	homeScore, awayScore := scoreOf(d)
	goals := goalEvents(d)

	oldPresent := st.present
	currentPresent := make(map[int]bool, len(goals))
	newHome, newAway := 0, 0
	reappearedHome, reappearedAway := 0, 0

	// 1. Event-driven reconciliation. An event ID never seen before is a
	// newly confirmed goal; chronological ordering keeps notifications in
	// minute order even when the feed delivers events out of sequence.
	// Known IDs (duplicate snapshot, replay or post-revocation restore)
	// never produce another Goal notification.
	for _, ev := range goals {
		currentPresent[ev.ID] = true
		if _, ok := st.known[ev.ID]; ok {
			if !oldPresent[ev.ID] {
				// Known goal reappearing after an absence: a restore. It
				// backs the scoreboard again but must not be re-announced.
				switch ev.Team.ID {
				case st.homeID:
					reappearedHome++
				case st.awayID:
					reappearedAway++
				}
			}
			continue
		}

		st.known[ev.ID] = ev
		st.alerted[ev.ID] = true
		switch ev.Team.ID {
		case st.homeID:
			newHome++
		case st.awayID:
			newAway++
		}
		_ = t.notifier.Goal(ev, d.HomeTeam, d.AwayTeam, homeScore, awayScore)
	}
	st.present = currentPresent

	// 2. Scoreboard-driven reconciliation per team.
	t.reconcileTeam(st, st.homeID, st.homeScore, homeScore, newHome, reappearedHome, &st.pendingHome, d, currentPresent)
	t.reconcileTeam(st, st.awayID, st.awayScore, awayScore, newAway, reappearedAway, &st.pendingAway, d, currentPresent)

	st.homeScore = homeScore
	st.awayScore = awayScore
}

// reconcileTeam applies the score-delta rules for one side of the scoreboard.
//
// newEvents/reappeared count goal events that (re)appeared in this snapshot;
// they back scoreboard goals and therefore consume pending confirmations.
// A positive delta adds unconfirmed goals to the pending count. A negative
// delta first consumes pending (never-announced) changes silently; any
// remainder revokes previously announced goals with a single correction
// each.
func (t *Tracker) reconcileTeam(
	st *matchState,
	teamID int,
	oldScore, newScore int,
	newEvents, reappeared int,
	pending *int,
	d *api.MatchDetails,
	currentPresent map[int]bool,
) {
	delta := newScore - oldScore

	if delta > 0 {
		*pending += delta
	}

	// Events now present in the feed confirm scoreboard goals, including
	// when the scoreboard itself did not move this poll (the delayed-event
	// case: score arrived first, event catches up on a later snapshot).
	if backed := min(newEvents+reappeared, *pending); backed > 0 {
		*pending -= backed
	}

	if delta < 0 {
		drop := -delta
		if drop <= *pending {
			// The revoked scoreboard goal was never announced (its event
			// had not arrived yet): nothing to correct.
			*pending -= drop
			return
		}
		drop -= *pending
		*pending = 0
		t.correctGoals(st, teamID, drop, currentPresent, d)
	}
}

// correctGoals sends at most one GoalRevoked notification per goal for
// count disallowed goals of the given team. Events that have vanished from
// the snapshot are corrected first, then the most recent announced goals
// (covering feeds where the event lags behind the scoreboard correction).
func (t *Tracker) correctGoals(st *matchState, teamID, count int, currentPresent map[int]bool, d *api.MatchDetails) {
	var missing, present []api.MatchEvent
	for id, ev := range st.known {
		if ev.Team.ID != teamID || !st.alerted[id] || st.corrected[id] {
			continue
		}
		if currentPresent[id] {
			present = append(present, ev)
		} else {
			missing = append(missing, ev)
		}
	}
	sortGoalsDesc(missing)
	sortGoalsDesc(present)

	candidates := append(missing, present...)
	if count > len(candidates) {
		count = len(candidates)
	}
	homeScore, awayScore := scoreOf(d)
	for i := 0; i < count; i++ {
		ev := candidates[i]
		st.corrected[ev.ID] = true
		_ = t.notifier.GoalRevoked(ev, d.HomeTeam, d.AwayTeam, homeScore, awayScore)
	}
}

// stateFor returns the state record for a match, creating it on first use.
func (t *Tracker) stateFor(matchID int) *matchState {
	st, ok := t.matches[matchID]
	if !ok {
		st = &matchState{
			known:     map[int]api.MatchEvent{},
			present:   map[int]bool{},
			alerted:   map[int]bool{},
			corrected: map[int]bool{},
		}
		t.matches[matchID] = st
	}
	return st
}

// scoreOf extracts the scoreboard values, treating nil pointers as 0-0.
func scoreOf(d *api.MatchDetails) (int, int) {
	home, away := 0, 0
	if d.HomeScore != nil {
		home = *d.HomeScore
	}
	if d.AwayScore != nil {
		away = *d.AwayScore
	}
	return home, away
}

// goalEvents returns the goal events in chronological minute order. The
// original feed order breaks ties, so an out-of-order snapshot still
// produces notifications in match order.
func goalEvents(d *api.MatchDetails) []api.MatchEvent {
	var goals []api.MatchEvent
	for i := range d.Events {
		if d.Events[i].Type == "goal" {
			goals = append(goals, d.Events[i])
		}
	}
	sort.SliceStable(goals, func(i, j int) bool {
		return goals[i].Minute < goals[j].Minute
	})
	return goals
}

// sortGoalsDesc orders goal events most-recent-first, using the event ID
// as a deterministic tie-breaker.
func sortGoalsDesc(events []api.MatchEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Minute != events[j].Minute {
			return events[i].Minute > events[j].Minute
		}
		return events[i].ID > events[j].ID
	})
}
