package indexer

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// parsePsbtTx deserializes a raw PSBT blob and returns the embedded
// unsigned wire transaction. Used by the lineage-vbytes accounting walk
// to reach into persisted OOR session psbts (`session.ArkPsbt`,
// `cp.Psbt`) that the resolver does not deserialize on its own.
func parsePsbtTx(raw []byte) (*wire.MsgTx, error) {
	pkt, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return nil, fmt.Errorf("parse psbt: %w", err)
	}
	if pkt == nil || pkt.UnsignedTx == nil {
		return nil, fmt.Errorf("psbt missing unsigned tx")
	}

	return pkt.UnsignedTx, nil
}

// EstimateOORLineageVBytes is the public entrypoint into the lineage
// vbytes calculation. Wraps the per-call resolver and walks every
// input's ancestry to produce a cumulative cap-arithmetic value.
// Callers (OOR submit cap check, future fee schedule) should treat the
// returned uint32 as witness-discounted virtual bytes.
//
// The entire walk runs inside a single read transaction so the per-
// call store queries (GetVTXO, GetOORSession, ListOORCheckpoints,
// GetRound, LoadVTXOTree) see a consistent snapshot of the ancestry
// graph. Without this snapshot, a parallel OOR submit appending
// checkpoints between the operator's cap read and the eventual
// LockInputsReq could push the effective cost over the cap; the
// snapshot eliminates that intra-cap inconsistency.
//
// Errors are categorized as internal failures (resolver/store lookup
// errors); the caller is responsible for translating them to the
// appropriate user-visible result and never as a typed cap rejection.
func EstimateOORLineageVBytes(ctx context.Context, store Store,
	inputs []wire.OutPoint, ark *psbt.Packet,
	checkpoints []*psbt.Packet) (uint32, error) {

	if store == nil {
		return 0, fmt.Errorf("vbytes calc: store must be provided")
	}

	var used uint32
	err := store.ExecReadTx(ctx, func(q Store) error {
		resolver := newLineageResolver(q, nil)

		v, err := LineageVBytes(
			ctx, q, resolver, inputs, ark, checkpoints,
		)
		if err != nil {
			return err
		}

		used = v

		return nil
	})
	if err != nil {
		return 0, err
	}

	return used, nil
}

// LineageVBytes returns the cumulative on-chain virtual bytes required
// to claim a VTXO produced by an OOR submit unilaterally. The number is
// the sum of unique-by-txid signed-tx vbytes across:
//
//   - every tree node in every ancestry fragment of every input VTXO,
//   - every OOR Ark + checkpoint tx that bridges parent VTXOs through
//     prior OOR hops (chain_depth > 0),
//   - the new submit's checkpoint set, and
//   - the new submit's Ark tx itself.
//
// Vbytes follow the standard `(base*3 + total + 3) / 4` formula so the
// witness discount is accounted for. De-duplicating by txid mirrors
// `client/unroll/descriptor_resolver.go` `seen` semantics: a tree node
// shared between two inputs is broadcast once on-chain, so it is also
// counted once in the cap arithmetic.
//
// This helper is the single source of truth that both the OOR submit
// validator (operator-side cap enforcement) and the client coin
// selector (pre-submit guard) consume, so no "client said OK / server
// said reject" arithmetic divergence can arise.
func LineageVBytes(ctx context.Context, store Store, resolver *lineageResolver,
	inputOutpoints []wire.OutPoint, ark *psbt.Packet,
	checkpoints []*psbt.Packet) (uint32, error) {

	if store == nil {
		return 0, fmt.Errorf("vbytes calc: store must be provided")
	}

	if resolver == nil {
		return 0, fmt.Errorf("vbytes calc: resolver must be provided")
	}

	seen := make(map[chainhash.Hash]struct{})
	var total uint64

	addTx := func(tx *wire.MsgTx) {
		if tx == nil {
			return
		}
		txid := tx.TxHash()
		if _, ok := seen[txid]; ok {
			return
		}
		seen[txid] = struct{}{}
		total += uint64(txVBytes(tx))
	}

	// Walk each input's ancestry. Errors here are categorized as
	// internal (resolver unable to determine cumulative cost) rather
	// than the typed cap rejection — the operator should not surface
	// "lineage too large" when the underlying lookup failed.
	for _, op := range inputOutpoints {
		row, err := store.GetVTXO(ctx, op)
		if err != nil {
			return 0, fmt.Errorf("lookup parent vtxo %s: %w", op,
				err)
		}

		lineage, err := resolver.Resolve(ctx, row)
		if err != nil {
			return 0, fmt.Errorf("resolve lineage for %s: %w", op,
				err)
		}

		// Walk every tree node across every ancestry fragment.
		// tree.Tree iterates pre-order; signed tree-node txs carry
		// witnesses so vbytes accounts for them via txVBytes.
		for _, fragment := range lineage.ancestryPaths {
			if fragment.treePath == nil ||
				fragment.treePath.Root == nil {

				continue
			}

			for node := range fragment.treePath.Root.NodesIter() {
				signedTx, err := node.ToSignedTx()
				if err != nil {
					// Skip degenerate nodes; the proof
					// chain will reject them
					// downstream if relevant. They do
					// not contribute to cap arithmetic.
					continue
				}

				addTx(signedTx)
			}
		}

		// Walk OOR ancestor txs (Ark + checkpoint) for parents that
		// were themselves received via OOR. resolver tracks chain
		// depth; non-zero chain depth implies the lineage walked
		// through prior OOR sessions whose txs must be republished
		// at exit time.
		if lineage.chainDepth > 0 {
			err := walkOORAncestry(
				ctx, store, resolver, op, addTx,
			)
			if err != nil {
				return 0, fmt.Errorf("walk oor ancestry for "+
					"%s: %w", op, err)
			}
		}
	}

	// Add the new submit's own bytes: every checkpoint plus the Ark
	// tx itself. These are not in the parent ancestry; they are the
	// new layer the recipient must publish on top.
	for _, cp := range checkpoints {
		if cp == nil || cp.UnsignedTx == nil {
			continue
		}

		addTx(cp.UnsignedTx)
	}
	if ark != nil && ark.UnsignedTx != nil {
		addTx(ark.UnsignedTx)
	}

	return narrowVBytesTotal(total)
}

// narrowVBytesTotal narrows the running uint64 sum into the uint32 wire
// width the cap-arithmetic surface uses. A total that exceeds uint32
// range cannot be represented without losing information; saturating
// at MaxUint32 would silently mask a real lineage above the cap (an
// operator running with `MaxOORLineageVBytes == MaxUint32` to mean
// "no cap" would then accept submits whose true cost exceeded uint32
// and was indistinguishable from an honestly computed MaxUint32
// measurement). Fail closed so the OOR submit path routes through
// ErrLineageWeightInternal rather than treating the saturated value
// as a real measurement.
func narrowVBytesTotal(total uint64) (uint32, error) {
	if total > uint64(^uint32(0)) {
		return 0, fmt.Errorf("lineage vbytes overflow: total=%d "+
			"exceeds uint32 max", total)
	}

	return uint32(total), nil
}

// walkOORAncestry traverses the OOR session chain rooted at outpoint
// and hands every Ark and checkpoint tx to addTx. The resolver caches
// session and checkpoint lookups so repeated calls within a single
// LineageVBytes invocation only hit the store once per session.
//
// Depth bound and cycle protection are inherited from
// walkOORSessionAncestryDriver, mirroring the recipient-events path so
// a corrupted A→B→A back-edge or a pathologically deep chain rejects
// instead of infinite-recursing or amplifying CPU under fanout.
//
// Persisted-PSBT parse failures fail closed: a malformed ancestor PSBT
// returns an error rather than silently zero-counting, since under-
// counting the cap would let a child submit that should have been
// rejected pass the operator's threshold.
func walkOORAncestry(ctx context.Context, store Store, r *lineageResolver,
	outpoint wire.OutPoint, addTx func(*wire.MsgTx)) error {

	// Treat the outpoint hash as a session id; this matches the
	// existing resolveVirtual contract where OOR-created VTXOs
	// inherit their producing session id from the Ark txid.
	startSessionID := append([]byte(nil), outpoint.Hash[:]...)

	pre := func(ctx context.Context, curID []byte, depth int) ([][]byte,
		error) {

		session, err := r.resolveSession(ctx, curID)
		if err != nil {
			return nil, fmt.Errorf("resolve session: %w", err)
		}
		if session == nil {
			return nil, nil
		}

		checkpoints, err := r.resolveSessionCheckpoints(
			ctx, curID, session,
		)
		if err != nil {
			return nil, fmt.Errorf("resolve checkpoints: %w", err)
		}

		if len(session.ArkPsbt) > 0 {
			tx, err := parsePsbtTx(session.ArkPsbt)
			if err != nil {
				return nil, fmt.Errorf("parse persisted ark "+
					"psbt for session %x: %w", curID, err)
			}
			addTx(tx)
		}
		for _, cp := range checkpoints {
			if cp.Psbt == nil || cp.Psbt.UnsignedTx == nil {
				continue
			}
			addTx(cp.Psbt.UnsignedTx)
		}

		// Extract the parent outpoints this session consumed and
		// keep only the parents that resolve to OOR-produced VTXOs
		// (RoundID == nil). Round-direct parents contribute through
		// the regular round-backed ancestry walk; following them
		// here would double-count or, worse, trip the seen-set on
		// distinct sessions that happen to share a parent outpoint.
		parents, err := sessionParentOutpoints(session, checkpoints)
		if err != nil {
			return nil, fmt.Errorf("session parents: %w", err)
		}

		parentSessionIDs := make([][]byte, 0, len(parents))
		for _, parent := range parents {
			row, err := store.GetVTXO(ctx, parent)
			if err != nil {
				// A parent outpoint that no longer resolves
				// likely means it was already round-confirmed
				// and pruned. That parent contributes its
				// tree path through the regular ancestry
				// walk; don't double-error here.
				continue
			}

			if row.RoundID != nil {
				// Round-direct parent: no further OOR
				// ancestry.
				continue
			}

			parentSessionIDs = append(
				parentSessionIDs,
				append(
					[]byte(nil), parent.Hash[:]...,
				),
			)
		}

		return parentSessionIDs, nil
	}

	return walkOORSessionAncestryDriver(ctx, startSessionID, pre, nil)
}

// txVBytes computes the virtual-byte size of a signed wire transaction
// using the standard witness-discounted weight unit:
//
//	weight = base*3 + total
//	vbytes = (weight + 3) / 4
//
// where base is the size with witness data stripped and total is the
// full serialized size. The +3 rounds up so a tx with weight 4 is one
// vB rather than zero.
func txVBytes(tx *wire.MsgTx) int {
	if tx == nil {
		return 0
	}

	base := tx.SerializeSizeStripped()
	full := tx.SerializeSize()
	weight := base*3 + full

	return (weight + 3) / 4
}
