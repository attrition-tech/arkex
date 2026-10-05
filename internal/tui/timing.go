package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/attrition-tech/arkex/internal/agent"
)

// Response counters use event receipt timestamps, not UI batching or paint
// time. The first answer may precede tools; total includes all later work up
// to the last answer delta. They deliberately do not count reasoning as text.
type responseTiming struct {
	start, requestSent, firstText, lastText time.Time
	llmFirstText                            time.Duration
	llmObserved                             bool
}

func (r *responseTiming) observe(e agent.Event) {
	if r.start.IsZero() {
		return
	}
	switch e := e.(type) {
	case agent.RequestSent:
		if r.firstText.IsZero() {
			r.requestSent = e.At
		}
	case agent.RequestTiming, agent.RequestRestart, agent.TurnStart:
		if r.firstText.IsZero() {
			r.requestSent = time.Time{}
		}
	case agent.TextDelta:
		if e.Text == "" || e.At.IsZero() {
			return
		}
		if r.firstText.IsZero() {
			r.firstText = e.At
			if !r.requestSent.IsZero() {
				r.llmFirstText = e.At.Sub(r.requestSent)
				r.llmObserved = true
			}
		}
		r.lastText = e.At
	}
}

func responseDuration(d time.Duration) string {
	d = max(0, d)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(d.Milliseconds()/10)/100), "0"), ".") + "s"
	}
	return fmt.Sprintf("%dm %02ds", int64(d/time.Minute), int64(d/time.Second)%60)
}

func (r responseTiming) footer(now time.Time, running bool) string {
	first, llm, total := "—", "—", "—"
	if !r.firstText.IsZero() {
		first = responseDuration(r.firstText.Sub(r.start))
		if r.llmObserved {
			llm = responseDuration(r.llmFirstText)
		}
	} else if running {
		first = responseDuration(now.Sub(r.start)) + "…"
		llm = "pending"
		if !r.requestSent.IsZero() {
			llm = responseDuration(now.Sub(r.requestSent)) + "…"
		}
	}
	if running {
		total = responseDuration(now.Sub(r.start)) + "…"
	} else if !r.lastText.IsZero() {
		total = responseDuration(r.lastText.Sub(r.start))
	}
	return fmt.Sprintf("First text %s · LLM first text %s · Total %s", first, llm, total)
}

func timingDuration(d time.Duration) string {
	if d <= 0 {
		return "unavailable"
	}
	if d < time.Millisecond {
		return "<1ms"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(10 * time.Millisecond).String()
}

func timingItems(m *model) []paletteItem {
	if len(m.requestTimings) == 0 {
		return []paletteItem{{title: "No completed requests measured", detail: "Timings are local to this run; resumed history has no timing data."}}
	}
	title := "Total run · " + timingDuration(m.runDuration)
	if m.running {
		title = "Run in progress"
	}
	items := []paletteItem{{title: title, detail: "Includes tools, approvals and compaction. Request timings are captured before display, not screen-paint latency."}}
	for i, r := range m.requestTimings {
		items = append(items, paletteItem{
			title:  fmt.Sprintf("Request %d · First token %s · Total %s", i+1, timingDuration(r.FirstToken), timingDuration(r.Total)),
			detail: fmt.Sprintf("Prepare %s · Dispatch %s · Connection %s · First answer text %s", timingDuration(r.Prepare), timingDuration(r.Dispatch), timingDuration(r.Connection), timingDuration(r.FirstText)),
		})
	}
	items = append(items, paletteItem{title: "What these numbers measure", detail: "From request preparation: dispatch ends at HTTP write; first token is first reasoning/text/tool content. Connection includes pool wait, DNS, TCP and TLS; it overlaps dispatch. Failed requests are included."})
	items = append(items, paletteItem{title: "Response footer", detail: "First text: send to first answer chunk (not reasoning/tools). LLM first text: that request's HTTP write to the same chunk. Total: send to last answer chunk, including intervening tools/retries; while running it shows elapsed time. Receipt is before UI batching/painting. Timings are not saved on resume."})
	return items
}
