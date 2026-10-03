package slackbot

import "testing"

func TestStopWords(t *testing.T) {
	yes := []string{"stop", "  STOP ", "停", "<@" + alpha + "> stop", "<@" + alpha + "> <@" + beta + "> stop",
		"stop <@" + beta + ">", "<@" + beta + "|beta>  停  "}
	no := []string{"停止", "stop please", "<@" + alpha + "> stop now", "", "<@" + alpha + ">"}
	for _, s := range yes {
		if !isStop(s) {
			t.Errorf("%q should stop", s)
		}
	}
	for _, s := range no {
		if isStop(s) {
			t.Errorf("%q should not stop", s)
		}
	}
}

func TestStopNamingSeveralPartners(t *testing.T) {
	tm := newTeam(t, false)
	tm.both(Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> <@" + beta + "> SLOW"})
	eventually(t, "both running", func() bool { return len(tm.ha.turns()) == 1 && len(tm.hb.turns()) == 1 })
	tm.both(Inbound{Channel: "C1", TS: "1.5", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> <@" + beta + "> stop"})
	eventually(t, "ack", func() bool { return tm.fp.count(StopAck) == 1 })
	eventually(t, "sessions closed", func() bool {
		return tm.ha.sessions[0].isClosed() && tm.hb.sessions[0].isClosed()
	})
}
