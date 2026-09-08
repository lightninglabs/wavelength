package actordelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
)

// admitIngressDelivery consumes an ingress identity in the enqueue transaction.
// False means an identical event already reached a durable inbox. Failed
// enqueues roll back the receipt, and an in-memory Tell never calls this
// helper.
func admitIngressDelivery(ctx context.Context, q ActorDeliveryQueries,
	params actor.EnqueueParams, now time.Time) (bool, error) {

	id, ok := mailboxconn.IngressReceiptFromContext(ctx)
	if !ok {
		return true, nil
	}
	// Length-prefix the type so it cannot alias a payload prefix. Compare
	// the adapted durable message, rather than attempt UUIDs or wire
	// timestamps.
	fingerprint := sha256.Sum256(
		fmt.Appendf(
			nil, "%d:%s%s", len(params.MessageType),
			params.MessageType, params.Payload,
		),
	)
	count, err := q.AdmitIngressReceipt(
		ctx, adsqlc.AdmitIngressReceiptParams{
			ID:          id,
			PayloadHash: fingerprint[:],
			MailboxID:   params.MailboxID,
			ConsumedAt:  now.Unix(),
			ExpiresAt: now.
				Add(mailboxconn.IngressReceiptRetention).
				Unix(),
		},
	)
	if err != nil {
		return false, err
	}
	if count != 0 {
		return true, nil
	}
	receipt, err := q.GetIngressReceipt(ctx, id)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(receipt.PayloadHash, fingerprint[:]) {
		return false, fmt.Errorf("%w: consumed identity %s has a "+
			"different payload",
			mailboxconn.ErrIngressReceiptMismatch, id)
	}

	return false, nil
}

// PruneIngressReceipts deletes at most limit expired network receipts. Expiry
// is also enforced at admission, so a cleanup backlog cannot extend dedup. Each
// call opens a short write transaction; actor processed_messages is untouched.
func (s *Store) PruneIngressReceipts(ctx context.Context, limit int) (int64,
	error) {

	if limit <= 0 || limit > 4096 {
		return 0, fmt.Errorf("invalid ingress cleanup limit %d", limit)
	}
	var count int64
	err := s.db.ExecTx(
		ctx, db.WriteTxOption(),
		func(q ActorDeliveryQueries) error {
			var err error
			count, err = q.PruneIngressReceipts(
				ctx, adsqlc.PruneIngressReceiptsParams{
					ExpiresAt: s.clock.Now().Unix(),
					Limit:     int32(limit),
				},
			)

			return err
		},
	)

	return count, err
}
