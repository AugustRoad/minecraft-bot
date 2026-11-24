package logtail

import (
	"regexp"
	"strings"
	"time"

	"github.com/AugustRoad/minecraft-bot/internal/events"
)

var (
	// Matches Vanilla/Paper/Purpur/Fabric/Forge log prefixes:
	//   [12:34:56] [Server thread/INFO]: <message>
	//   [12:34:56] [Async Chat Thread - #0/INFO]: <message>
	//   [12:34:56] [Server thread/INFO] [minecraft/DedicatedServer]: <message>
	//   [12:34:56 INFO]: <message>
	logPrefixRe = regexp.MustCompile(`^\[[^\]]+\](?:\s+\[[^\]]+/(?:INFO|WARN)\](?:\s+\[[^\]]+\])?|(?:\s+(?:INFO|WARN))):\s+(.*)$`)
	// Matches single-bracket Paper/Purpur style: [12:34:56 INFO]: <message>
	singleBracketPrefixRe = regexp.MustCompile(`^\[[^\]]+\s+(?:INFO|WARN)\]:\s+(.*)$`)

	doneRe       = regexp.MustCompile(`^Done \([0-9.,]+s\)!`)
	stoppingRe   = regexp.MustCompile(`^Stopping the server$`)
	joinRe       = regexp.MustCompile(`^([A-Za-z0-9_]{3,16}) joined the game$`)
	leaveRe      = regexp.MustCompile(`^([A-Za-z0-9_]{3,16}) left the game$`)
	chatRe       = regexp.MustCompile(`^(?:\[Not Secure\]\s+)?<([A-Za-z0-9_]{3,16})>\s+(.+)$`)
	advanceRe    = regexp.MustCompile(`^([A-Za-z0-9_]{3,16}) has (?:made the advancement|completed the challenge|reached the goal) \[(.+)\]$`)
	deathPattern = regexp.MustCompile(`^([A-Za-z0-9_]{3,16}) (` + strings.Join([]string{
		`was slain by .+`,
		`was shot by .+`,
		`was pummeled by .+`,
		`was pricked to death`,
		`walked into a cactus.*`,
		`drowned.*`,
		`experienced kinetic energy.*`,
		`blew up`,
		`was blown up by .+`,
		`was killed by .+`,
		`hit the ground too hard.*`,
		`fell from a high place.*`,
		`fell off .+`,
		`fell into .+`,
		`fell out of the world.*`,
		`was doomed to fall.*`,
		`was squashed by .+`,
		`went up in flames`,
		`walked into fire.*`,
		`burned to death`,
		`was burnt to a crisp.*`,
		`went off with a bang.*`,
		`tried to swim in lava.*`,
		`was struck by lightning.*`,
		`discovered the floor was lava.*`,
		`walked into the danger zone.*`,
		`suffocated in a wall.*`,
		`was squished too much`,
		`starved to death.*`,
		`withered away.*`,
		`froze to death.*`,
		`was frozen to death by .+`,
		`was impaled by .+`,
		`was skewered by .+`,
		`was stung to death.*`,
		`was obliterated by .+`,
		`was fireballed by .+`,
		`was poked to death by .+`,
		`died.*`,
	}, "|") + `)$`)
)

// ParseLine inspects a single line from Minecraft Java Edition's latest.log.
// Returns (event, true) when the line corresponds to a user-visible game/server event.
func ParseLine(line string, now time.Time) (events.Event, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return events.Event{}, false
	}

	var body string
	if m := logPrefixRe.FindStringSubmatch(line); m != nil {
		body = strings.TrimSpace(m[1])
	} else if m := singleBracketPrefixRe.FindStringSubmatch(line); m != nil {
		body = strings.TrimSpace(m[1])
	} else {
		return events.Event{}, false
	}
	if body == "" {
		return events.Event{}, false
	}

	if doneRe.MatchString(body) {
		return events.Event{
			Type:      events.TypeServerReady,
			Timestamp: now,
			Message:   body,
		}, true
	}

	if stoppingRe.MatchString(body) {
		return events.Event{
			Type:      events.TypeServerStopping,
			Timestamp: now,
			Message:   body,
		}, true
	}

	if sm := chatRe.FindStringSubmatch(body); sm != nil {
		return events.Event{
			Type:      events.TypeChat,
			Timestamp: now,
			Player:    sm[1],
			Message:   sm[2],
		}, true
	}

	if sm := joinRe.FindStringSubmatch(body); sm != nil {
		return events.Event{
			Type:      events.TypePlayerJoin,
			Timestamp: now,
			Player:    sm[1],
		}, true
	}

	if sm := leaveRe.FindStringSubmatch(body); sm != nil {
		return events.Event{
			Type:      events.TypePlayerLeave,
			Timestamp: now,
			Player:    sm[1],
		}, true
	}

	if sm := advanceRe.FindStringSubmatch(body); sm != nil {
		return events.Event{
			Type:      events.TypeAdvancement,
			Timestamp: now,
			Player:    sm[1],
			Message:   body,
		}, true
	}

	if sm := deathPattern.FindStringSubmatch(body); sm != nil {
		return events.Event{
			Type:      events.TypeDeath,
			Timestamp: now,
			Player:    sm[1],
			Message:   body,
		}, true
	}

	return events.Event{}, false
}
