package db

import (
	"context"
	"errors"
	"sync"
)

const (
	incomingSMSSubscriberBuffer = 32
	incomingSMSBacklogLimit     = 256
)

type incomingSMSHub struct {
	mu          sync.Mutex
	nextSubID   uint64
	subscribers map[uint64]chan SMS
}

var incomingSMS = newIncomingSMSHub()

func newIncomingSMSHub() *incomingSMSHub {
	return &incomingSMSHub{subscribers: make(map[uint64]chan SMS)}
}

func SubscribeIncomingSMS() (<-chan SMS, func()) {
	return incomingSMS.subscribe()
}

func ListIncomingUnreadSMSAfter(ctx context.Context, afterID uint, limit int) ([]SMS, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if limit <= 0 || limit > incomingSMSBacklogLimit {
		limit = incomingSMSBacklogLimit
	}
	var rows []SMS
	err := DB.WithContext(ctx).
		Where("type = ? AND status = ? AND id > ?", smsTypeIncoming, smsStatusUnread, afterID).
		Order("id ASC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

func publishIncomingSMS(sms SMS) {
	if sms.Type != smsTypeIncoming || sms.ID == 0 {
		return
	}
	incomingSMS.publish(sms)
}

func publishIncomingSMSByID(id uint) {
	if DB == nil || id == 0 {
		return
	}
	var sms SMS
	if err := DB.First(&sms, id).Error; err != nil {
		return
	}
	publishIncomingSMS(sms)
}

func (hub *incomingSMSHub) publish(sms SMS) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for id, subscriber := range hub.subscribers {
		select {
		case subscriber <- sms:
		default:
			delete(hub.subscribers, id)
			close(subscriber)
		}
	}
}

func (hub *incomingSMSHub) subscribe() (<-chan SMS, func()) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.nextSubID++
	id := hub.nextSubID
	stream := make(chan SMS, incomingSMSSubscriberBuffer)
	hub.subscribers[id] = stream
	return stream, func() {
		hub.mu.Lock()
		if subscriber, ok := hub.subscribers[id]; ok {
			delete(hub.subscribers, id)
			close(subscriber)
		}
		hub.mu.Unlock()
	}
}
