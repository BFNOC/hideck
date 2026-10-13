package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/internal/db"
)

const smsEventHeartbeatInterval = 15 * time.Second

func (s *Server) handleSMSEvents(c *gin.Context) {
	afterID, ok := parseSMSEventAfterID(c)
	if !ok {
		return
	}
	stream, cancel := db.SubscribeIncomingSMS()
	defer cancel()
	var backlog []db.SMS
	if afterID != nil {
		var err error
		backlog, err = db.ListIncomingUnreadSMSAfter(c.Request.Context(), *afterID, 0)
		if err != nil {
			slog.Error("查询短信事件补发失败", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"message": "短信事件查询失败，请稍后重试"})
			return
		}
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-store")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	_, _ = fmt.Fprint(c.Writer, ": connected\n\n")
	c.Writer.Flush()
	seen := make(map[uint]struct{}, len(backlog))
	for _, sms := range backlog {
		writeSMSReceivedEvent(c, sms)
		seen[sms.ID] = struct{}{}
	}
	c.Writer.Flush()
	heartbeat := time.NewTicker(smsEventHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case sms, ok := <-stream:
			if !ok {
				return
			}
			if _, already := seen[sms.ID]; already {
				continue
			}
			writeSMSReceivedEvent(c, sms)
			c.Writer.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(c.Writer, ": heartbeat\n\n")
			c.Writer.Flush()
		case <-c.Request.Context().Done():
			return
		case <-s.shutdownCh:
			return
		}
	}
}

func writeSMSReceivedEvent(c *gin.Context, sms db.SMS) {
	payload, err := json.Marshal(sms)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(c.Writer, "id: %d\nevent: sms_received\ndata: %s\n\n", sms.ID, payload)
}

func parseSMSEventAfterID(c *gin.Context) (*uint, bool) {
	if header := strings.TrimSpace(c.GetHeader("Last-Event-ID")); header != "" {
		return parseSMSCursorValue(c, header)
	}
	return parseSMSQueryAfterID(c)
}
