package webdavarchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/yibaiba/hideck/internal/db"
	"gorm.io/gorm"
)

type memoryRemote struct {
	mu       sync.Mutex
	objects  map[string][]byte
	uploads  int
	failNext error
}

func (remote *memoryRemote) Probe(context.Context) error { return nil }

func (remote *memoryRemote) Put(_ context.Context, upload Upload) error {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.failNext != nil {
		err := remote.failNext
		remote.failNext = nil
		return err
	}
	remote.uploads++
	remote.objects[upload.RemotePath] = append([]byte(nil), upload.Data...)
	return nil
}

func (remote *memoryRemote) PutFile(_ context.Context, upload FileUpload) error {
	data, err := os.ReadFile(upload.LocalPath)
	if err != nil {
		return err
	}
	return remote.Put(context.Background(), Upload{RemotePath: upload.RemotePath, Data: data})
}

func TestServiceArchivesIncrementallyAndCleansOnlyArchivedData(t *testing.T) {
	database := newArchiveTestDB(t)
	recordingDir := t.TempDir()
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -60)
	sms := db.SMS{
		IMSI: "234150000000001", ICCID: "8944100000000000001", Peer: "+447700900123",
		Sender: "+447700900123", Recipient: "+447700900999", Content: "archived", Type: 1,
		Status: 0, Timestamp: old,
	}
	if err := database.Create(&sms).Error; err != nil {
		t.Fatal(err)
	}
	contact := db.SMSContact{
		IMSI: sms.IMSI, ICCID: sms.ICCID, Peer: sms.Peer, LastSMSID: sms.ID,
		LastTimestamp: sms.Timestamp, LastContent: sms.Content, LastType: sms.Type, UnreadCount: 1,
	}
	if err := database.Create(&contact).Error; err != nil {
		t.Fatal(err)
	}
	ended := old.Add(2 * time.Minute)
	call := db.VoiceCallRecord{
		CallID: "call-1", DeviceID: "wwan0", ICCID: sms.ICCID, Direction: "inbound",
		Peer: sms.Peer, Status: "completed", StartedAt: old, EndedAt: &ended,
		DurationSeconds: 120, RecordingName: "call_old.mp3",
	}
	if err := database.Create(&call).Error; err != nil {
		t.Fatal(err)
	}
	recordingPath := filepath.Join(recordingDir, call.RecordingName)
	if err := os.WriteFile(recordingPath, []byte("audio-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	remote := &memoryRemote{objects: make(map[string][]byte)}
	service := newArchiveTestService(t, archiveTestServiceOptions{
		database: database, remote: remote, recordingDir: recordingDir, now: now,
	})
	settings := Settings{IncludeSMS: true, IncludeCalls: true, IncludeRecordings: true, RetentionDays: 30}
	first, err := service.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if first.Uploaded != 3 || first.Skipped != 0 {
		t.Fatalf("first summary = %+v", first)
	}
	if !archiveContains(remote, "sms/", `"iccid": "8944100000000000001"`) {
		t.Fatalf("SMS archive did not retain ICCID: %+v", remote.objects)
	}
	second, err := service.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if second.Uploaded != 0 || second.Skipped != 3 {
		t.Fatalf("second summary = %+v", second)
	}
	if err := database.Model(&db.SMS{}).Where("id = ?", sms.ID).Update("status", 1).Error; err != nil {
		t.Fatal(err)
	}
	third, err := service.Run(context.Background(), settings)
	if err != nil || third.Uploaded != 1 || third.Skipped != 2 {
		t.Fatalf("changed-data summary = %+v, %v", third, err)
	}

	unarchived := db.SMS{
		IMSI: sms.IMSI, ICCID: sms.ICCID, Peer: sms.Peer, Sender: sms.Sender, Recipient: sms.Recipient,
		Content: "keep locally", Type: 1, Status: 0, Timestamp: old.Add(24 * time.Hour),
	}
	if err := database.Create(&unarchived).Error; err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewCleanup(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if preview.SMSCount != 1 || preview.CallCount != 1 || preview.RecordingCount != 1 {
		t.Fatalf("preview = %+v", preview)
	}
	cleaned, err := service.Cleanup(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.SMSCount != 1 || cleaned.CallCount != 1 || cleaned.RecordingCount != 1 {
		t.Fatalf("cleaned = %+v", cleaned)
	}
	assertArchiveCleanupState(t, database, unarchived.ID, recordingPath)
}

func TestServiceRetriesUploadBeforeWritingLedger(t *testing.T) {
	database := newArchiveTestDB(t)
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	sms := db.SMS{IMSI: "imsi", ICCID: "iccid", Peer: "+1", Content: "retry", Type: 1, Timestamp: now}
	if err := database.Create(&sms).Error; err != nil {
		t.Fatal(err)
	}
	remote := &memoryRemote{objects: make(map[string][]byte), failNext: errors.New("dav unavailable")}
	service := newArchiveTestService(t, archiveTestServiceOptions{
		database: database, remote: remote, recordingDir: t.TempDir(), now: now,
	})
	if _, err := service.Run(context.Background(), Settings{IncludeSMS: true}); err == nil {
		t.Fatal("failed upload reported success")
	}
	var ledgerCount int64
	if err := database.Model(&db.ArchiveObject{}).Count(&ledgerCount).Error; err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 0 {
		t.Fatalf("ledger count after failed upload = %d", ledgerCount)
	}
	result, err := service.Run(context.Background(), Settings{IncludeSMS: true})
	if err != nil || result.Uploaded != 1 {
		t.Fatalf("retry result = %+v, %v", result, err)
	}
}

func TestMissingRecordingPreventsCallCleanup(t *testing.T) {
	database := newArchiveTestDB(t)
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	started := now.AddDate(0, 0, -60)
	ended := started.Add(time.Minute)
	call := db.VoiceCallRecord{
		CallID: "missing-recording", DeviceID: "wwan0", Direction: "inbound", Status: "completed",
		StartedAt: started, EndedAt: &ended, RecordingName: "missing.mp3",
	}
	if err := database.Create(&call).Error; err != nil {
		t.Fatal(err)
	}
	remote := &memoryRemote{objects: make(map[string][]byte)}
	service := newArchiveTestService(t, archiveTestServiceOptions{
		database: database, remote: remote, recordingDir: t.TempDir(), now: now,
	})
	settings := Settings{IncludeCalls: true, IncludeRecordings: true, RetentionDays: 30}
	if _, err := service.Run(context.Background(), settings); err == nil {
		t.Fatal("missing recording reported a successful archive run")
	}
	preview, err := service.PreviewCleanup(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if preview.CallCount != 0 || preview.RecordingCount != 0 {
		t.Fatalf("missing recording became cleanup eligible: %+v", preview)
	}
}

func newArchiveTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "archive.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&db.SMS{}, &db.SMSContact{}, &db.VoiceCallRecord{}, &db.ArchiveObject{}); err != nil {
		t.Fatal(err)
	}
	return database
}

type archiveTestServiceOptions struct {
	database     *gorm.DB
	remote       Remote
	recordingDir string
	now          time.Time
}

func newArchiveTestService(t *testing.T, options archiveTestServiceOptions) *Service {
	t.Helper()
	service, err := NewService(ServiceOptions{
		Store: NewDatabaseStore(options.database), Remote: options.remote, RecordingDir: options.recordingDir,
		Now: func() time.Time { return options.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func archiveContains(remote *memoryRemote, prefix, value string) bool {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	for path, data := range remote.objects {
		if strings.HasPrefix(path, prefix) && strings.Contains(string(data), value) {
			return true
		}
	}
	return false
}

func assertArchiveCleanupState(t *testing.T, database *gorm.DB, remainingSMS uint, recordingPath string) {
	t.Helper()
	var smsRows []db.SMS
	if err := database.Find(&smsRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(smsRows) != 1 || smsRows[0].ID != remainingSMS {
		t.Fatalf("remaining SMS = %+v", smsRows)
	}
	var contact db.SMSContact
	if err := database.First(&contact).Error; err != nil || contact.LastSMSID != remainingSMS {
		t.Fatalf("rebuilt contact = %+v, %v", contact, err)
	}
	var calls int64
	if err := database.Model(&db.VoiceCallRecord{}).Count(&calls).Error; err != nil || calls != 0 {
		t.Fatalf("remaining calls = %d, %v", calls, err)
	}
	if _, err := os.Stat(recordingPath); !os.IsNotExist(err) {
		t.Fatalf("recording still exists: %v", err)
	}
}
