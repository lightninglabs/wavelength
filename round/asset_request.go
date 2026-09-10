package round

import (
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/types"
)

func validateAssetRequest(req *types.VTXORequest) error {
	if err := req.ValidateAssetFields(); err != nil {
		return err
	}
	if req.AssetRef == "" {
		return nil
	}

	assetRef, err := tapsdk.ParseAssetRef(req.AssetRef)
	if err != nil {
		return fmt.Errorf("asset reference: %w", err)
	}
	if assetRef.String() != req.AssetRef {
		return fmt.Errorf("asset reference must use canonical encoding")
	}

	return nil
}
