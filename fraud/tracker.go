package fraud

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/vtxo"
)

// TrackVTXOs asks tracker to arm passive fraud watches for live OOR VTXOs.
func TrackVTXOs(ctx context.Context, tracker actor.TellOnlyRef[Msg],
	descs []*vtxo.Descriptor) error {

	if tracker == nil {
		return fmt.Errorf("%w: fraud tracker is nil",
			ErrWatchUnavailable)
	}

	err := tracker.Tell(ctx, &TrackVTXOsRequest{VTXOs: descs})
	if err != nil {
		return fmt.Errorf("track fraud vtxos: %w", err)
	}

	return nil
}

// shouldTrackDescriptor reports whether desc needs recipient fraud defense.
func shouldTrackDescriptor(desc *vtxo.Descriptor) bool {
	if desc == nil {
		return false
	}
	if desc.Status != vtxo.VTXOStatusLive {
		return false
	}

	return desc.ChainDepth > 0
}

// joinTrackError appends err to existing without dropping later descriptors.
func joinTrackError(existing, err error) error {
	if err == nil {
		return existing
	}

	return errors.Join(existing, err)
}
