package usagestore

import (
	"context"
	"database/sql"
	"fmt"
)

// deleteBatchRows bounds each delete transaction, so a large cleanup does not
// grow the WAL by the whole expired set at once.
const deleteBatchRows = 10000

// deleteExpired removes request rows and hourly totals older than the retention.
func (s *Store) deleteExpired(ctx context.Context) error {
	retention := *s.retention.Load()
	now := s.now()
	requestCutoff := now.AddDate(0, 0, -retention.RequestDays).UnixMilli()
	if err := deleteInBatches(ctx, s.db, `DELETE FROM requests WHERE id IN (SELECT id FROM requests WHERE ts_ms < ? LIMIT ?)`, requestCutoff); err != nil {
		return fmt.Errorf("delete expired requests: %w", err)
	}
	rollupCutoff := hourStart(now.AddDate(0, 0, -retention.RollupDays)).UnixMilli()
	if err := deleteInBatches(ctx, s.db, `DELETE FROM usage_hourly WHERE rowid IN (SELECT rowid FROM usage_hourly WHERE hour_ms < ? LIMIT ?)`, rollupCutoff); err != nil {
		return fmt.Errorf("delete expired hourly totals: %w", err)
	}
	return nil
}

func deleteInBatches(ctx context.Context, db *sql.DB, statement string, cutoffMs int64) error {
	for {
		result, err := db.ExecContext(ctx, statement, cutoffMs, deleteBatchRows)
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if deleted < deleteBatchRows {
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
	}
}
