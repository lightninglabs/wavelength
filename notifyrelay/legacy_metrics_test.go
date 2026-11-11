package notifyrelay

import (
	"context"

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
