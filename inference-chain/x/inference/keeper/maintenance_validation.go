package keeper

import (
	"context"
	"fmt"

	cosmossdk_math "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/productscience/inference/x/inference/types"
)

// checkEpochPhaseOverlap verifies the proposed maintenance window does not overlap
// epoch-critical PoC commit/exchange or DKG (SetNewValidators) phases.
func (k Keeper) checkEpochPhaseOverlap(ctx context.Context, startHeight int64, durationBlocks uint64, mp *types.MaintenanceParams) error {
	endHeight := startHeight + int64(durationBlocks) - 1

	params, err := k.GetParams(ctx)
	if err != nil {
		return fmt.Errorf("failed to get params: %w", err)
	}
	ep := params.EpochParams
	if ep == nil {
		return nil // no epoch params, skip check
	}

	// We need to check against epochs that could overlap with [startHeight, endHeight].
	// Use the effective epoch as a reference, then check the current and next epoch.
	effectiveEpoch, found := k.GetEffectiveEpoch(ctx)
	if !found {
		return nil // no epoch yet, skip check
	}

	// Check against the effective epoch and the next few epochs that might overlap
	ec := types.NewEpochContext(*effectiveEpoch, *ep)
	epochsToCheck := []types.EpochContext{ec}

	// Add next epochs until we're past endHeight
	for i := 0; i < 5; i++ {
		next := epochsToCheck[len(epochsToCheck)-1].NextEpochContext()
		if next.StartOfPoC() > endHeight {
			break
		}
		epochsToCheck = append(epochsToCheck, next)
	}

	for _, epoch := range epochsToCheck {
		if epoch.EpochIndex == 0 {
			continue
		}

		// Check PoC generation + exchange phase overlap
		pocStart := epoch.StartOfPoC()
		pocExchangeEnd := epoch.PoCExchangeDeadline()
		if startHeight <= pocExchangeEnd && endHeight >= pocStart {
			return types.ErrMaintenanceOverlapsPoCPhase
		}

		// Check PoC validation phase overlap
		valStart := epoch.StartOfPoCValidation()
		valEnd := epoch.EndOfPoCValidation()
		if startHeight <= valEnd && endHeight >= valStart {
			return types.ErrMaintenanceOverlapsPoCPhase
		}

		// Check SetNewValidators (DKG) phase overlap
		setNewVal := epoch.SetNewValidators()
		if startHeight <= setNewVal && endHeight >= setNewVal {
			return types.ErrMaintenanceOverlapsDKGPhase
		}
	}

	return nil
}

// checkConcurrencyLimits verifies that adding a new reservation does not exceed
// concurrent participant count or power caps at scheduling time.
//
// All power math is done in math.Int (cosmossdk_math.Int) to avoid int64
// overflow on either token aggregation or the bps multiplication.
func (k Keeper) checkConcurrencyLimits(ctx context.Context, startHeight int64, durationBlocks uint64, participant sdk.AccAddress, mp *types.MaintenanceParams) error {
	endHeight := startHeight + int64(durationBlocks) - 1

	concurrentCount := uint32(0)
	concurrentPower := cosmossdk_math.ZeroInt()

	// Get this participant's power for the power cap check
	participantPower := k.getParticipantPower(ctx, participant)

	// Bounded overlap search: scan reservations whose start height falls in the
	// range that could overlap with our proposed window.
	// A reservation [s, s+d) overlaps [startHeight, endHeight] iff:
	//   s <= endHeight AND s+d-1 >= startHeight
	// Since d <= max_window_blocks, we know s >= startHeight - max_window_blocks
	scanFrom := startHeight - int64(mp.MaintenanceMaxWindowBlocks)
	if scanFrom < 0 {
		scanFrom = 0
	}
	scanTo := endHeight

	err := k.IterateMaintenanceStartHeightRange(ctx, scanFrom, scanTo, func(reservationID uint64) (bool, error) {
		r, found := k.GetMaintenanceReservation(ctx, reservationID)
		if !found {
			return false, nil
		}
		// Skip completed or canceled reservations
		if r.Status == types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_COMPLETED ||
			r.Status == types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_CANCELED {
			return false, nil
		}

		// Check actual overlap: reservation [r.StartHeight, r.StartHeight+r.DurationBlocks-1]
		rEnd := r.StartHeight + int64(r.DurationBlocks) - 1
		if r.StartHeight <= endHeight && rEnd >= startHeight {
			concurrentCount++
			rAddr, err := sdk.AccAddressFromBech32(r.Participant)
			if err == nil {
				concurrentPower = concurrentPower.Add(k.getParticipantPower(ctx, rAddr))
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("failed to check concurrency: %w", err)
	}

	// Check count cap (including the new reservation)
	if concurrentCount+1 > mp.MaintenanceMaxConcurrentValidators {
		return types.ErrMaintenanceConcurrentCountExceeded
	}

	// Check power cap (including this participant's power) using integer math.
	totalPower := k.getTotalConsensusPower(ctx)
	if totalPower.IsPositive() && mp.MaintenanceMaxConcurrentPowerBps > 0 {
		maxPower := totalPower.MulRaw(int64(mp.MaintenanceMaxConcurrentPowerBps)).QuoRaw(10000)
		if concurrentPower.Add(participantPower).GT(maxPower) {
			return types.ErrMaintenanceConcurrentPowerExceeded
		}
	}

	return nil
}

// validatorPower returns the consensus power of v as a math.Int. Using
// TokensFromConsensusPower's inverse here would lose precision; instead we
// just return v.Tokens, which is the effective bonded-token weight that
// underlies consensus power. The caller compares this against bps of the
// total bonded tokens, so the units are consistent.
func validatorPower(v stakingtypes.Validator) cosmossdk_math.Int {
	return v.Tokens
}

// checkParticipantOverlap checks that the proposed window does not overlap
// with any existing scheduled/active reservation for the same participant.
func (k Keeper) checkParticipantOverlap(ctx context.Context, participant sdk.AccAddress, startHeight int64, durationBlocks uint64, mp *types.MaintenanceParams) error {
	endHeight := startHeight + int64(durationBlocks) - 1

	scanFrom := startHeight - int64(mp.MaintenanceMaxWindowBlocks)
	if scanFrom < 0 {
		scanFrom = 0
	}
	scanTo := endHeight

	var overlapFound bool
	err := k.IterateMaintenanceStartHeightRange(ctx, scanFrom, scanTo, func(reservationID uint64) (bool, error) {
		r, found := k.GetMaintenanceReservation(ctx, reservationID)
		if !found {
			return false, nil
		}
		if r.Status == types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_COMPLETED ||
			r.Status == types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_CANCELED {
			return false, nil
		}
		if r.Participant != participant.String() {
			return false, nil
		}

		rEnd := r.StartHeight + int64(r.DurationBlocks) - 1
		if r.StartHeight <= endHeight && rEnd >= startHeight {
			overlapFound = true
			return true, nil // stop iteration
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("failed to check participant overlap: %w", err)
	}

	if overlapFound {
		return types.ErrMaintenanceOverlap
	}
	return nil
}

// getParticipantPower returns the consensus power (in bonded-token units, as
// math.Int) for a participant. Returns ZeroInt if the participant is not a
// validator. This is an O(1) lookup via the staking keeper — no full
// validator-set scan.
func (k Keeper) getParticipantPower(ctx context.Context, participant sdk.AccAddress) cosmossdk_math.Int {
	// In Gonka, the participant account address bytes match the validator
	// operator address bytes, so we can convert directly.
	v, err := k.Staking.GetValidator(ctx, sdk.ValAddress(participant))
	if err != nil {
		return cosmossdk_math.ZeroInt()
	}
	return validatorPower(v)
}

// getTotalConsensusPower returns the total bonded-token power across all
// validators as a math.Int. Uses GetLastTotalPower (which returns the staking
// module's cached LastTotalPower in *consensus power* units, then re-scales
// back to bonded-token units to keep units consistent with getParticipantPower).
//
// We multiply by DefaultPowerReduction to convert "consensus power units" back
// to "bonded-token units" so that the bps comparison in checkConcurrencyLimits
// is unit-consistent: both sides are bonded-token amounts.
func (k Keeper) getTotalConsensusPower(ctx context.Context) cosmossdk_math.Int {
	totalConsPower, err := k.Staking.GetLastTotalPower(ctx)
	if err != nil {
		return cosmossdk_math.ZeroInt()
	}
	// LastTotalPower is stored in consensus-power units; reverse the
	// PowerReduction to get bonded-token units.
	return totalConsPower.Mul(sdk.DefaultPowerReduction)
}
