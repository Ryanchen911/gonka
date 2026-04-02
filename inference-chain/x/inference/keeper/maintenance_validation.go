package keeper

import (
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
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
func (k Keeper) checkConcurrencyLimits(ctx context.Context, startHeight int64, durationBlocks uint64, participant sdk.AccAddress, mp *types.MaintenanceParams) error {
	endHeight := startHeight + int64(durationBlocks) - 1

	// Count concurrent reservations that overlap with [startHeight, endHeight]
	concurrentCount := uint32(0)
	var concurrentPower int64

	// Get this participant's power for the power cap check
	participantPower := k.getParticipantPower(ctx, participant)

	// Bounded overlap search: scan reservations whose start height falls in the
	// range that could overlap with our proposed window.
	// A reservation [s, s+d) overlaps [startHeight, endHeight] iff:
	//   s <= endHeight AND s+d-1 >= startHeight
	// Since d <= max_window_blocks, we know s >= startHeight - max_window_blocks
	scanFrom := startHeight - int64(mp.MaintenanceMaxWindowBlocks)
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
				concurrentPower += k.getParticipantPower(ctx, rAddr)
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

	// Check power cap (including this participant's power)
	totalPower := k.getTotalConsensusPower(ctx)
	if totalPower > 0 && mp.MaintenanceMaxConcurrentPowerBps > 0 {
		maxPower := totalPower * int64(mp.MaintenanceMaxConcurrentPowerBps) / 10000
		if concurrentPower+participantPower > maxPower {
			return types.ErrMaintenanceConcurrentPowerExceeded
		}
	}

	return nil
}

// checkParticipantOverlap checks that the proposed window does not overlap
// with any existing scheduled/active reservation for the same participant.
func (k Keeper) checkParticipantOverlap(ctx context.Context, participant sdk.AccAddress, startHeight int64, durationBlocks uint64, mp *types.MaintenanceParams) error {
	endHeight := startHeight + int64(durationBlocks) - 1

	scanFrom := startHeight - int64(mp.MaintenanceMaxWindowBlocks)
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

// getParticipantPower returns the consensus power for a participant.
// Returns 0 if the participant is not a validator or power cannot be determined.
func (k Keeper) getParticipantPower(ctx context.Context, participant sdk.AccAddress) int64 {
	validators, err := k.Staking.GetAllValidators(ctx)
	if err != nil {
		return 0
	}
	participantStr := participant.String()
	for _, v := range validators {
		// Convert operator address to account address for comparison
		accAddr, err := sdk.AccAddressFromBech32(v.OperatorAddress)
		if err != nil {
			// Try direct comparison as bech32 strings may use different prefixes
			valAddr, err2 := sdk.ValAddressFromBech32(v.OperatorAddress)
			if err2 != nil {
				continue
			}
			accAddr = sdk.AccAddress(valAddr)
		}
		if accAddr.String() == participantStr {
			return v.Tokens.Int64()
		}
	}
	return 0
}

// getTotalConsensusPower returns the total consensus power across all validators.
func (k Keeper) getTotalConsensusPower(ctx context.Context) int64 {
	validators, err := k.Staking.GetAllValidators(ctx)
	if err != nil {
		return 0
	}
	var total int64
	for _, v := range validators {
		total += v.Tokens.Int64()
	}
	return total
}
