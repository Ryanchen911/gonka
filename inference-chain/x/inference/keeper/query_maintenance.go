package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (k Keeper) MaintenanceCredit(ctx context.Context, req *types.QueryMaintenanceCreditRequest) (*types.QueryMaintenanceCreditResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	// TODO: implement in Task 5.1
	return &types.QueryMaintenanceCreditResponse{Found: false}, nil
}

func (k Keeper) MaintenanceScheduled(ctx context.Context, req *types.QueryMaintenanceScheduledRequest) (*types.QueryMaintenanceScheduledResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	// TODO: implement in Task 5.1
	return &types.QueryMaintenanceScheduledResponse{Found: false}, nil
}

func (k Keeper) MaintenanceActive(ctx context.Context, req *types.QueryMaintenanceActiveRequest) (*types.QueryMaintenanceActiveResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	// TODO: implement in Task 5.1
	return &types.QueryMaintenanceActiveResponse{}, nil
}

func (k Keeper) MaintenanceStatus(ctx context.Context, req *types.QueryMaintenanceStatusRequest) (*types.QueryMaintenanceStatusResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	// TODO: implement in Task 5.1
	return &types.QueryMaintenanceStatusResponse{Found: false}, nil
}

func (k Keeper) MaintenanceConcurrency(ctx context.Context, req *types.QueryMaintenanceConcurrencyRequest) (*types.QueryMaintenanceConcurrencyResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	// TODO: implement in Task 5.1
	return &types.QueryMaintenanceConcurrencyResponse{}, nil
}

func (k Keeper) MaintenanceSchedulability(ctx context.Context, req *types.QueryMaintenanceSchedulabilityRequest) (*types.QueryMaintenanceSchedulabilityResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}

	sdkCtx := sdk.UnwrapSDKContext(ctx)
	blockHeight := sdkCtx.BlockHeight()

	reject := func(reason string) (*types.QueryMaintenanceSchedulabilityResponse, error) {
		return &types.QueryMaintenanceSchedulabilityResponse{Schedulable: false, RejectionReason: reason}, nil
	}

	mp := k.GetMaintenanceParams(ctx)
	if mp == nil || !mp.MaintenanceEnabled {
		return reject("maintenance windows are disabled")
	}

	participantAddr, err := sdk.AccAddressFromBech32(req.Participant)
	if err != nil {
		return reject("invalid participant address")
	}

	_, err = k.Participants.Get(ctx, participantAddr)
	if err != nil {
		return reject("participant not found")
	}

	if req.DurationBlocks == 0 {
		return reject("duration_blocks must be positive")
	}
	if req.DurationBlocks > mp.MaintenanceMaxWindowBlocks {
		return reject("duration exceeds maximum maintenance window blocks")
	}

	if req.StartHeight <= blockHeight+int64(mp.MaintenanceMinScheduleLeadBlocks) {
		return reject("start height does not satisfy minimum scheduling lead time")
	}

	state := k.GetOrCreateMaintenanceState(ctx, participantAddr)
	if state.ScheduledReservationId != 0 {
		return reject("participant already has a scheduled maintenance window")
	}

	if state.CreditBlocks < req.DurationBlocks {
		return reject("insufficient maintenance credit")
	}

	if err := k.checkEpochPhaseOverlap(ctx, req.StartHeight, req.DurationBlocks, mp); err != nil {
		return reject(err.Error())
	}

	if err := k.checkConcurrencyLimits(ctx, req.StartHeight, req.DurationBlocks, participantAddr, mp); err != nil {
		return reject(err.Error())
	}

	if err := k.checkParticipantOverlap(ctx, participantAddr, req.StartHeight, req.DurationBlocks, mp); err != nil {
		return reject(err.Error())
	}

	return &types.QueryMaintenanceSchedulabilityResponse{Schedulable: true}, nil
}
