package tui

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dantearo/arkex/internal/agent"
)

// targetFPS is the renderer's frame cap (Bubble Tea's maximum) and the rate
// at which streamed events are folded into the UI.
const targetFPS = 120

// frameInterval is how long the coalescer waits after the first event of a
// batch before handing the batch to the program.
const frameInterval = time.Second / targetFPS

// eventsMsg carries a batch of agent events; the UI applies them all, then
// redraws once.
type eventsMsg struct{ events []agent.Event }

// coalescer batches agent events per frame. Bubble Tea calls View after every
// message, so a fast model that streams 500 deltas a second would otherwise
// force 500 redraws a second; with the coalescer it is at most targetFPS.
// The first event of a batch is delayed by at most one frame.
type coalescer struct {
	send func(tea.Msg)

	mu        sync.Mutex
	pending   []agent.Event
	scheduled bool
}

func newCoalescer(send func(tea.Msg)) *coalescer {
	return &coalescer{send: send}
}

// push queues e and arms the frame timer if it is not already running.
func (c *coalescer) push(e agent.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, e)
	if !c.scheduled {
		c.scheduled = true
		time.AfterFunc(frameInterval, c.flush)
	}
}

// flush delivers everything queued so far. It holds the lock across send so
// a concurrent flush (the timer racing a final drain) cannot reorder
// batches; send only blocks until the program's loop accepts the message.
func (c *coalescer) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scheduled = false
	if len(c.pending) == 0 {
		return
	}
	batch := c.pending
	c.pending = nil
	c.send(eventsMsg{events: batch})
}
