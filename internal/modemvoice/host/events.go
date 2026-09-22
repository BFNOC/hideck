package host

import "github.com/iniwex5/vowifi-go/runtimehost/voicehost"

// Notifications are queued in call order and delivered outside device locks.
// Phone callbacks may synchronously query or reject the same call.
type notification struct {
	incoming *voicehost.IncomingCall
	event    voicehost.CallEvent
}

func (c *Controller) SubscribeIncomingCalls(fn func(voicehost.IncomingCall)) func() {
	c.mu.Lock()
	index := len(c.incoming)
	c.incoming = append(c.incoming, fn)
	c.mu.Unlock()
	return func() { c.mu.Lock(); c.incoming[index] = nil; c.mu.Unlock() }
}

func (c *Controller) SubscribeCallEvents(fn func(voicehost.CallEvent)) func() {
	c.mu.Lock()
	index := len(c.events)
	c.events = append(c.events, fn)
	c.mu.Unlock()
	return func() { c.mu.Lock(); c.events[index] = nil; c.mu.Unlock() }
}

func (c *Controller) publish(n notification) {
	c.mu.Lock()
	c.pending = append(c.pending, n)
	if !c.dispatching {
		c.dispatching = true
		go c.dispatch()
	}
	c.mu.Unlock()
}

func (c *Controller) dispatch() {
	for {
		c.mu.Lock()
		if len(c.pending) == 0 {
			c.dispatching = false
			c.mu.Unlock()
			return
		}
		n := c.pending[0]
		c.pending[0] = notification{}
		c.pending = c.pending[1:]
		incoming := append([]func(voicehost.IncomingCall){}, c.incoming...)
		events := append([]func(voicehost.CallEvent){}, c.events...)
		c.mu.Unlock()
		if n.incoming != nil {
			for _, fn := range incoming {
				if fn != nil {
					fn(*n.incoming)
				}
			}
		} else {
			for _, fn := range events {
				if fn != nil {
					fn(n.event)
				}
			}
		}
	}
}
