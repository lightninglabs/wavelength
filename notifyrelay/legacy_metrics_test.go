package notifyrelay

import (
	"context"
	"time"

	bolt "go.etcd.io/bbolt"
)

// pendingJobs supports retired store regression fixtures, never the daemon.
func (s *Store) pendingJobs(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count = int64(tx.Bucket([]byte(jobsBucket)).Stats().KeyN)

		return nil
	})

	return count, err
}

// claimWithProgress adapts retired store fixtures to the production loop.
func (s *Store) claimWithProgress(ctx context.Context, now time.Time) (
	*Delivery, bool, error) {

	delivery, err := s.claim(ctx, now)

	return delivery, delivery != nil, err
}
