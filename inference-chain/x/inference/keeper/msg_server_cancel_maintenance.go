package keeper

import (
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

func (k msgServer) CancelMaintenance(goCtx context.Context, msg *types.MsgCancelMaintenance) (*types.MsgCancelMaintenanceResponse, error) {
	if err := k.CheckPermission(goCtx, msg, AccountPermission); err != nil {
		return nil, err
	}

	sdkCtx := sdk.UnwrapSDKContext(goCtx)

	// Look up the reservation
	r, found := k.GetMaintenanceReservation(goCtx, msg.ReservationId)
	if !found {
		return nil, types.ErrMaintenanceReservationNotFound
	}

	// Only scheduled reservations can be canceled
	if r.Status != types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_SCHEDULED {
		return nil, types.ErrMaintenanceNotScheduled
	}

	// Verify caller is the participant or the original creator
	if msg.Creator != r.Participant && msg.Creator != r.CreatedBy {
		return nil, types.ErrInvalidPermission
	}

	// Transition to canceled
	r.Status = types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_CANCELED
	if err := k.SetMaintenanceReservation(goCtx, r); err != nil {
		return nil, err
	}

	// Restore credit to participant
	participantAddr, err := sdk.AccAddressFromBech32(r.Participant)
	if err != nil {
		return nil, err
	}
	state := k.GetOrCreateMaintenanceState(goCtx, participantAddr)
	state.CreditBlocks += r.DurationBlocks
	// Cap credit at max
	mp := k.GetMaintenanceParams(goCtx)
	if mp != nil && state.CreditBlocks > mp.MaintenanceCreditCapBlocks {
		state.CreditBlocks = mp.MaintenanceCreditCapBlocks
	}
	state.ScheduledReservationId = 0
	if err := k.SetMaintenanceState(goCtx, state); err != nil {
		return nil, err
	}

	// Remove transition schedule entries (endHeight must match scheduling: startHeight + duration - 1)
	endHeight := r.StartHeight + int64(r.DurationBlocks) - 1
	if err := k.DeleteMaintenanceTransition(goCtx, r.StartHeight, r.ReservationId); err != nil {
		return nil, fmt.Errorf("failed to delete activate transition: %w", err)
	}
	if err := k.DeleteMaintenanceTransition(goCtx, endHeight, r.ReservationId); err != nil {
		return nil, fmt.Errorf("failed to delete complete transition: %w", err)
	}

	// Remove start-height index
	if err := k.DeleteMaintenanceStartHeightIndex(goCtx, r.StartHeight, r.ReservationId); err != nil {
		return nil, fmt.Errorf("failed to delete start-height index: %w", err)
	}

	k.LogInfo("Maintenance window canceled",
		types.Maintenance,
		"reservation_id", r.ReservationId,
		"participant", r.Participant,
		"credit_restored", r.DurationBlocks,
	)

	sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
		"maintenance_canceled",
		sdk.NewAttribute("reservation_id", fmt.Sprint(r.ReservationId)),
		sdk.NewAttribute("participant", r.Participant),
		sdk.NewAttribute("credit_restored", fmt.Sprint(r.DurationBlocks)),
	))

	return &types.MsgCancelMaintenanceResponse{}, nil
}
