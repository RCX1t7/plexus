package slackbot

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// routeGate is the in-memory routing gate (ANSWERS #16, architect ruling
// 2026-10-09): harness events routed through the worker's per-thread
// dispatch and fanned out across partners, no disk on the event path.
const routeGate = 20000 // events/s

// BenchmarkEventRouting floods harness events through the real Worker
// event loop (Session.Events -> turn dispatch -> work tracking) for two
// partners in parallel threads (fan-out across workers and goroutines). Disk is touched only once per turn (dedup
// of the trigger and the final post), amortized over the flood.
//
//	go test ./internal/slackbot/ -run '^$' -bench BenchmarkEventRouting -benchtime 200000x
func BenchmarkEventRouting(b *testing.B) {
	tm := newTeam(b, false)
	workers := []*Worker{tm.alpha, tm.beta}
	ids := []string{alpha, beta}
	per := b.N / len(workers)
	if per < 1 {
		per = 1
	}
	b.ResetTimer()
	start := time.Now()
	var wg sync.WaitGroup
	for i, w := range workers {
		wg.Add(1)
		go func(i int, w *Worker) {
			defer wg.Done()
			w.Handle(tm.ctx, Inbound{Channel: fmt.Sprintf("C%d", i), TS: "1.0", User: sin, Text: fmt.Sprintf("<@%s> FLOOD %d", ids[i], per)})
		}(i, w)
	}
	wg.Wait()
	deadline := time.Now().Add(time.Duration(b.N)*time.Millisecond + 30*time.Second)
	for tm.fp.count(fmt.Sprintf("flooded %d", per)) < len(workers) {
		if time.Now().After(deadline) {
			b.Fatal("flood did not finish")
		}
		time.Sleep(200 * time.Microsecond)
	}
	el := time.Since(start)
	b.StopTimer()
	rate := float64(per*len(workers)) / el.Seconds()
	b.ReportMetric(rate, "events/s")
	if b.N >= 100000 && rate < routeGate {
		b.Errorf("in-memory routing %.0f events/s is below the %d events/s gate", rate, routeGate)
	}
}
