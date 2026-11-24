package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/pkg/logger"
)

// vowifiStartWaitLimit bounds how long a request holds the device locks while
// the SWu tunnel comes up. It stays under the 55s timeout the web UI sets for
// PUT /devices/:id (web/src/services/devices.ts). After the bound the start
// keeps running in the background; Disable, Restart, eSIM switch and device
// delete cancel it through the VoWiFi lifecycle controller. Until it ends, a
// repeated enable answers pending and changes that would race it answer
// rejectWhileVoWiFiStarting.
var vowifiStartWaitLimit = 40 * time.Second

// errVoWiFiStartingChange and errVoWiFiStartingRepeat abort the card policy
// write of a VoWiFi enable that arrives while a start is still running: the
// first when the effective call mode or data strategy differs from the running
// start, the second when it matches, the enable intent is saved and the request
// names no mode or data strategy other than the saved one.
var (
	errVoWiFiStartingChange = errors.New("vowifi start running: mode or data strategy change")
	errVoWiFiStartingRepeat = errors.New("vowifi start running: repeated enable")
)

// backgroundStarts counts the goroutines that still have to log the result of
// a backgrounded start. Tests wait on it before they swap the global logger.
var backgroundStarts sync.WaitGroup

// rejectWhileVoWiFiStarting answers 409 for a change that would race a VoWiFi
// start still running for deviceID. Callers check Pool.IsVoWiFiStarting under
// the device mutation lock, before the change it guards.
func rejectWhileVoWiFiStarting(c businessContext, deviceID, op string) {
	logger.Info("VoWiFi 正在连接，拒绝本次修改", "device", deviceID, "op", op)
	c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "VoWiFi 正在连接，请先关闭或等待"})
}

// answerVoWiFiStartPending answers a repeated enable while a VoWiFi start is
// still running for deviceID. policySaved tells whether this request saved the
// card policy first: the enable intent, or a mode or data strategy it names.
func answerVoWiFiStartPending(c businessContext, deviceID string, policySaved bool) {
	logger.Info("VoWiFi 正在连接，重复开启不再提交启动", "device", deviceID, "policy_saved", policySaved)
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"pending": true,
		"message": "VoWiFi 正在连接，结果请看设备概览",
		"device":  deviceID,
	})
}

// awaitVoWiFiStart runs start and returns its result when it finishes within
// vowifiStartWaitLimit. Otherwise it returns pending=true, leaves start running
// and only logs its eventual result. A panic in start becomes its error.
// devices only labels the logs: a device ID, or the comma-joined devices a
// card-policy restart reaches.
func awaitVoWiFiStart(devices, op string, start func() error) (pending bool, err error) {
	began, limit := time.Now(), vowifiStartWaitLimit
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("VoWiFi 启动 panic，已转为错误",
					"device", devices, "op", op, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				done <- fmt.Errorf("VoWiFi 启动 panic: %v", r)
			}
		}()
		done <- start()
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	if finished, err := startResultBy(done, timer.C); finished {
		return false, err
	}
	logger.Warn("VoWiFi 启动未在限时内完成，释放设备锁后在后台继续",
		"device", devices, "op", op, "waited_ms", time.Since(began).Milliseconds(), "limit_ms", limit.Milliseconds())
	backgroundStarts.Add(1)
	go func() {
		defer backgroundStarts.Done()
		err := <-done
		elapsed := time.Since(began).Milliseconds()
		switch {
		case errors.Is(err, context.Canceled):
			logger.Info("后台 VoWiFi 启动已取消", "device", devices, "op", op, "elapsed_ms", elapsed, "err", err)
		case err != nil:
			logger.Warn("后台 VoWiFi 启动失败", "device", devices, "op", op, "elapsed_ms", elapsed, "err", err)
		default:
			logger.Info("后台 VoWiFi 启动完成", "device", devices, "op", op, "elapsed_ms", elapsed)
		}
	}()
	return true, nil
}

// startResultBy waits for done until expired fires. A result already waiting
// when expired fires still counts, so a start that ends right at the limit is
// answered with its result instead of pending.
func startResultBy(done <-chan error, expired <-chan time.Time) (finished bool, err error) {
	select {
	case err := <-done:
		return true, err
	case <-expired:
	}
	select {
	case err := <-done:
		return true, err
	default:
		return false, nil
	}
}
