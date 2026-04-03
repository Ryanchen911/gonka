package keeper

import (
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

// ProcessMaintenanceTransitions processes all maintenance lifecycle transitions
// scheduled for the exact current block height. Called from BeginBlock.
//
// Access pattern:
//  1. One prefix lookup for transition rows at the exact current block height
//  2. Iterate only the rows returned for that exact height
//  3. One direct reservation lookup per returned row
//  4. Apply transition (Scheduled->Active or Active->Completed)
//  5. Update the participant's MaintenanceState references
//  6. Delete consumed transition row
func (k Keeper) ProcessMaintenanceTransitions(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	blockHeight := sdkCtx.BlockHeight()

	mp := k.GetMaintenanceParams(ctx)
	if mp == nil || !mp.MaintenanceEnabled {
		return nil
	}

	// Collect transitions to process (we must not modify during iteration)
	type pendingTransition struct {
		reservationID  uint64
		transitionType uint32
	}
	var transitions []pendingTransition

	err := k.IterateMaintenanceTransitionsAtHeight(ctx, blockHeight, func(reservationID uint64, transitionType uint32) (bool, error) {
		transitions = append(transitions, pendingTransition{
			reservationID:  reservationID,
			transitionType: transitionType,
		})
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("failed to iterate maintenance transitions at height %d: %w", blockHeight, err)
	}

	for _, t := range transitions {
		var transitionErr error
		switch types.MaintenanceTransitionType(t.transitionType) {
		case types.MaintenanceTransitionType_MAINTENANCE_TRANSITION_TYPE_ACTIVATE:
			transitionErr = k.activateMaintenanceReservation(ctx, sdkCtx, t.reservationID, mp)
			if transitionErr != nil {
				k.LogError("Failed to activate maintenance reservation",
					types.Maintenance, "reservation_id", t.reservationID, "error", transitionErr)
			}
		case types.MaintenanceTransitionType_MAINTENANCE_TRANSITION_TYPE_COMPLETE:
			transitionErr = k.completeMaintenanceReservation(ctx, sdkCtx, t.reservationID)
			if transitionErr != nil {
				k.LogError("Failed to complete maintenance reservation",
					types.Maintenance, "reservation_id", t.reservationID, "error", transitionErr)
			}
		default:
			k.LogError("Unknown maintenance transition type",
				types.Maintenance, "reservation_id", t.reservationID, "type", t.transitionType)
			// Delete unknown transition types to avoid infinite retry
			transitionErr = nil
		}

		// Only delete consumed transition row after successful processing
		if transitionErr == nil {
			if err := k.DeleteMaintenanceTransition(ctx, blockHeight, t.reservationID); err != nil {
				k.LogError("Failed to delete maintenance transition",
					types.Maintenance, "reservation_id", t.reservationID, "error", err)
			}
		}
	}

	return nil
}

// activateMaintenanceReservation transitions a reservation from Scheduled to Active.
func (k Keeper) activateMaintenanceReservation(ctx context.Context, sdkCtx sdk.Context, reservationID uint64, mp *types.MaintenanceParams) error {
	r, found := k.GetMaintenanceReservation(ctx, reservationID)
	if !found {
		return fmt.Errorf("reservation %d not found", reservationID)
	}
	if r.Status != types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_SCHEDULED {
		return fmt.Errorf("reservation %d is not in scheduled state (status=%d)", reservationID, r.Status)
	}

	// Activation-time advisory re-check (Task 3.4 will add full logic)
	// For now, just emit a warning if caps would be exceeded
	warning := k.checkActivationTimeConcurrency(ctx, r, mp)
	if warning != "" {
		r.ActivationWarning = warning
		k.LogWarn("Maintenance reservation activated with concurrency advisory warning",
			types.Maintenance, "reservation_id", reservationID, "warning", warning)
	}

	// Transition to Active
	r.Status = types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_ACTIVE
	if err := k.SetMaintenanceReservation(ctx, r); err != nil {
		return err
	}

	// Update participant's MaintenanceState
	participantAddr, err := sdk.AccAddressFromBech32(r.Participant)
	if err != nil {
		return err
	}
	state := k.GetOrCreateMaintenanceState(ctx, participantAddr)
	state.ActiveReservationId = reservationID
	state.ScheduledReservationId = 0

	// Mark maintenance usage for the current epoch (suppresses credit accrual)
	epochIndex, found := k.GetEffectiveEpochIndex(ctx)
	if found {
		state.LastMaintenanceEpoch = epochIndex
	}

	if err := k.SetMaintenanceState(ctx, state); err != nil {
		return err
	}

	k.LogInfo("Maintenance window activated",
		types.Maintenance,
		"reservation_id", reservationID,
		"participant", r.Participant,
		"start_height", r.StartHeight,
		"duration_blocks", r.DurationBlocks,
	)

	sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
		"maintenance_activated",
		sdk.NewAttribute("reservation_id", fmt.Sprint(reservationID)),
		sdk.NewAttribute("participant", r.Participant),
		sdk.NewAttribute("start_height", fmt.Sprint(r.StartHeight)),
		sdk.NewAttribute("duration_blocks", fmt.Sprint(r.DurationBlocks)),
	))

	return nil
}

// completeMaintenanceReservation transitions a reservation from Active to Completed.
func (k Keeper) completeMaintenanceReservation(ctx context.Context, sdkCtx sdk.Context, reservationID uint64) error {
	r, found := k.GetMaintenanceReservation(ctx, reservationID)
	if !found {
		return fmt.Errorf("reservation %d not found", reservationID)
	}
	if r.Status != types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_ACTIVE {
		return fmt.Errorf("reservation %d is not in active state (status=%d)", reservationID, r.Status)
	}

	// Transition to Completed
	r.Status = types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_COMPLETED
	if err := k.SetMaintenanceReservation(ctx, r); err != nil {
		return err
	}

	// Clear participant's active reservation reference
	participantAddr, err := sdk.AccAddressFromBech32(r.Participant)
	if err != nil {
		return err
	}
	state := k.GetOrCreateMaintenanceState(ctx, participantAddr)
	state.ActiveReservationId = 0
	if err := k.SetMaintenanceState(ctx, state); err != nil {
		return err
	}

	k.LogInfo("Maintenance window completed",
		types.Maintenance,
		"reservation_id", reservationID,
		"participant", r.Participant,
	)

	sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
		"maintenance_completed",
		sdk.NewAttribute("reservation_id", fmt.Sprint(reservationID)),
		sdk.NewAttribute("participant", r.Participant),
	))

	return nil
}

// checkActivationTimeConcurrency re-checks concurrency caps at activation time.
// Returns a warning string if current caps would reject this reservation; empty string otherwise.
// The reservation still activates regardless — this is advisory only.
func (k Keeper) checkActivationTimeConcurrency(ctx context.Context, r types.MaintenanceReservation, mp *types.MaintenanceParams) string {
	endHeight := r.StartHeight + int64(r.DurationBlocks) - 1
	participantAddr, err := sdk.AccAddressFromBech32(r.Participant)
	if err != nil {
		return ""
	}

	// Count currently active/scheduled reservations that overlap with this window
	concurrentCount := uint32(0)
	var concurrentPower int64

	scanFrom := r.StartHeight - int64(mp.MaintenanceMaxWindowBlocks)
	if scanFrom < 0 {
		scanFrom = 0
	}
	scanTo := endHeight

	_ = k.IterateMaintenanceStartHeightRange(ctx, scanFrom, scanTo, func(reservationID uint64) (bool, error) {
		if reservationID == r.ReservationId {
			return false, nil // skip self
		}
		other, found := k.GetMaintenanceReservation(ctx, reservationID)
		if !found {
			return false, nil
		}
		if other.Status == types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_COMPLETED ||
			other.Status == types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_CANCELED {
			return false, nil
		}

		otherEnd := other.StartHeight + int64(other.DurationBlocks) - 1
		if other.StartHeight <= endHeight && otherEnd >= r.StartHeight {
			concurrentCount++
			otherAddr, err := sdk.AccAddressFromBech32(other.Participant)
			if err == nil {
				concurrentPower += k.getParticipantPower(ctx, otherAddr)
			}
		}
		return false, nil
	})

	var warnings []string

	// Check count cap (including this reservation)
	if concurrentCount+1 > mp.MaintenanceMaxConcurrentValidators {
		warnings = append(warnings, fmt.Sprintf(
			"concurrent count %d exceeds cap %d", concurrentCount+1, mp.MaintenanceMaxConcurrentValidators))
	}

	// Check power cap (including this participant)
	participantPower := k.getParticipantPower(ctx, participantAddr)
	totalPower := k.getTotalConsensusPower(ctx)
	if totalPower > 0 && mp.MaintenanceMaxConcurrentPowerBps > 0 {
		maxPower := totalPower * int64(mp.MaintenanceMaxConcurrentPowerBps) / 10000
		if concurrentPower+participantPower > maxPower {
			warnings = append(warnings, fmt.Sprintf(
				"concurrent power %d exceeds cap %d (%.1f%% of total %d)",
				concurrentPower+participantPower, maxPower,
				float64(mp.MaintenanceMaxConcurrentPowerBps)/100, totalPower))
		}
	}

	if len(warnings) == 0 {
		return ""
	}
	result := "activation-time concurrency advisory: "
	for i, w := range warnings {
		if i > 0 {
			result += "; "
		}
		result += w
	}
	return result
}
