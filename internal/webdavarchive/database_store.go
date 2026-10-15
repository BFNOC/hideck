package webdavarchive

import (
	"context"

	"github.com/yibaiba/hideck/internal/db"
	"gorm.io/gorm"
)

type DatabaseStore struct{ database *gorm.DB }

func NewDatabaseStore(database *gorm.DB) *DatabaseStore {
	return &DatabaseStore{database: database}
}

func (store *DatabaseStore) ListSMS(ctx context.Context) ([]SMSRecord, error) {
	rows, err := db.ListArchiveSMS(ctx, store.database)
	if err != nil {
		return nil, err
	}
	result := make([]SMSRecord, 0, len(rows))
	for _, row := range rows {
		result = append(result, SMSRecord{
			ID: row.ID, IMSI: row.IMSI, ICCID: row.ICCID, Peer: row.Peer, LocalPhone: row.LocalPhone,
			Sender: row.Sender, Recipient: row.Recipient, Content: row.Content, Type: row.Type,
			Status: row.Status, Timestamp: row.Timestamp, Incomplete: row.Incomplete,
		})
	}
	return result, nil
}

func (store *DatabaseStore) ListCalls(ctx context.Context) ([]CallRecord, error) {
	rows, err := db.ListArchiveCalls(ctx, store.database)
	if err != nil {
		return nil, err
	}
	result := make([]CallRecord, 0, len(rows))
	for _, row := range rows {
		result = append(result, CallRecord{
			ID: row.ID, CallID: row.CallID, DeviceID: row.DeviceID, ICCID: row.ICCID,
			Direction: row.Direction, Peer: row.Peer, Status: row.Status,
			StartedAt: row.StartedAt, AnsweredAt: row.AnsweredAt, EndedAt: row.EndedAt,
			DurationSeconds: row.DurationSeconds, EndReason: row.EndReason, Codec: row.Codec,
			RecordingName: row.RecordingName, RecordingError: row.RecordingError,
		})
	}
	return result, nil
}

func (store *DatabaseStore) FindObject(ctx context.Context, kind, localKey string) (Object, bool, error) {
	row, found, err := db.FindArchiveObject(ctx, store.database, kind, localKey)
	return Object{
		Kind: row.Kind, LocalKey: row.LocalKey, Checksum: row.Checksum, RemotePath: row.RemotePath,
		SizeBytes: row.SizeBytes, SourceTime: row.SourceTime, ArchivedAt: row.ArchivedAt,
	}, found, err
}

func (store *DatabaseStore) UpsertObject(ctx context.Context, object Object) error {
	return db.UpsertArchiveObject(ctx, store.database, db.ArchiveObject{
		Kind: object.Kind, LocalKey: object.LocalKey, Checksum: object.Checksum,
		RemotePath: object.RemotePath, SizeBytes: object.SizeBytes,
		SourceTime: object.SourceTime, ArchivedAt: object.ArchivedAt,
	})
}

func (store *DatabaseStore) DeleteRecords(ctx context.Context, request DeleteRequest) error {
	return db.DeleteArchivedRecords(ctx, store.database, db.ArchiveDeleteRequest{
		SMSIDs: request.SMSIDs, CallIDs: request.CallIDs,
	})
}
