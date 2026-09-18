package atc

import (
	"fmt"
	"strings"
	"time"

	"github.com/skycontrol/skycontrol/internal/airfield"
	"github.com/skycontrol/skycontrol/internal/radio"
)

// DCS Class-D is too small for jets. 15 NM / 10k ft is a military tower bubble.
const (
	handoffRadiusNM = 15.0
	handoffHysteresisNM = 3.0
	patternRadiusNM = 5.0
	patternAGLFt    = 2500.0
	minHandoffAGLFt = 1500.0
	divertCloserNM  = 5.0
	autoHandoffGap  = 30 * time.Second
	remindAfter     = 20 * time.Second
	remindAgainAfter = 45 * time.Second
)

func ownerOrNearest(st *AircraftState) *airfield.Airfield {
	if st == nil {
		return nil
	}
	if st.Owner != nil {
		return st.Owner
	}
	return st.Nearest
}

func (t *Tower) TickHandoff() {
	t.mu.Lock()
	now := time.Now()
	for id, st := range t.aircraft {
		if st == nil || strings.HasPrefix(id, "demo-") {
			continue
		}
		if now.Sub(st.LastSeen) > 20*time.Second {
			delete(t.aircraft, id)
		}
	}
	st := t.primaryLocked()
	var act *hoAction
	if st != nil {
		t.seedOwnerLocked(st)
		t.maybeDivertLocked(st)
		act = t.planHandoffLocked(st)
	}
	t.mu.Unlock()
	if act != nil {
		t.execHandoff(act)
	}
}

func playerish(st *AircraftState) bool {
	if st == nil || st.Pilot == "" || st.Pilot == st.Type {
		return false
	}
	return strings.Contains(st.Pilot, "|") || containsDigit(st.Callsign) || containsDigit(st.Pilot)
}

func aircraftRank(st *AircraftState) (int, time.Time) {
	if st == nil {
		return -999, time.Time{}
	}
	if strings.HasPrefix(st.ID, "demo-") {
		return -1, st.LastSeen
	}
	if time.Since(st.LastSeen) > 12*time.Second {
		return 0, st.LastSeen
	}
	score := 10
	if playerish(st) {
		score += 20
	}
	if !st.OnGround {
		score += 50
	}
	if st.SpeedMS > 20 {
		score += 10
	}
	return score, st.LastSeen
}

func betterAC(a, b *AircraftState) bool {
	if b == nil {
		return a != nil
	}
	if a == nil {
		return false
	}
	sa, ta := aircraftRank(a)
	sb, tb := aircraftRank(b)
	if sa != sb {
		return sa > sb
	}
	if !a.Spawned.Equal(b.Spawned) {
		return a.Spawned.After(b.Spawned)
	}
	return ta.After(tb)
}

func (t *Tower) primaryLocked() *AircraftState {
	var best *AircraftState
	for _, st := range t.aircraft {
		if betterAC(st, best) {
			best = st
		}
	}
	return best
}

func containsDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func (t *Tower) seedOwnerLocked(st *AircraftState) {
	if st == nil || st.Owner != nil {
		return
	}
	if st.Nearest != nil {
		st.Owner = st.Nearest
	}
}

type hoAction struct {
	remind int // 0 first contact, 1 first reminder, 2 last call
	pilot  string
	owner  *airfield.Airfield
	dest   *airfield.Airfield
}

func (t *Tower) planHandoffLocked(st *AircraftState) *hoAction {
	if st == nil || st.OnGround || strings.HasPrefix(st.ID, "demo-") {
		return nil
	}
	now := time.Now()
	if now.Sub(st.hoTick) < time.Second {
		return nil
	}
	st.hoTick = now

	if st.Owner == nil {
		return nil
	}

	if st.NeedContact && st.Dest != nil && st.Dest.Name != st.Owner.Name {
		st.NeedContact = false
		st.HandoffAt = now
		st.Reminders = 0
		st.LastAutoHO = now
		st.JustSwitched = false
		t.log.Info("handoff contact (retarget)", "from", st.Owner.Name, "to", st.Dest.Name)
		return t.contactAction(st, 0)
	}

	// Reminders while waiting for check-in.
	if st.Dest != nil && st.Owner != nil && st.Dest.Name != st.Owner.Name && !st.HandoffAt.IsZero() {
		wait := now.Sub(st.HandoffAt)
		if st.Reminders == 0 && wait >= remindAfter {
			st.Reminders = 1
			st.LastRemind = now
			return t.contactAction(st, 1)
		}
		if st.Reminders == 1 && wait >= remindAgainAfter {
			st.Reminders = 2
			st.LastRemind = now
			return t.contactAction(st, 2)
		}
		return nil
	}

	if !t.shouldAutoLocked(st) {
		return nil
	}
	dest := t.handoffTargetLocked(st)
	if dest == nil || dest == st.Owner || (st.Owner != nil && dest.Name == st.Owner.Name) {
		return nil
	}
	st.Dest = dest
	st.HandoffAt = now
	st.Reminders = 0
	st.LastAutoHO = now
	st.JustSwitched = false
	t.log.Info("auto handoff contact", "from", st.Owner.Name, "to", dest.Name, "pilot", st.Callsign)
	return t.contactAction(st, 0)
}

func (t *Tower) contactAction(st *AircraftState, remind int) *hoAction {
	return &hoAction{
		remind: remind,
		pilot:  st.Callsign,
		owner:  st.Owner,
		dest:   st.Dest,
	}
}

func (t *Tower) shouldAutoLocked(st *AircraftState) bool {
	if st == nil || st.Owner == nil || st.OnGround {
		return false
	}
	agl := st.AltitudeFt - st.Owner.ElevationFt
	if agl < minHandoffAGLFt {
		return false
	}
	dOwner := airfield.DistanceNM(st.Latitude, st.Longitude, st.Owner.Latitude, st.Owner.Longitude)
	if dOwner < patternRadiusNM && agl < patternAGLFt {
		return false
	}
	if !st.LastAutoHO.IsZero() && time.Since(st.LastAutoHO) < autoHandoffGap {
		return false
	}
	if st.Dest != nil && st.Owner != nil && st.Dest.Name != st.Owner.Name && !st.HandoffAt.IsZero() {
		return false
	}
	dest := t.handoffTargetLocked(st)
	if dest == nil {
		return false
	}
	dDest := airfield.DistanceNM(st.Latitude, st.Longitude, dest.Latitude, dest.Longitude)
	leftBubble := dOwner > handoffRadiusNM && dDest < dOwner
	halfway := dDest+handoffHysteresisNM < dOwner
	return leftBubble || halfway
}

func (t *Tower) handoffTargetLocked(st *AircraftState) *airfield.Airfield {
	if st == nil {
		return nil
	}
	if st.Dest != nil && t.destMakesSenseLocked(st, st.Dest) {
		return st.Dest
	}
	return t.nextNearestLocked(st)
}

func (t *Tower) nextNearestLocked(st *AircraftState) *airfield.Airfield {
	if st == nil || t.airfields == nil {
		return nil
	}
	mapName := ""
	if st.Owner != nil {
		mapName = st.Owner.Map
	} else if st.Nearest != nil {
		mapName = st.Nearest.Map
	}
	nearby := t.airfields.NearbyOnMap(mapName, st.Latitude, st.Longitude, 8)
	var ahead, any *airfield.Airfield
	aheadD, anyD := 1e9, 1e9
	for _, n := range nearby {
		if st.Owner != nil && strings.EqualFold(n.Name, st.Owner.Name) {
			continue
		}
		af, ok := t.airfields.GetByName(n.Name)
		if !ok || af == nil {
			continue
		}
		if n.DistanceNM < anyD {
			any, anyD = af, n.DistanceNM
		}
		if headingDelta(st.Heading, n.BearingDeg) <= 60 && n.DistanceNM < aheadD {
			ahead, aheadD = af, n.DistanceNM
		}
	}
	if ahead != nil {
		return ahead
	}
	return any
}

func (t *Tower) destMakesSenseLocked(st *AircraftState, dest *airfield.Airfield) bool {
	if dest == nil {
		return false
	}
	brg := bearingTo(st.Latitude, st.Longitude, dest.Latitude, dest.Longitude)
	if headingDelta(st.Heading, brg) <= 90 {
		return true
	}
	dDest := airfield.DistanceNM(st.Latitude, st.Longitude, dest.Latitude, dest.Longitude)
	alt := t.nextNearestLocked(st)
	if alt == nil || alt.Name == dest.Name {
		return true
	}
	dAlt := airfield.DistanceNM(st.Latitude, st.Longitude, alt.Latitude, alt.Longitude)
	return dDest <= dAlt+divertCloserNM
}

func (t *Tower) maybeDivertLocked(st *AircraftState) {
	if st == nil || st.Dest == nil || st.OnGround {
		return
	}
	alt := t.nextNearestLocked(st)
	if alt == nil || alt.Name == st.Dest.Name {
		return
	}
	dDest := airfield.DistanceNM(st.Latitude, st.Longitude, st.Dest.Latitude, st.Dest.Longitude)
	dAlt := airfield.DistanceNM(st.Latitude, st.Longitude, alt.Latitude, alt.Longitude)
	brg := bearingTo(st.Latitude, st.Longitude, alt.Latitude, alt.Longitude)
	if dAlt+divertCloserNM < dDest && headingDelta(st.Heading, brg) <= 60 {
		t.log.Info("handoff divert", "from", st.Dest.Name, "to", alt.Name)
		st.Dest = alt
		if !st.HandoffAt.IsZero() && st.Owner != nil && st.Owner.Name != alt.Name {
			st.HandoffAt = time.Now()
			st.Reminders = 0
			st.NeedContact = true
		}
	}
}

func (t *Tower) execHandoff(act *hoAction) {
	if act == nil || act.owner == nil || act.dest == nil {
		return
	}
	pilot := act.pilot
	if pilot == "" {
		pilot = "Aircraft"
	}
	freq := SpeakFrequency(act.dest.PrimaryTowerFreq())
	if freq == "" {
		freq = "this frequency"
	}
	destCS := act.dest.Callsign(airfield.RoleTower)
	if len(act.dest.Frequencies.Approach) > 0 {
		destCS = act.dest.Callsign(airfield.RoleApproach)
		if sp := SpeakFrequency(act.dest.Frequencies.Approach[0]); sp != "" {
			freq = sp
		}
	}
	ownerCS := act.owner.Callsign(airfield.RoleTower)
	var text string
	switch act.remind {
	case 1:
		text = fmt.Sprintf("%s, %s, I say again, contact %s on %s.", pilot, ownerCS, destCS, freq)
	case 2:
		text = fmt.Sprintf("%s, %s, last call, contact %s on %s. If unable, stay this frequency.", pilot, ownerCS, destCS, freq)
	default:
		text = fmt.Sprintf("%s, %s, contact %s on %s.", pilot, ownerCS, destCS, freq)
	}
	fmt.Printf("  handoff: %s -> %s\n", act.owner.Name, act.dest.Name)
	t.sayAs(act.owner, RoleTower, text)
}

func (t *Tower) beginHandoff(st *AircraftState, dest *airfield.Airfield) *hoAction {
	if st == nil || dest == nil {
		return nil
	}
	t.seedOwnerLocked(st)
	if st.Owner != nil && dest.Name == st.Owner.Name {
		return nil
	}
	st.Dest = dest
	st.HandoffAt = time.Now()
	st.Reminders = 0
	st.LastAutoHO = time.Now()
	st.JustSwitched = false
	return t.contactAction(st, 0)
}

func (t *Tower) completeHandoffLocked(st *AircraftState) {
	if st == nil || st.Dest == nil {
		return
	}
	if st.Owner != nil && st.Dest.Name == st.Owner.Name {
		st.HandoffAt = time.Time{}
		st.Reminders = 0
		st.JustSwitched = false
		return
	}
	st.Prev = st.Owner
	st.Owner = st.Dest
	st.HandoffAt = time.Time{}
	st.Reminders = 0
	st.JustSwitched = true
	t.log.Info("handoff complete", "now", st.Owner.Name, "pilot", st.Callsign)
	fmt.Printf("  now talking as %s\n", st.Owner.Name)
}

func (t *Tower) rollbackHandoffLocked(st *AircraftState) bool {
	if st == nil || !st.JustSwitched || st.Prev == nil {
		return false
	}
	st.Owner, st.Prev = st.Prev, st.Owner
	st.Dest = st.Prev
	st.JustSwitched = false
	st.HandoffAt = time.Now()
	st.Reminders = 0
	t.log.Info("handoff rollback — still on old freq", "owner", st.Owner.Name)
	fmt.Printf("  rollback to %s (you probably didn't switch yet)\n", st.Owner.Name)
	return true
}

func (t *Tower) namedField(text string, nearby []airfield.Nearby, home *airfield.Airfield) *airfield.Airfield {
	name := guessFieldName(text, nearby, home)
	if name != "" {
		if af, ok := t.airfields.GetByName(name); ok {
			return af
		}
	}
	return t.fieldBySpokenFreq(text, nearby, home)
}

func (t *Tower) fieldBySpokenFreq(text string, nearby []airfield.Nearby, home *airfield.Airfield) *airfield.Airfield {
	want := spokenFreqKeys(text)
	if len(want) == 0 {
		return nil
	}
	homeName := ""
	if home != nil {
		homeName = strings.ToLower(home.Name)
	}
	for _, n := range nearby {
		if strings.ToLower(n.Name) == homeName {
			continue
		}
		af, ok := t.airfields.GetByName(n.Name)
		if !ok {
			continue
		}
		for _, s := range append(append([]string{}, af.Frequencies.Tower...), af.Frequencies.Ground...) {
			if want[freqKeyKHz(s)] {
				return af
			}
		}
	}
	return nil
}

func spokenFreqKeys(text string) map[int]bool {
	out := map[int]bool{}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			continue
		}
		j := i
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		if j >= len(text) || text[j] != '.' {
			i = j
			continue
		}
		k := j + 1
		for k < len(text) && text[k] >= '0' && text[k] <= '9' {
			k++
		}
		if k > j+1 {
			if f := freqKeyKHz(text[i:k]); f > 0 {
				out[f] = true
			}
		}
		i = k
	}
	return out
}

func freqKeyKHz(s string) int {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.ToUpper(s), "AM")
	s = strings.TrimSuffix(s, "FM")
	var mhz float64
	if _, err := fmt.Sscanf(s, "%f", &mhz); err != nil || mhz < 30 {
		return 0
	}
	return int(mhz*1000 + 0.5)
}

func (t *Tower) airRole(af *airfield.Airfield, onGround bool) Role {
	if onGround {
		if af != nil && len(af.Frequencies.Ground) > 0 {
			return RoleGround
		}
		return RoleTower
	}
	if af != nil && len(af.Frequencies.Approach) > 0 {
		return RoleApproach
	}
	return RoleTower
}

func (t *Tower) sayAs(af *airfield.Airfield, role Role, text string) {
	cs := t.cfgCallsign(af, role)
	tx := radio.Transmission{
		Callsign:  cs,
		Text:      SpeakForRadio(text),
		Spoken:    text,
		Coalition: 0,
	}
	if f := uhfOf(af); f.Hz > 0 {
		tx.Frequency = f
		tx.ExtraFreqs = []radio.Frequency{f}
	}
	t.sayTX(tx, text)
}

func uhfOf(af *airfield.Airfield) radio.Frequency {
	if af == nil {
		return radio.Frequency{}
	}
	pick := func(list []string) radio.Frequency {
		var vhf radio.Frequency
		for _, s := range list {
			f, err := radio.ParseFrequency(s)
			if err != nil {
				continue
			}
			if f.Hz >= 200_000_000 {
				return f
			}
			if vhf.Hz == 0 {
				vhf = f
			}
		}
		return vhf
	}
	if f := pick(af.Frequencies.Tower); f.Hz > 0 {
		return f
	}
	if f := pick(af.Frequencies.Ground); f.Hz > 0 {
		return f
	}
	if f := pick(af.Frequencies.Approach); f.Hz > 0 {
		return f
	}
	return radio.Frequency{}
}

func fieldNameIn(text, name string) bool {
	if name == "" || text == "" {
		return false
	}
	return fieldNameHits(compactField(foldFieldAliases(strings.ToLower(text))), name)
}

func headingDelta(hdg, brg float64) float64 {
	d := hdg - brg
	for d > 180 {
		d -= 360
	}
	for d < -180 {
		d += 360
	}
	if d < 0 {
		d = -d
	}
	return d
}

func bearingTo(lat1, lon1, lat2, lon2 float64) float64 {
	return airfield.BearingDeg(lat1, lon1, lat2, lon2)
}

func (t *Tower) handleHandoffCall(call radio.ReceivedCall) bool {
	text := strings.ToLower(strings.TrimSpace(call.Transcript))
	intent := DetectIntent(text)

	t.mu.Lock()
	st := t.identifyCaller(call)
	if st == nil {
		st = t.findByPilot(call.Pilot)
	}
	t.seedOwnerLocked(st)

	// Say-again after a switch: replay on BOTH freqs. Do not yank them back.
	if intent == IntentSayAgain && st != nil && st.JustSwitched {
		owner, prev := st.Owner, st.Prev
		t.lastMu.Lock()
		last := t.lastText
		t.lastMu.Unlock()
		t.mu.Unlock()
		if last == "" {
			last = "Say again, I did not copy."
		}
		fmt.Println("  say-again on both freqs (you may still be on the old one)")
		if owner != nil {
			t.sayAs(owner, RoleTower, last)
		}
		if prev != nil && (owner == nil || prev.Name != owner.Name) {
			t.sayAs(prev, RoleTower, last)
		}
		return true
	}

	if intent == IntentUnable && st != nil && st.Owner != nil {
		st.Dest = st.Owner
		st.HandoffAt = time.Time{}
		st.Reminders = 0
		st.NeedContact = false
		st.JustSwitched = false
		af := st.Owner
		pilot := st.Callsign
		t.mu.Unlock()
		t.sayAs(af, RoleTower, fmt.Sprintf("%s, %s, roger, remain this frequency.",
			pilot, t.cfgCallsign(af, RoleTower)))
		return true
	}

	var nearby []airfield.Nearby
	if st != nil && t.airfields != nil {
		mapName := ""
		if st.Owner != nil {
			mapName = st.Owner.Map
		} else if st.Nearest != nil {
			mapName = st.Nearest.Map
		}
		nearby = t.airfields.NearbyOnMap(mapName, st.Latitude, st.Longitude, 20)
		push := func(af *airfield.Airfield) {
			if af == nil {
				return
			}
			for _, n := range nearby {
				if strings.EqualFold(n.Name, af.Name) {
					return
				}
			}
			nearby = append(nearby, airfield.Nearby{Name: af.Name, TowerFreq: af.PrimaryTowerFreq()})
		}
		push(st.Owner)
		push(st.Dest)
		push(st.Prev)
		push(st.Nearest)
	}
	var home *airfield.Airfield
	if st != nil {
		home = st.Owner
		if home == nil {
			home = st.Nearest
		}
	}
	named := t.namedField(call.Transcript, nearby, home)

	contactTalk := containsAny(text, "switch to", "switching to", "contacting", "contact ",
		"change to", "changing to", "going over", "hand off", "handoff")
	destTalk := containsAny(text, "departing for", "headed to", "heading to", "going to",
		"enroute to", "en route to", "destination", "inbound to")

	addressedOwner := st != nil && st.Owner != nil && fieldNameIn(foldFieldAliases(text), st.Owner.Name)
	namedOther := named != nil && st != nil && st.Owner != nil && named.Name != st.Owner.Name
	checking := intent == IntentRadioCheck || intent == IntentInbound || intent == IntentLanding ||
		intent == IntentCheckIn ||
		containsAny(text, "checking in", "check in", "with you")

	become := func(dest *airfield.Airfield) bool {
		if st == nil || dest == nil {
			return false
		}
		st.Dest = dest
		t.completeHandoffLocked(st)
		owner, prev, pilot := st.Owner, st.Prev, st.Callsign
		t.mu.Unlock()
		t.execCheckin(owner, prev, pilot)
		return true
	}

	// Already talking to the other tower (check-in / "switching to X" without calling the old one).
	if namedOther && (checking || (contactTalk && !addressedOwner)) {
		return become(named)
	}
	if named != nil && contactTalk && st != nil {
		act := t.beginHandoff(st, named)
		t.mu.Unlock()
		if act != nil {
			t.execHandoff(act)
			return true
		}
		return false
	}
	// Arm destination only on a real dest phrase — NOT "how far to Kutaisi".
	if named != nil && destTalk && st != nil && (st.Owner == nil || named.Name != st.Owner.Name) {
		st.Dest = named
	}

	pending := st != nil && st.Dest != nil && st.Owner != nil && st.Dest.Name != st.Owner.Name && !st.HandoffAt.IsZero()
	talkingToOld := addressedOwner && !namedOther

	if pending && checking && !talkingToOld {
		t.completeHandoffLocked(st)
		owner, prev, pilot := st.Owner, st.Prev, st.Callsign
		t.mu.Unlock()
		t.execCheckin(owner, prev, pilot)
		return true
	}
	t.mu.Unlock()
	return false
}

func (t *Tower) execCheckin(owner, prev *airfield.Airfield, pilot string) {
	if owner == nil {
		return
	}
	if pilot == "" {
		pilot = "Aircraft"
	}
	cs := t.cfgCallsign(owner, RoleTower)
	rw := t.activeSpoken(owner, nil)
	t.mu.Lock()
	st := t.primaryLocked()
	text := fmt.Sprintf("%s, %s, radar contact. %s in use.", pilot, cs, rw)
	text = t.attachTraffic(text, st, owner, "check_in")
	t.mu.Unlock()
	fmt.Printf("  now talking as %s (check-in also on old freq if you forgot to switch)\n", owner.Name)
	t.sayAs(owner, RoleTower, text)
	if prev != nil && prev.Name != owner.Name {
		t.sayAs(prev, RoleTower, text)
	}
}
