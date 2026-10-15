package webdavarchive

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func (service *Service) PreviewCleanup(ctx context.Context, settings Settings) (CleanupPreview, error) {
	if settings.RetentionDays <= 0 {
		return CleanupPreview{}, fmt.Errorf("本地保留天数必须大于 0")
	}
	preview := CleanupPreview{Before: service.now().AddDate(0, 0, -settings.RetentionDays).UTC()}
	if settings.IncludeSMS {
		if err := service.collectCleanupSMS(ctx, &preview); err != nil {
			return CleanupPreview{}, err
		}
	}
	if settings.IncludeCalls {
		if err := service.collectCleanupCalls(ctx, settings, &preview); err != nil {
			return CleanupPreview{}, err
		}
	}
	return preview, nil
}

func (service *Service) Cleanup(ctx context.Context, settings Settings) (CleanupPreview, error) {
	preview, err := service.PreviewCleanup(ctx, settings)
	if err != nil {
		return CleanupPreview{}, err
	}
	request := DeleteRequest{SMSIDs: preview.EligibleSMSIDs, CallIDs: preview.EligibleCallIDs}
	if err := service.store.DeleteRecords(ctx, request); err != nil {
		return CleanupPreview{}, fmt.Errorf("删除已归档本地记录失败: %w", err)
	}
	for _, path := range preview.RecordingFiles {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return preview, fmt.Errorf("删除已归档录音 %q 失败: %w", path, err)
		}
	}
	return preview, nil
}

func (service *Service) collectCleanupSMS(ctx context.Context, preview *CleanupPreview) error {
	rows, err := service.store.ListSMS(ctx)
	if err != nil {
		return fmt.Errorf("读取短信记录失败: %w", err)
	}
	for _, row := range rows {
		if !row.Timestamp.Before(preview.Before) {
			continue
		}
		data, err := marshalArchiveJSON(row)
		if err != nil {
			return err
		}
		item := newDataItem(KindSMS, strconv.FormatUint(uint64(row.ID), 10), row.Timestamp, data)
		matches, err := service.objectMatches(ctx, item.object)
		if err != nil {
			return err
		}
		if matches {
			preview.EligibleSMSIDs = append(preview.EligibleSMSIDs, row.ID)
			preview.SMSCount++
		}
	}
	return nil
}

func (service *Service) collectCleanupCalls(ctx context.Context, settings Settings, preview *CleanupPreview) error {
	rows, err := service.store.ListCalls(ctx)
	if err != nil {
		return fmt.Errorf("读取通话记录失败: %w", err)
	}
	for _, row := range rows {
		if !endedBefore(row, preview.Before) {
			continue
		}
		archived, err := service.callArchived(ctx, row)
		if err != nil {
			return err
		}
		if !archived {
			continue
		}
		files, bytes, safe, err := service.cleanupRecording(ctx, settings, row)
		if err != nil {
			return err
		}
		if !safe {
			continue
		}
		preview.EligibleCallIDs = append(preview.EligibleCallIDs, row.ID)
		preview.CallCount++
		preview.RecordingFiles = append(preview.RecordingFiles, files...)
		preview.RecordingCount += len(files)
		preview.RecordingBytes += bytes
	}
	return nil
}

func (service *Service) callArchived(ctx context.Context, row CallRecord) (bool, error) {
	data, err := marshalArchiveJSON(row)
	if err != nil {
		return false, err
	}
	item := newDataItem(KindCall, strconv.FormatUint(row.ID, 10), row.StartedAt, data)
	return service.objectMatches(ctx, item.object)
}

func (service *Service) cleanupRecording(ctx context.Context, settings Settings, row CallRecord) ([]string, int64, bool, error) {
	name := strings.TrimSpace(row.RecordingName)
	if name == "" {
		return nil, 0, true, nil
	}
	if !settings.IncludeRecordings {
		return nil, 0, false, nil
	}
	path, err := safeRecordingPath(service.recordingDir, name)
	if err != nil {
		return nil, 0, false, nil
	}
	checksum, size, err := hashFile(path)
	if os.IsNotExist(err) {
		_, found, findErr := service.store.FindObject(ctx, KindRecording, name)
		return nil, 0, found, findErr
	}
	if err != nil {
		return nil, 0, false, nil
	}
	object := Object{Kind: KindRecording, LocalKey: name, Checksum: checksum}
	matches, err := service.objectMatches(ctx, object)
	if err != nil || !matches {
		return nil, 0, false, err
	}
	return []string{path}, size, true, nil
}

func (service *Service) objectMatches(ctx context.Context, expected Object) (bool, error) {
	stored, found, err := service.store.FindObject(ctx, expected.Kind, expected.LocalKey)
	if err != nil {
		return false, fmt.Errorf("读取归档台账失败: %w", err)
	}
	return found && stored.Checksum == expected.Checksum, nil
}

func endedBefore(row CallRecord, cutoff time.Time) bool {
	return row.EndedAt != nil && row.EndedAt.Before(cutoff)
}
