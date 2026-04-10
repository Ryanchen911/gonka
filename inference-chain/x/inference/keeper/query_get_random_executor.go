package keeper

import (
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/group"
	"github.com/productscience/inference/x/inference/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (k Keeper) GetRandomExecutor(goCtx context.Context, req *types.QueryGetRandomExecutorRequest) (*types.QueryGetRandomExecutorResponse, error) {
	if req == nil {
		k.LogError("GetRandomExecutor: received nil request", types.EpochGroup)
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}

	k.LogInfo("GetRandomExecutor: Starting executor selection", types.EpochGroup,
		"model_id", req.Model)

	filterFn, err := k.createFilterFn(goCtx, req.Model)
	if err != nil {
		k.LogError("GetRandomExecutor: failed to create filter function", types.EpochGroup,
			"model_id", req.Model, "error", err.Error())
		return nil, err
	}

	// Wrap filter to exclude participants in active maintenance windows.
	// Maintenance-covered participants remain in epoch groups but must not
	// receive new inference assignments during their maintenance window.
	originalFilter := filterFn
	filterFn = func(members []*group.GroupMember) []*group.GroupMember {
		filtered := originalFilter(members)
		return k.filterOutMaintenanceParticipants(goCtx, filtered)
	}

	epochGroup, err := k.GetCurrentEpochGroup(goCtx)
	if err != nil {
		k.LogError("GetRandomExecutor: failed to get current epoch group", types.EpochGroup,
			"model_id", req.Model, "error", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	k.LogInfo("GetRandomExecutor: Retrieved epoch group", types.EpochGroup,
		"model_id", req.Model, "epoch_id", epochGroup.GroupData.EpochIndex)

	modelFound := false
	for _, m := range epochGroup.GroupData.GetSubGroupModels() {
		if m == req.Model {
			modelFound = true
			break
		}
	}
	if !modelFound {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("model %s not registered", req.Model))
	}

	participant, err := epochGroup.GetRandomMemberForModel(goCtx, req.Model, filterFn)
	if err != nil {
		k.LogError("GetRandomExecutor: failed to get random member", types.EpochGroup,
			"model_id", req.Model, "error", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	k.LogInfo("GetRandomExecutor: Selected participant", types.EpochGroup,
		"model_id", req.Model, "participant_address", participant.Address)

	return &types.QueryGetRandomExecutorResponse{
		Executor: *participant,
	}, nil
}

func (k Keeper) createFilterFn(goCtx context.Context, modelId string) (func(members []*group.GroupMember) []*group.GroupMember, error) {
	sdkCtx := sdk.UnwrapSDKContext(goCtx)

	k.LogInfo("GetRandomExecutor: createFilterFn: Starting filter creation", types.EpochGroup,
		"model_id", modelId, "block_height", sdkCtx.BlockHeight())

	effectiveEpoch, found := k.GetEffectiveEpoch(goCtx)
	if !found || effectiveEpoch == nil {
		k.LogError("GetRandomExecutor: createFilterFn: no effective epoch found", types.EpochGroup,
			"model_id", modelId)
		return nil, status.Error(codes.Unavailable, "GetRandomExecutor: no effective epoch found")
	}

	epochParams, err := k.GetParams(goCtx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if epochParams.EpochParams == nil {
		k.LogError("GetRandomExecutor: createFilterFn: epoch params are nil", types.EpochGroup,
			"model_id", modelId, "epoch_index", effectiveEpoch.Index)
		return nil, status.Error(codes.Unavailable, "GetRandomExecutor: epoch params are nil")
	}

	epochContext, err := types.NewEpochContextFromEffectiveEpoch(*effectiveEpoch, *epochParams.EpochParams, sdkCtx.BlockHeight())
	if err != nil {
		k.LogError("GetRandomExecutor: createFilterFn: failed to create epoch context", types.EpochGroup,
			"model_id", modelId, "epoch_index", effectiveEpoch.Index, "error", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}
	currentPhase := epochContext.GetCurrentPhase(sdkCtx.BlockHeight())

	k.LogInfo("GetRandomExecutor: createFilterFn: Determined current phase", types.EpochGroup,
		"model_id", modelId, "current_phase", string(currentPhase),
		"epoch_index", effectiveEpoch.Index, "latest_epoch_index", epochContext.EpochIndex,
		"block_height", sdkCtx.BlockHeight(), "set_new_validators_block_height", epochContext.SetNewValidators())

	_, isActive, err := k.GetActiveConfirmationPoCEvent(goCtx)
	if err != nil {
		k.LogError("GetRandomExecutor: createFilterFn: failed to check confirmation PoC", types.EpochGroup,
			"model_id", modelId, "error", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	if isActive {
		return k.createIsAvailableDuringPoCFilterFn(goCtx, effectiveEpoch.Index, modelId)
	}

	if currentPhase == types.InferencePhase && sdkCtx.BlockHeight() > epochContext.SetNewValidators() {
		return func(members []*group.GroupMember) []*group.GroupMember {
			return members
		}, nil
	}

	return k.createIsAvailableDuringPoCFilterFn(goCtx, effectiveEpoch.Index, modelId)
}

func (k Keeper) createIsAvailableDuringPoCFilterFn(ctx context.Context, epochId uint64, modelId string) (func(members []*group.GroupMember) []*group.GroupMember, error) {
	k.LogInfo("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Starting PoC availability filter creation", types.EpochGroup,
		"epoch_id", epochId, "model_id", modelId)

	activeParticipants, found := k.GetActiveParticipants(ctx, epochId)
	if !found {
		msg := fmt.Sprintf("GetRandomExecutor: createIsAvailableDuringPocFilterFn failed, can't find active participants. epochId = %d", epochId)
		k.LogError("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: active participants not found", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId)
		return nil, status.Error(codes.Unavailable, msg)
	}

	if activeParticipants.Participants == nil {
		k.LogError("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: participants list is nil", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId)
		return nil, status.Error(codes.Internal, "participants list is nil")
	}

	k.LogInfo("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Found active participants", types.EpochGroup,
		"epoch_id", epochId, "model_id", modelId, "participant_count", len(activeParticipants.Participants))

	isAvailableDuringPoc := make(map[string]bool)
	totalParticipantsChecked := 0
	participantsWithModel := 0
	participantsWithAvailableNodes := 0

	for _, participant := range activeParticipants.Participants {
		totalParticipantsChecked++

		if participant == nil {
			k.LogWarn("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: found nil participant", types.EpochGroup,
				"epoch_id", epochId, "model_id", modelId, "participant_index", totalParticipantsChecked-1)
			continue
		}

		k.LogDebug("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Processing participant", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
			"participant_models", participant.Models, "ml_nodes_arrays", len(participant.MlNodes))

		// Find the model index
		var participantModelIndex = -1
		for i, model := range participant.Models {
			if model == modelId {
				participantModelIndex = i
				break
			}
		}

		if participantModelIndex == -1 {
			k.LogDebug("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: participant doesn't support model", types.EpochGroup,
				"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
				"participant_models", participant.Models)
			continue
		}

		participantsWithModel++
		k.LogDebug("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: participant supports model", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
			"model_index", participantModelIndex)

		// Defensive programming: check bounds
		if len(participant.MlNodes) <= participantModelIndex {
			k.LogWarn("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: model index out of bounds", types.EpochGroup,
				"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
				"model_index", participantModelIndex, "ml_nodes_length", len(participant.MlNodes))
			continue
		}

		// Defensive programming: check for nil model MLNodes array
		modelMLNodes := participant.MlNodes[participantModelIndex]
		if modelMLNodes == nil {
			k.LogWarn("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: model MLNodes array is nil", types.EpochGroup,
				"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
				"model_index", participantModelIndex)
			continue
		}

		if modelMLNodes.MlNodes == nil {
			k.LogWarn("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: MlNodes slice is nil", types.EpochGroup,
				"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
				"model_index", participantModelIndex)
			continue
		}

		k.LogDebug("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Checking MLNodes for POC_SLOT availability", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
			"ml_nodes_count", len(modelMLNodes.MlNodes))

		nodeCount := 0
		availableNodeCount := 0
		for _, node := range modelMLNodes.MlNodes {
			nodeCount++

			if node == nil {
				k.LogWarn("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: found nil MLNode", types.EpochGroup,
					"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
					"node_index", nodeCount-1)
				continue
			}

			k.LogDebug("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Checking node timeslot allocation", types.EpochGroup,
				"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
				"node_id", node.NodeId, "timeslot_allocation", node.TimeslotAllocation,
				"timeslot_length", len(node.TimeslotAllocation))

			// Defensive programming: check timeslot allocation bounds and values
			if len(node.TimeslotAllocation) <= 1 {
				k.LogWarn("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: invalid timeslot allocation length", types.EpochGroup,
					"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
					"node_id", node.NodeId, "timeslot_allocation", node.TimeslotAllocation,
					"expected_min_length", 2)
				continue
			}

			// Check POC_SLOT availability (index 1)
			if node.TimeslotAllocation[1] {
				availableNodeCount++
				isAvailableDuringPoc[participant.Index] = true
				k.LogInfo("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Found node available during PoC", types.EpochGroup,
					"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
					"node_id", node.NodeId, "timeslot_allocation", node.TimeslotAllocation)
				// Break after finding first available node for this participant
				break
			}
		}

		if availableNodeCount > 0 {
			participantsWithAvailableNodes++
		}

		k.LogDebug("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Participant node analysis complete", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId, "participant_address", participant.Index,
			"total_nodes", nodeCount, "available_nodes", availableNodeCount,
			"participant_available", isAvailableDuringPoc[participant.Index])
	}

	k.LogInfo("GetRandomExecutor: createIsAvailableDuringPoCFilterFn: Analysis complete", types.EpochGroup,
		"epoch_id", epochId, "model_id", modelId,
		"total_participants_checked", totalParticipantsChecked,
		"participants_with_model", participantsWithModel,
		"participants_with_available_nodes", participantsWithAvailableNodes,
		"available_participants", len(isAvailableDuringPoc))

	return func(members []*group.GroupMember) []*group.GroupMember {
		k.LogDebug("GetRandomExecutor: PoC filter function: Starting member filtering", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId, "input_member_count", len(members))

		filtered := make([]*group.GroupMember, 0, len(members))
		for _, member := range members {
			if member == nil {
				k.LogWarn("GetRandomExecutor: PoC filter function: found nil group member", types.EpochGroup,
					"epoch_id", epochId, "model_id", modelId)
				continue
			}

			if member.Member == nil {
				k.LogWarn("GetRandomExecutor: PoC filter function: group member has nil Member field", types.EpochGroup,
					"epoch_id", epochId, "model_id", modelId)
				continue
			}

			if isAvailable, exists := isAvailableDuringPoc[member.Member.Address]; exists && isAvailable {
				filtered = append(filtered, member)
				k.LogDebug("GetRandomExecutor: PoC filter function: included member", types.EpochGroup,
					"epoch_id", epochId, "model_id", modelId,
					"member_address", member.Member.Address)
			} else {
				k.LogDebug("GetRandomExecutor: PoC filter function: excluded member", types.EpochGroup,
					"epoch_id", epochId, "model_id", modelId,
					"member_address", member.Member.Address, "exists", exists, "available", isAvailable)
			}
		}

		k.LogInfo("GetRandomExecutor: PoC filter function: Filtering complete", types.EpochGroup,
			"epoch_id", epochId, "model_id", modelId,
			"input_member_count", len(members), "filtered_member_count", len(filtered))

		return filtered
	}, nil
}
