package host

import (
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
)

func (c *Controller) applyCall(d *device, change modemvoice.Change) error {
	d.mu.Lock()
	current := d.call
	d.mu.Unlock()
	if current == nil {
		if !change.Ended && change.Call.Call.Inbound {
			c.incomingCall(d, change.Call)
		}
		return nil
	}
	if current.trackedID == "" && !change.Call.Call.Inbound && !change.Ended {
		current.trackedID = change.Call.ID
	}
	if current.trackedID != change.Call.ID {
		return nil
	}
	if change.Ended {
		reason := current.endReason
		if reason == "" {
			reason = "remote_hangup"
		}
		if err := c.endMedia(d, reason); err != nil {
			d.setStatus("failed", err)
			return err
		}
		return nil
	}
	state, kind := "ringing", "CallRinging"
	if change.Call.Call.State == modemvoice.Active || change.Call.Call.State == modemvoice.Held {
		state, kind = "connected", "CallAnswered"
	}
	d.mu.Lock()
	previous := current.snapshot.State
	current.snapshot.State = state
	current.snapshot.Held = change.Call.Call.State == modemvoice.Held
	snapshot := current.snapshot
	d.mu.Unlock()
	if previous != state {
		c.publish(notification{event: voicehost.CallEvent{Type: kind, DeviceID: d.id, CallID: snapshot.CallID,
			Direction: snapshot.Direction, State: state, Time: time.Now(), AudioCodec: "PCMU"}})
	}
	return nil
}

func (c *Controller) incomingCall(d *device, tracked modemvoice.TrackedCall) {
	if tracked.Call.State != modemvoice.Incoming {
		return
	}
	conn, err := c.options.Listen()
	if err != nil {
		d.setStatus("failed", err)
		return
	}
	snapshot := voicehost.CallSnapshot{DeviceID: d.id, CallID: "modemvoice-" + tracked.ID, Direction: "inbound",
		Peer: tracked.Call.Number, State: "ringing", StartTime: time.Now(), ClientSDP: offer(conn)}
	d.mu.Lock()
	d.call = &call{snapshot: snapshot, trackedID: tracked.ID, conn: conn}
	d.mu.Unlock()
	c.publish(notification{incoming: &voicehost.IncomingCall{DeviceID: d.id, CallID: snapshot.CallID,
		Caller: snapshot.Peer, OfferSDP: snapshot.ClientSDP, ReceivedAt: snapshot.StartTime, State: "ringing"}})
}
