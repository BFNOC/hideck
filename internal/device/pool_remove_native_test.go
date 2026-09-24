package device

import (
	"context"
	"testing"

	"github.com/yibaiba/hideck/internal/volte"
)

func TestRemoveWorkerRetiresNativeSession(t *testing.T) {
	p, port := testPhoneWorker(t)
	id := port.worker.ID
	p.volteCtl = volte.NewControllerWithBackup(&nativeStartEffectHost{}, t.TempDir())
	if err := p.volteCtl.Enable(context.Background(), id); err == nil {
		t.Fatal("expected failed native startup")
	}
	if err := p.RemoveWorker(id); err != nil {
		t.Fatal(err)
	}
	if got := p.volteCtl.Status(id); got.Phase != volte.PhaseIdle {
		t.Fatalf("removed worker retained native state: %+v", got)
	}
	p.mu.Lock()
	p.workers[id] = &Worker{ID: id, Config: port.worker.Config}
	p.mu.Unlock()
	if got := p.volteCtl.Status(id); got.Phase != volte.PhaseIdle {
		t.Fatalf("replacement inherited native state: %+v", got)
	}
}
