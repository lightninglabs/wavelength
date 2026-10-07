package waved

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testFailedOutgoingSnapshot encodes a failed outgoing snapshot with the
// record types the oor codec uses: version (1), session id (3), phase (5), the
// empty checkpoint and input lists (9 and 11, each a zero element count), and
// the optional failed-before-PONR flag (27). A nil flag omits the record, as a
// snapshot written before the flag existed does.
func testFailedOutgoingSnapshot(t *testing.T, sessionID chainhash.Hash,
	prePONR *bool) []byte {

	t.Helper()

	version := uint64(6)
	session := sessionID[:]
	phase := []byte("failed")
	emptyList := []byte{0x00}
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(tlv.Type(1), &version),
		tlv.MakePrimitiveRecord(tlv.Type(3), &session),
		tlv.MakePrimitiveRecord(tlv.Type(5), &phase),
		tlv.MakePrimitiveRecord(tlv.Type(9), &emptyList),
		tlv.MakePrimitiveRecord(tlv.Type(11), &emptyList),
	}
	if prePONR != nil {
		var flag uint8
		if *prePONR {
			flag = 1
		}
		records = append(
			records,
			tlv.MakePrimitiveRecord(
				tlv.Type(27), &flag,
			),
		)
	}

	stream, err := tlv.NewStream(records...)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, stream.Encode(&buf))

	return buf.Bytes()
}

// TestSendOORReplayReportsFailureOrigin verifies a keyed replay of a durably
// failed outgoing OOR reports failed_before_ponr only when the session
// recorded that it failed before the point of no return. A post-PONR failure,
// a failure from before the origin was recorded, and a snapshot that cannot be
// decoded all report a plain failure, because the operator may hold a
// co-signed spend and the recipient output may exist.
func TestSendOORReplayReportsFailureOrigin(t *testing.T) {
	t.Parallel()

	yes, no := true, false
	tests := []struct {
		name     string
		prePONR  *bool
		raw      []byte
		status   db.OORSessionStatus
		wantFail bool
		wantPre  bool
	}{
		{
			name:     "failed before ponr",
			prePONR:  &yes,
			status:   db.OORSessionStatusFailed,
			wantFail: true,
			wantPre:  true,
		},
		{
			name:     "failed after ponr",
			prePONR:  &no,
			status:   db.OORSessionStatusFailed,
			wantFail: true,
		},
		{
			name:     "failed with unrecorded origin",
			status:   db.OORSessionStatusFailed,
			wantFail: true,
		},
		{
			name: "failed with undecodable snapshot",
			raw: []byte{
				0xff,
			},
			status:   db.OORSessionStatusFailed,
			wantFail: true,
		},
		{
			name:    "pending is not a failure",
			prePONR: &yes,
			status:  db.OORSessionStatusPending,
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, _, registryStore, _, _ := newSendOORTestStores(t)
			sessionID := chainhash.HashH([]byte{byte(i)})

			raw := test.raw
			if raw == nil {
				raw = testFailedOutgoingSnapshot(
					t, sessionID, test.prePONR,
				)
			}
			outgoing := db.OORSessionDirectionOutgoing
			record := db.OORSessionRegistryRecord{
				SessionID:       sessionID,
				ActorID:         "actor",
				Direction:       outgoing,
				Phase:           "failed",
				Status:          test.status,
				SnapshotData:    raw,
				SnapshotVersion: 6,
			}
			err := registryStore.UpsertSession(t.Context(), record)
			require.NoError(t, err)

			rpcServer := NewRPCServer(&Server{
				cfg:             &Config{},
				log:             btclog.Disabled,
				oorSessionStore: registryStore,
			})

			replay, prePONR, err := rpcServer.
				outgoingOORReplayStatus(
					t.Context(), oor.SessionID(sessionID),
				)
			require.NoError(t, err)
			require.Equal(t, test.wantPre, prePONR)
			if test.wantFail {
				require.Equal(t, "failed", replay)
			} else {
				require.Equal(t, "submitted", replay)
			}
		})
	}
}

// TestOORSessionInfoReportsFailureOrigin verifies GetOORSession and
// ListOORSessions set failed_before_ponr only for an outgoing failed session
// that recorded a pre-PONR failure, and never for a session that has not
// failed or whose origin is not recorded.
func TestOORSessionInfoReportsFailureOrigin(t *testing.T) {
	t.Parallel()

	yes, no := true, false
	tests := []struct {
		name    string
		prePONR *bool
		status  db.OORSessionStatus
		want    bool
	}{
		{
			name:    "failed before ponr",
			prePONR: &yes,
			status:  db.OORSessionStatusFailed,
			want:    true,
		},
		{
			name:    "failed after ponr",
			prePONR: &no,
			status:  db.OORSessionStatusFailed,
		},
		{
			name:   "failed with unrecorded origin",
			status: db.OORSessionStatusFailed,
		},
		{
			name:    "pending",
			prePONR: &yes,
			status:  db.OORSessionStatusPending,
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, _, registryStore, _, _ := newSendOORTestStores(t)
			sessionID := chainhash.HashH([]byte{0x40, byte(i)})
			raw := testFailedOutgoingSnapshot(
				t, sessionID, test.prePONR,
			)
			outgoing := db.OORSessionDirectionOutgoing
			record := db.OORSessionRegistryRecord{
				SessionID:       sessionID,
				ActorID:         "actor",
				Direction:       outgoing,
				Phase:           "failed",
				Status:          test.status,
				SnapshotData:    raw,
				SnapshotVersion: 6,
			}
			err := registryStore.UpsertSession(t.Context(), record)
			require.NoError(t, err)

			rpcServer := NewRPCServer(&Server{
				cfg:             &Config{},
				log:             btclog.Disabled,
				oorSessionStore: registryStore,
			})

			info := rpcServer.oorStatusToProto(
				t.Context(), &db.OORStatusSummary{
					Metadata: sqlc.OorStatus{
						SessionID: sessionID[:],
						Direction: int32(outgoing),
						Status:    int32(test.status),
						Phase:     "failed",
					},
				},
			)
			require.Equal(t, test.want, info.FailedBeforePonr)

			// An incoming session never reports an outgoing
			// failure origin, even over the same registry row.
			incoming := waverpc.OORSessionDirection(
				db.OORSessionDirectionIncoming,
			)
			info = rpcServer.oorStatusToProto(
				t.Context(), &db.OORStatusSummary{
					Metadata: sqlc.OorStatus{
						SessionID: sessionID[:],
						Direction: int32(incoming),
						Status:    int32(test.status),
						Phase:     "failed",
					},
				},
			)
			require.False(t, info.FailedBeforePonr)
		})
	}
}
