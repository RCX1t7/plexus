package slackbot

import (
	"testing"
	"time"
)

// Review item 10: question routing.

func TestQuestionPlainAnswerWhenOnlyOnePartnerAsks(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> ASK 2"})
	eventually(t, "questions posted", func() bool { return tm.fp.count("question 2?") == 1 })
	// two questions from one partner form a queue: plain text answers the oldest first
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "first"})
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: sin, Text: "second"})
	eventually(t, "answers", func() bool { return tm.fp.count("answered: first | second") == 1 })
}

func TestQuestionNotAnsweredByMessageToAnotherPartner(t *testing.T) {
	tm := newTeam(t, false)
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "<@" + alpha + "> ASK 1"})
	eventually(t, "question posted", func() bool { return tm.fp.count("question 1?") == 1 })
	in := Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "<@" + beta + "> please run the benchmarks"}
	tm.both(in)
	eventually(t, "beta turn", func() bool { return len(tm.hb.turns()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if tm.fp.count("answered:") != 0 {
		t.Fatal("a message to beta was taken as alpha's answer")
	}
	tm.alpha.Handle(tm.ctx, Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: sin, Text: "yes"})
	eventually(t, "answered", func() bool { return tm.fp.count("answered: yes") == 1 })
}

func TestQuestionsFromTwoPartnersNeedAMention(t *testing.T) {
	tm := newTeam(t, false)
	tm.both(Inbound{Channel: "C1", TS: "1.0", User: sin, Text: "ASK 1 <@" + alpha + "> <@" + beta + ">"})
	eventually(t, "both asked", func() bool { return tm.fp.count("question 1?") == 2 })
	eventually(t, "both stored", func() bool {
		q, _ := tm.st.OpenQuestionsInThread("C1:1.0")
		return len(q) == 2
	})
	tm.both(Inbound{Channel: "C1", TS: "1.1", ThreadTS: "1.0", User: sin, Text: "yes"})
	time.Sleep(100 * time.Millisecond)
	if tm.fp.count("answered:") != 0 {
		t.Fatal("ambiguous plain answer was consumed")
	}
	tm.both(Inbound{Channel: "C1", TS: "1.2", ThreadTS: "1.0", User: sin, Text: "<@" + alpha + "> A"})
	eventually(t, "alpha answered", func() bool { return tm.fp.count("answered: A") == 1 })
	// now only beta waits: plain text answers it
	tm.both(Inbound{Channel: "C1", TS: "1.3", ThreadTS: "1.0", User: sin, Text: "B"})
	eventually(t, "beta answered", func() bool { return tm.fp.count("answered: B") == 1 })
}
