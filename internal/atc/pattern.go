package atc

import (
	"fmt"
	"strings"
	"time"

	"github.com/skycontrol/skycontrol/internal/radio"
)

// Pattern legs for a military overhead recovery.
const (
	legNone     = ""
	legInitial  = "initial"
	legBreak    = "break"
	legDownwind = "downwind"
	legBase     = "base"
	legFinal    = "final"
	legLanded   = "landed"
	legThanks   = "thanks"
	legGreeting = "greeting"
	legAirborne = "airborne"
	legSignoff  = "signoff"
	legStraight  = "straight"
	legEmergency = "emergency"
)

// handlePatternCall intercepts overhead / departure / courtesy calls so
// ChatGPT cannot spam "cleared to land" on every position report.
func (t *Tower) handlePatternCall(call radio.ReceivedCall) bool {
	text := strings.ToLower(strings.TrimSpace(call.Transcript))
	if text == "" {
		return false
	}

	t.mu.Lock()
	st := t.identifyCaller(call)
	if st == nil {
		st = t.findByPilot(call.Pilot)
	}
	leg := detectPatternLeg(text, st)
	if leg == "" {
		t.mu.Unlock()
		return false
	}

	t.seedOwnerLocked(st)
	af := ownerOrNearest(st)
	pilot := "Aircraft"
	if st != nil && st.Callsign != "" {
		pilot = st.Callsign
	}
	role := RoleTower
	if st != nil && st.OnGround && (leg == legThanks || leg == legGreeting || leg == legSignoff || leg == legLanded) {
		role = t.airRole(af, true)
	}
	cs := t.cfgCallsign(af, role)
	runway := t.activeSpoken(af, st)
	wind := t.windPhrase()
	msg, next, clearLand, reset := patternReply(leg, st, pilot, cs, runway, wind)

	if st != nil {
		if reset {
			st.Pattern = ""
			st.ClearedLand = false
		}
		if next != "" {
			st.Pattern = next
		}
		if clearLand {
			st.ClearedLand = true
		}
		if leg == legAirborne {
			st.ClearedTakeoff = true
		}
		st.LastClearance = time.Now()
	}
	t.lastIntent = Intent(leg)
	t.lastIntentAt = time.Now()
	t.mu.Unlock()

	if msg == "" {
		return false
	}
	t.log.Info("pattern", "leg", leg, "next", next, "text", msg)
	fmt.Printf("  pattern: %s\n", leg)
	if af != nil {
		t.sayAs(af, role, msg)
	} else {
		t.say(call.Frequency, cs, msg)
	}
	return true
}

func detectPatternLeg(text string, st *AircraftState) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return ""
	}
	if containsAny(t, "mayday", "pan pan", "pan-pan", "declaring an emergency",
		"declaring emergency", "emergency", "low fuel", "minimum fuel", "bingo",
		"engine out", "flameout", "bird strike", "birdstrike") {
		return legEmergency
	}
	// Never steal these — existing handlers own them.
	if containsAny(t,
		"say again", "see again", "last transmission",
		"radio check", "how do you hear",
		"request taxi", "requesting taxi", "ready to taxi",
		"request takeoff", "requesting takeoff", "ready for departure", "ready for takeoff",
		"request startup", "requesting startup",
		"switching to", "switch to", "contact ", "contacting",
		"go around", "going around", "missed approach",
		"touch and go", "touch-and-go",
		"hold short",
		"request parking", "taxi to parking") {
		return ""
	}
	if containsAny(t, "thank you", "thanks", "cheers", "good day", "good night",
		"have a good", "no further", "that was accurate", "appreciate it", "appreciate that") {
		return legThanks
	}
	if containsAny(t, "good morning", "good afternoon", "good evening",
		"how are you", "how's it going", "hows it going", "how are ya",
		"morning tower", "evening tower") {
		return legGreeting
	}
	if containsAny(t, "leaving your frequency", "frequency change approved",
		"switching to departure", "contact departure", "off your frequency") {
		return legSignoff
	}

	onGround := st != nil && st.OnGround
	if onGround {
		if containsAny(t, "airborne", "climbing out", "off the deck") {
			return legAirborne
		}
		return ""
	}

	if containsAny(t, "straight in", "straight-in", "direct approach", "direct-in", "direct in") {
		if containsAny(t, "short final", "on final", "full stop") {
			return legFinal
		}
		return legStraight
	}
	if containsAny(t, "short final", "on final", "on the numbers", "short finals", "rolling out") {
		return legFinal
	}
	if containsAny(t, "full stop") && containsAny(t, "approach", "landing", "final") {
		return legFinal
	}
	if containsAny(t, "turning base", "on base", "base leg", "turning to base") {
		return legBase
	}
	if strings.Contains(t, "base") && !containsAny(t, "airbase", "database", "based") {
		return legBase
	}
	if containsAny(t, "downwind", "abeam") {
		return legDownwind
	}
	if containsAny(t, "gear down", "three down", "wheels down") &&
		!containsAny(t, "short final", "on final") {
		return legDownwind
	}
	if containsAny(t, "midfield break", "the break", "breaking", "overhead", "in the break") {
		return legBreak
	}
	if strings.Contains(t, "break") && !containsAny(t, "breakup", "breaker", "breakfast") {
		return legBreak
	}
	if containsAny(t, "initial", "entering the pattern", "for the overhead", "left initial", "right initial") {
		return legInitial
	}
	if containsAny(t, "inbound", "in bound", "calling inbound") &&
		!containsAny(t, "request landing", "full stop") {
		return legInitial
	}
	if containsAny(t, "airborne", "climbing out", "off the deck", "climbing away") {
		return legAirborne
	}
	return ""
}

func patternReply(leg string, st *AircraftState, pilot, station, runway, wind string) (msg, next string, clearLand, reset bool) {
	already := st != nil && st.ClearedLand
	cur := ""
	if st != nil {
		cur = st.Pattern
	}
	switch leg {
	case legThanks:
		msg = fmt.Sprintf("%s, %s, roger, good day.", pilot, station)
		return msg, cur, false, false
	case legGreeting:
		msg = fmt.Sprintf("%s, %s, good day, go ahead.", pilot, station)
		return msg, cur, false, false
	case legEmergency:
		if wind != "" {
			msg = fmt.Sprintf("%s, %s, roger emergency, %s, cleared to land, full stop. %s.", pilot, station, runway, wind)
		} else {
			msg = fmt.Sprintf("%s, %s, roger emergency, %s, cleared to land, full stop.", pilot, station, runway)
		}
		return msg, legEmergency, true, false
	case legStraight:
		if wind != "" {
			msg = fmt.Sprintf("%s, %s, continue straight in, report final. %s. %s.", pilot, station, wind, runway)
		} else {
			msg = fmt.Sprintf("%s, %s, continue straight in, report final, %s.", pilot, station, runway)
		}
		return msg, legStraight, false, false
	case legFinal:
		if already {
			msg = fmt.Sprintf("%s, %s, continue, cleared to land %s, full stop.", pilot, station, runway)
		} else {
			msg = fmt.Sprintf("%s, %s, %s, cleared to land, full stop.", pilot, station, runway)
		}
		return msg, legFinal, true, false
	case legSignoff:
		msg = fmt.Sprintf("%s, %s, frequency change approved, good day.", pilot, station)
		return msg, cur, false, false
	case legAirborne:
		msg = fmt.Sprintf("%s, %s, radar contact, continue climb, remain this frequency.", pilot, station)
		return msg, "departure", false, false
	case legInitial:
		if wind != "" {
			msg = fmt.Sprintf("%s, %s, report break. %s. %s.", pilot, station, wind, runway)
		} else {
			msg = fmt.Sprintf("%s, %s, report break, %s.", pilot, station, runway)
		}
		return msg, legInitial, false, false
	case legBreak:
		msg = fmt.Sprintf("%s, %s, roger break, report downwind.", pilot, station)
		return msg, legBreak, false, false
	case legDownwind:
		msg = fmt.Sprintf("%s, %s, roger, report base.", pilot, station)
		if st != nil && st.LandingGear >= 0.5 {
			msg = fmt.Sprintf("%s, %s, roger gear, report base.", pilot, station)
		}
		return msg, legDownwind, false, false
	case legBase:
		if already {
			msg = fmt.Sprintf("%s, %s, continue, cleared to land %s.", pilot, station, runway)
		} else {
			msg = fmt.Sprintf("%s, %s, %s, cleared to land.", pilot, station, runway)
		}
		return msg, legBase, true, false
	}
	_ = reset
	return "", cur, false, false
}

func (t *Tower) windPhrase() string {
	w := t.windSample()
	if !w.OK || w.SpeedKt < 1 {
		return "wind calm"
	}
	return fmt.Sprintf("wind %.0f at %.0f", w.FromDeg, w.SpeedKt)
}
