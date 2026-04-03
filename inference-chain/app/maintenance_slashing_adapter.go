package app

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/keeper"
)

// MaintenanceSlashingAdapter adapts the inference keeper's maintenance check
// (which uses AccAddress) to the slashing keeper's MaintenanceChecker interface
// (which uses ConsAddress). This bridge is necessary because the slashing module
// operates on consensus addresses while maintenance state is keyed by participant
// (account) addresses.
type MaintenanceSlashingAdapter struct {
	inferenceKeeper *keeper.Keeper
}

// NewMaintenanceSlashingAdapter creates a new adapter.
func NewMaintenanceSlashingAdapter(k *keeper.Keeper) *MaintenanceSlashingAdapter {
	return &MaintenanceSlashingAdapter{inferenceKeeper: k}
}

// IsValidatorInActiveMaintenance checks if the validator identified by its
// consensus address is currently in an active maintenance window.
// It converts ConsAddress -> ValAddress -> AccAddress to look up maintenance state.
func (a *MaintenanceSlashingAdapter) IsValidatorInActiveMaintenance(ctx context.Context, consAddr sdk.ConsAddress) bool {
	// In Gonka, the validator operator address and participant address share the
	// same underlying bytes (AccAddress == ValAddress byte-wise), so we can
	// convert ConsAddress to AccAddress via the staking module's validator lookup.
	validators, err := a.inferenceKeeper.Staking.GetAllValidators(ctx)
	if err != nil {
		return false
	}

	for _, v := range validators {
		pk, err := v.ConsPubKey()
		if err != nil {
			continue
		}
		if sdk.ConsAddress(pk.Address()).Equals(consAddr) {
			// Found the validator — decode the bech32 operator address to AccAddress.
			// v.GetOperator() returns a bech32 string; we must decode it properly.
			valAddr, err := sdk.ValAddressFromBech32(v.GetOperator())
			if err != nil {
				return false
			}
			accAddr := sdk.AccAddress(valAddr)
			return a.inferenceKeeper.IsParticipantInActiveMaintenance(ctx, accAddr)
		}
	}

	return false
}
