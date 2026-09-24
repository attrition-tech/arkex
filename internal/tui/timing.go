package tui

import (
	"fmt"
	"time"
)

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
			detail: fmt.Sprintf("Prepare %s · Dispatch %s · Connection %s", timingDuration(r.Prepare), timingDuration(r.Dispatch), timingDuration(r.Connection)),
		})
	}
	items = append(items, paletteItem{title: "What these numbers measure", detail: "From request preparation: dispatch ends at HTTP write; first token is first reasoning/text/tool content. Connection includes pool wait, DNS, TCP and TLS; it overlaps dispatch. Failed requests are included."})
	return items
}
