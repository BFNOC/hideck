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
