package oor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// TaprootAssetTransferVersion is the current Wavelength OOR asset
	// extension schema.
	TaprootAssetTransferVersion uint16 = 0

	// MaxTaprootAssetCheckpointPackages bounds the number of asset-bearing
	// checkpoint edges in one OOR graph. It intentionally matches the OOR
	// checkpoint limit enforced by the client and operator.
	MaxTaprootAssetCheckpointPackages = 64

	// MaxTaprootAssetPackageBytes bounds one sealed tap-sdk package. Proof
	// sources are intentionally retained in those packages, so this limit
	// is larger than the PSBT-only transport limit.
	MaxTaprootAssetPackageBytes = 64 * 1024 * 1024

	// MaxTaprootAssetTransferBytes bounds the complete encoded extension.
	// The first showcase is expected to use one or a small number of
	// inputs; this cap prevents checkpoint count and proof size multiplying
	// without bound before the operator can inspect the packages.
	MaxTaprootAssetTransferBytes = 128 * 1024 * 1024

	taprootAssetTransferMagic        = "WAVTAPOR"
	taprootAssetTransferChecksumSize = sha256.Size
	taprootAssetTransferHeaderSize   = len(taprootAssetTransferMagic) + 2 +
		2
)

var (
	// ErrTaprootAssetTransferInvalid reports a malformed or internally
	// inconsistent OOR asset extension.
	ErrTaprootAssetTransferInvalid = errors.New("invalid Taproot Asset " +
		"OOR transfer")

	// ErrTaprootAssetTransferVersion reports an unsupported extension
	// version.
	ErrTaprootAssetTransferVersion = errors.New("unsupported Taproot " +
		"Asset OOR transfer version")
)

// TaprootAssetTransfer contains the sealed tap-sdk recovery packages for all
// asset-moving edges in one Wavelength OOR graph. CheckpointPackages are in
// the same order as the submitted checkpoint PSBTs; ArkPackage describes the
// final checkpoint-to-recipient transition.
//
// The package bytes remain opaque at this shared layer. The client-side
// tap-sdk adapter creates them and the operator-side validator opens and
// validates them. This keeps taproot-assets implementation types out of the
// OOR transaction primitives.
type TaprootAssetTransfer struct {
	// Version identifies the container schema.
	Version uint16

	// CheckpointPackages contains one positional slot per checkpoint edge.
	// A non-empty slot is the sealed package for an asset-bearing
	// checkpoint; an empty slot marks an ordinary Bitcoin-only checkpoint.
	CheckpointPackages [][]byte

	// ArkPackage is the sealed package for the final Ark edge.
	ArkPackage []byte
}

// Validate checks bounded container invariants. expectedCheckpoints binds the
// extension to an OOR checkpoint set when non-negative.
func (t *TaprootAssetTransfer) Validate(expectedCheckpoints int) error {
	if t == nil {
		return fmt.Errorf("%w: transfer is nil",
			ErrTaprootAssetTransferInvalid)
	}
	if t.Version != TaprootAssetTransferVersion {
		return fmt.Errorf("%w: %d", ErrTaprootAssetTransferVersion,
			t.Version)
	}
	if len(t.CheckpointPackages) == 0 {
		return fmt.Errorf("%w: checkpoint packages are required",
			ErrTaprootAssetTransferInvalid)
	}
	if len(t.CheckpointPackages) > MaxTaprootAssetCheckpointPackages {
		return fmt.Errorf("%w: checkpoint package count %d exceeds %d",
			ErrTaprootAssetTransferInvalid,
			len(t.CheckpointPackages),
			MaxTaprootAssetCheckpointPackages)
	}
	if expectedCheckpoints >= 0 &&
		len(t.CheckpointPackages) != expectedCheckpoints {
		return fmt.Errorf("%w: checkpoint package count %d does not "+
			"match checkpoint count %d",
			ErrTaprootAssetTransferInvalid,
			len(t.CheckpointPackages), expectedCheckpoints)
	}

	var (
		total         uint64
		assetPackages int
	)
	for i := range t.CheckpointPackages {
		total += uint64(len(t.CheckpointPackages[i]))
		if len(t.CheckpointPackages[i]) == 0 {
			continue
		}
		assetPackages++
		if err := validateTaprootAssetPackage(
			fmt.Sprintf("checkpoint package %d", i),
			t.CheckpointPackages[i],
		); err != nil {
			return err
		}
	}
	if assetPackages == 0 {
		return fmt.Errorf("%w: at least one asset-bearing checkpoint "+
			"package is required", ErrTaprootAssetTransferInvalid)
	}
	total += uint64(len(t.ArkPackage))
	if err := validateTaprootAssetPackage(
		"Ark package", t.ArkPackage,
	); err != nil {
		return err
	}
	if total > MaxTaprootAssetTransferBytes {
		return fmt.Errorf("%w: package payload size %d exceeds %d",
			ErrTaprootAssetTransferInvalid, total,
			MaxTaprootAssetTransferBytes)
	}

	return nil
}

// Clone returns a deep copy suitable for ownership by a durable FSM state.
func (t *TaprootAssetTransfer) Clone() *TaprootAssetTransfer {
	if t == nil {
		return nil
	}

	clone := &TaprootAssetTransfer{
		Version:            t.Version,
		CheckpointPackages: make([][]byte, len(t.CheckpointPackages)),
		ArkPackage:         bytes.Clone(t.ArkPackage),
	}
	for i := range t.CheckpointPackages {
		clone.CheckpointPackages[i] = bytes.Clone(
			t.CheckpointPackages[i],
		)
	}

	return clone
}

// MarshalBinary returns a versioned, checksummed representation suitable for
// snapshots and non-protobuf persistence.
func (t *TaprootAssetTransfer) MarshalBinary() ([]byte, error) {
	if err := t.Validate(-1); err != nil {
		return nil, err
	}

	var body bytes.Buffer
	body.Grow(taprootAssetTransferHeaderSize + len(t.ArkPackage))
	body.WriteString(taprootAssetTransferMagic)
	if err := binary.Write(&body, binary.BigEndian, t.Version); err != nil {
		return nil, err
	}
	count := uint16(len(t.CheckpointPackages))
	if err := binary.Write(&body, binary.BigEndian, count); err != nil {
		return nil, err
	}
	for i := range t.CheckpointPackages {
		if err := writeTaprootAssetCheckpointSlot(
			&body, t.CheckpointPackages[i],
		); err != nil {
			return nil, err
		}
	}
	if err := writeTaprootAssetPackage(&body, t.ArkPackage); err != nil {
		return nil, err
	}

	checksum := sha256.Sum256(body.Bytes())
	body.Write(checksum[:])
	if body.Len() > MaxTaprootAssetTransferBytes {
		return nil, fmt.Errorf("%w: encoded size %d exceeds %d",
			ErrTaprootAssetTransferInvalid, body.Len(),
			MaxTaprootAssetTransferBytes)
	}

	return body.Bytes(), nil
}

// UnmarshalBinary decodes and validates a versioned OOR asset extension.
func (t *TaprootAssetTransfer) UnmarshalBinary(encoded []byte) error {
	if t == nil {
		return fmt.Errorf("%w: nil receiver",
			ErrTaprootAssetTransferInvalid)
	}
	if len(encoded) > MaxTaprootAssetTransferBytes {
		return fmt.Errorf("%w: encoded size %d exceeds %d",
			ErrTaprootAssetTransferInvalid, len(encoded),
			MaxTaprootAssetTransferBytes)
	}
	minimum := taprootAssetTransferHeaderSize + 4 +
		taprootAssetTransferChecksumSize
	if len(encoded) < minimum {
		return fmt.Errorf("%w: encoded transfer is truncated",
			ErrTaprootAssetTransferInvalid)
	}

	body := encoded[:len(encoded)-taprootAssetTransferChecksumSize]
	wantChecksum := encoded[len(body):]
	checksum := sha256.Sum256(body)
	if !bytes.Equal(checksum[:], wantChecksum) {
		return fmt.Errorf("%w: checksum mismatch",
			ErrTaprootAssetTransferInvalid)
	}

	reader := bytes.NewReader(body)
	magic := make([]byte, len(taprootAssetTransferMagic))
	if _, err := io.ReadFull(reader, magic); err != nil {
		return fmt.Errorf("%w: read magic: %w",
			ErrTaprootAssetTransferInvalid, err)
	}
	if string(magic) != taprootAssetTransferMagic {
		return fmt.Errorf("%w: invalid magic",
			ErrTaprootAssetTransferInvalid)
	}

	var version uint16
	if err := binary.Read(reader, binary.BigEndian, &version); err != nil {
		return fmt.Errorf("%w: read version: %w",
			ErrTaprootAssetTransferInvalid, err)
	}
	if version != TaprootAssetTransferVersion {
		return fmt.Errorf("%w: %d", ErrTaprootAssetTransferVersion,
			version)
	}

	var count uint16
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return fmt.Errorf("%w: read checkpoint count: %w",
			ErrTaprootAssetTransferInvalid, err)
	}
	if count == 0 || count > MaxTaprootAssetCheckpointPackages {
		return fmt.Errorf("%w: invalid checkpoint package count %d",
			ErrTaprootAssetTransferInvalid, count)
	}

	decoded := TaprootAssetTransfer{
		Version:            version,
		CheckpointPackages: make([][]byte, count),
	}
	for i := range decoded.CheckpointPackages {
		pkg, err := readTaprootAssetCheckpointSlot(reader)
		if err != nil {
			return fmt.Errorf("%w: checkpoint package %d: %w",
				ErrTaprootAssetTransferInvalid, i, err)
		}
		decoded.CheckpointPackages[i] = pkg
	}
	arkPackage, err := readTaprootAssetPackage(reader)
	if err != nil {
		return fmt.Errorf("%w: Ark package: %w",
			ErrTaprootAssetTransferInvalid, err)
	}
	decoded.ArkPackage = arkPackage
	if reader.Len() != 0 {
		return fmt.Errorf("%w: trailing bytes",
			ErrTaprootAssetTransferInvalid)
	}
	if err := decoded.Validate(-1); err != nil {
		return err
	}

	*t = decoded

	return nil
}

func validateTaprootAssetPackage(label string, pkg []byte) error {
	if len(pkg) == 0 {
		return fmt.Errorf("%w: %s is empty",
			ErrTaprootAssetTransferInvalid, label)
	}
	if len(pkg) > MaxTaprootAssetPackageBytes {
		return fmt.Errorf("%w: %s size %d exceeds %d",
			ErrTaprootAssetTransferInvalid, label, len(pkg),
			MaxTaprootAssetPackageBytes)
	}

	return nil
}

func writeTaprootAssetPackage(w io.Writer, pkg []byte) error {
	if err := validateTaprootAssetPackage("package", pkg); err != nil {
		return err
	}
	if err := binary.Write(
		w, binary.BigEndian,
		uint32(
			len(pkg),
		),
	); err != nil {
		return err
	}
	_, err := w.Write(pkg)

	return err
}

func writeTaprootAssetCheckpointSlot(w io.Writer, pkg []byte) error {
	if len(pkg) > MaxTaprootAssetPackageBytes {
		return fmt.Errorf("%w: package size %d exceeds %d",
			ErrTaprootAssetTransferInvalid, len(pkg),
			MaxTaprootAssetPackageBytes)
	}
	if err := binary.Write(
		w, binary.BigEndian,
		uint32(
			len(pkg),
		),
	); err != nil {
		return err
	}
	if len(pkg) == 0 {
		return nil
	}

	_, err := w.Write(pkg)

	return err
}

func readTaprootAssetPackage(r *bytes.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	if length == 0 || length > MaxTaprootAssetPackageBytes {
		return nil, fmt.Errorf("invalid package size %d", length)
	}
	if uint64(length) > uint64(r.Len()) {
		return nil, io.ErrUnexpectedEOF
	}

	pkg := make([]byte, length)
	_, err := io.ReadFull(r, pkg)

	return pkg, err
}

func readTaprootAssetCheckpointSlot(r *bytes.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	if length > MaxTaprootAssetPackageBytes {
		return nil, fmt.Errorf("invalid package size %d", length)
	}
	if length == 0 {
		return nil, nil
	}
	if uint64(length) > uint64(r.Len()) {
		return nil, io.ErrUnexpectedEOF
	}

	pkg := make([]byte, length)
	_, err := io.ReadFull(r, pkg)

	return pkg, err
}
