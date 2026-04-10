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
//
// This is on the slashing hot path: it is invoked for every validator that
// missed a block, on every block. Implementation must be O(1) — never iterate
// the validator set here.
func (a *MaintenanceSlashingAdapter) IsValidatorInActiveMaintenance(ctx context.Context, consAddr sdk.ConsAddress) bool {
	// O(1) lookup of the validator by consensus address.
	v, err := a.inferenceKeeper.Staking.GetValidatorByConsAddr(ctx, consAddr)
	if err != nil {
		return false
	}
	valAddr, err := sdk.ValAddressFromBech32(v.GetOperator())
	if err != nil {
		return false
	}
	// In Gonka, AccAddress and ValAddress share the same underlying bytes.
	return a.inferenceKeeper.IsParticipantInActiveMaintenance(ctx, sdk.AccAddress(valAddr))
}
