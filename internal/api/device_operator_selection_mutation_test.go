package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/device"
)

func TestSetOperatorSelectionWaitsForManagedDeviceMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pool := device.NewPool(&config.Config{})
	t.Cleanup(func() { _ = pool.Shutdown() })
	releaseMutation := pool.AcquireManagedDeviceMutation("dev-1")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "device_id", Value: "dev-1"}}
	ctx.Request = httptest.NewRequest(http.MethodPut, "/devices/dev-1/operator-selection",
		strings.NewReader(`{"mode":"automatic"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Server{pool: pool}).handleDeviceMgmtSetOperatorSelection(ctx)
	}()

	select {
	case <-done:
		t.Fatal("operator selection bypassed the managed device mutation lease")
	case <-time.After(20 * time.Millisecond):
	}
	releaseMutation()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("operator selection did not resume after mutation lease release")
	}
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDeviceActionsWaitForManagedDeviceMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		method string
		body   string
		handle func(*Server, *gin.Context)
	}{
		{name: "flight mode", method: http.MethodPut, body: `{"enabled":true}`, handle: (*Server).handleDeviceMgmtSetFlightMode},
		{name: "reboot", method: http.MethodPost, handle: (*Server).handleDeviceMgmtReboot},
		{name: "start network", method: http.MethodPost, handle: (*Server).handleDeviceMgmtStartNetwork},
		{name: "stop network", method: http.MethodPost, handle: (*Server).handleDeviceMgmtStopNetwork},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := device.NewPool(&config.Config{})
			t.Cleanup(func() { _ = pool.Shutdown() })
			releaseMutation := pool.AcquireManagedDeviceMutation("dev-1")
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "device_id", Value: "dev-1"}}
			ctx.Request = httptest.NewRequest(test.method, "/devices/dev-1/action", strings.NewReader(test.body))
			if test.body != "" {
				ctx.Request.Header.Set("Content-Type", "application/json")
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				test.handle(&Server{pool: pool}, ctx)
			}()

			select {
			case <-done:
				t.Fatal("device action bypassed the managed mutation lease")
			case <-time.After(20 * time.Millisecond):
			}
			releaseMutation()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("device action did not resume after mutation lease release")
			}
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestRefreshManagedWorkerRuntimeRejectsReplacement(t *testing.T) {
	pool := device.NewPool(&config.Config{})
	t.Cleanup(func() { _ = pool.Shutdown() })
	oldWorker := &device.Worker{ID: "dev-1"}
	replacement := &device.Worker{ID: oldWorker.ID}
	setNestedPrivateField(t, pool, []string{"workers"}, map[string]*device.Worker{replacement.ID: replacement})
	if (&Server{pool: pool}).refreshManagedWorkerRuntime(oldWorker, "stale_refresh") {
		t.Fatal("stale worker refresh was accepted after replacement")
	}
}
