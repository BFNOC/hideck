package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
)

func (c *Controller) lockReady(ctx context.Context, id string) (*device, error) {
	d := c.get(id)
	if d == nil {
		return nil, errors.New("模组直拨尚未启动")
	}
	d.op.Lock()
	d.mu.Lock()
	ready := d.phase == "ready" && d.ctx.Err() == nil
	d.mu.Unlock()
	if !ready {
		d.op.Unlock()
		return nil, errors.New("模组直拨尚未就绪，请查看设备状态")
	}
	if err := d.resources.Check(ctx); err != nil {
		d.op.Unlock()
		return nil, err
	}
	return d, nil
}

func (c *Controller) BeginCall(ctx context.Context, req voicehost.BeginCallRequest) (voicehost.CallSnapshot, error) {
	if _, err := relay(req.SDP); err != nil {
		return voicehost.CallSnapshot{}, err
	}
	d, err := c.lockReady(ctx, req.DeviceID)
	if err != nil {
		return voicehost.CallSnapshot{}, err
	}
	defer d.op.Unlock()
	if c.ActiveCall(d.id) != nil {
		return voicehost.CallSnapshot{}, errors.New("模组已有通话")
	}
	conn, err := c.options.Listen()
	if err != nil {
		return voicehost.CallSnapshot{}, err
	}
	current := &call{conn: conn, snapshot: voicehost.CallSnapshot{CallID: "modemvoice-" + uuid.NewString(),
		DeviceID: d.id, Direction: "outbound", Peer: req.Callee, State: "calling", StartTime: time.Now(), ClientSDP: offer(conn)}}
	// Open real audio first. Failure must not leave an unowned dialed call.
	if err := c.openMedia(d.ctx, d, current, req.SDP); err != nil {
		return voicehost.CallSnapshot{}, errors.Join(err, conn.Close())
	}
	d.mu.Lock()
	d.call = current
	d.mu.Unlock()
	update, dialErr := d.session.Dial(ctx, req.Callee)
	if !update.Attempted {
		cleanupErr := c.endMedia(d, "dial_rejected")
		c.apply(d, update)
		return voicehost.CallSnapshot{}, errors.Join(dialErr, cleanupErr)
	}
	c.apply(d, update)
	if dialErr != nil {
		// ATD is never retried. Reconcile before returning because a timed-out
		// command can have created a physical call despite the missing OK.
		refresh, refreshErr := d.session.Refresh(d.ctx)
		c.apply(d, refresh)
		if refreshErr == nil && current.trackedID != "" {
			ended, hangupErr := d.session.Hangup(d.ctx, current.trackedID)
			c.apply(d, ended)
			dialErr = errors.Join(dialErr, hangupErr)
		}
		return voicehost.CallSnapshot{}, errors.Join(dialErr, refreshErr, c.endMedia(d, "dial_failed"))
	}
	return current.snapshot, nil
}

func (c *Controller) AnswerIncomingCall(ctx context.Context, req voicehost.AnswerRequest) (voicehost.AnswerResult, error) {
	d, err := c.lockReady(ctx, req.DeviceID)
	if err != nil {
		return voicehost.AnswerResult{}, err
	}
	defer d.op.Unlock()
	current, err := matchingCall(d, req.CallID)
	if err != nil {
		return voicehost.AnswerResult{}, err
	}
	if current.bridge != nil {
		return voicehost.AnswerResult{}, errors.New("通话已接听或正在接听")
	}
	if err := c.openMedia(d.ctx, d, current, req.SDP); err != nil {
		return voicehost.AnswerResult{}, errors.Join(err, c.endMedia(d, "audio_failed"))
	}
	update, err := d.session.Answer(ctx, current.trackedID)
	c.apply(d, update)
	if err != nil {
		return voicehost.AnswerResult{}, errors.Join(err, c.endMedia(d, "answer_failed"))
	}
	return voicehost.AnswerResult{CallID: req.CallID, OfferSDP: current.snapshot.ClientSDP, State: "answering"}, nil
}

func matchingCall(d *device, id string) (*call, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.call == nil || d.call.snapshot.CallID != id {
		return nil, errors.New("模组通话已结束或已更换")
	}
	return d.call, nil
}

func (c *Controller) HangupCall(ctx context.Context, id, callID string) error {
	d := c.get(id)
	if d == nil {
		return errors.New("模组直拨会话不存在")
	}
	d.op.Lock()
	defer d.op.Unlock()
	current, err := matchingCall(d, callID)
	if err != nil {
		return err
	}
	update, err := d.session.Refresh(ctx)
	c.apply(d, update)
	if err != nil {
		return err
	}
	if c.ActiveCall(id) == nil {
		return nil
	}
	if current.trackedID == "" {
		return errors.New("尚未确认模组通话索引，请稍后重试挂断")
	}
	update, err = d.session.Hangup(ctx, current.trackedID)
	c.apply(d, update)
	if err != nil {
		return err
	}
	return c.endMedia(d, "local_hangup")
}

func (c *Controller) RejectIncomingCall(req voicehost.RejectRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return c.HangupCall(ctx, req.DeviceID, req.CallID)
}

func (c *Controller) SendCallDTMF(string, string, string) error {
	return errors.New("模组直拨暂不支持按键发送")
}
func (c *Controller) HoldCall(context.Context, string, string) error {
	return voicehost.ErrHoldNotAligned
}
func (c *Controller) ResumeCall(context.Context, string, string) error {
	return voicehost.ErrHoldNotAligned
}
func (c *Controller) SwitchCall(string, string) error {
	return errors.New("模组直拨暂不支持切换通话")
}
func (c *Controller) StartCallCapture(string, string, string) error {
	return fmt.Errorf("模组直拨仅在网页接听后录音，不支持未接听录音")
}

func (c *Controller) apply(d *device, update modemvoice.Update) {
	for _, change := range update.Changes {
		if change.Call.Call.Mode != 0 {
			continue
		}
		c.applyCall(d, change)
	}
}
