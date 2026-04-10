package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/group"
	"github.com/productscience/inference/x/inference/types"
)

// --- Reservation CRUD ---

// NextMaintenanceReservationID returns the next reservation ID and increments the counter.
func (k Keeper) NextMaintenanceReservationID(ctx context.Context) (uint64, error) {
	counter, err := k.MaintenanceReservationCounter.Get(ctx)
	if err != nil {
		counter = 0
	}
	nextID := counter + 1
	if err := k.MaintenanceReservationCounter.Set(ctx, nextID); err != nil {
		return 0, err
	}
	return nextID, nil
}

// SetMaintenanceReservation stores a reservation by its ID.
func (k Keeper) SetMaintenanceReservation(ctx context.Context, r types.MaintenanceReservation) error {
	return k.MaintenanceReservations.Set(ctx, r.ReservationId, r)
}

// GetMaintenanceReservation retrieves a reservation by ID.
func (k Keeper) GetMaintenanceReservation(ctx context.Context, id uint64) (types.MaintenanceReservation, bool) {
	v, err := k.MaintenanceReservations.Get(ctx, id)
	if err != nil {
		return types.MaintenanceReservation{}, false
	}
	return v, true
}

// --- MaintenanceState CRUD ---

// SetMaintenanceState stores the per-participant maintenance state.
func (k Keeper) SetMaintenanceState(ctx context.Context, state types.MaintenanceState) error {
	addr, err := sdk.AccAddressFromBech32(state.Participant)
	if err != nil {
		return err
	}
	return k.MaintenanceStates.Set(ctx, addr, state)
}

// GetMaintenanceState retrieves per-participant maintenance state.
func (k Keeper) GetMaintenanceState(ctx context.Context, participant sdk.AccAddress) (types.MaintenanceState, bool) {
	v, err := k.MaintenanceStates.Get(ctx, participant)
	if err != nil {
		return types.MaintenanceState{}, false
	}
	return v, true
}

// GetOrCreateMaintenanceState retrieves or initializes maintenance state for a participant.
func (k Keeper) GetOrCreateMaintenanceState(ctx context.Context, participant sdk.AccAddress) types.MaintenanceState {
	state, found := k.GetMaintenanceState(ctx, participant)
	if !found {
		return types.MaintenanceState{
			Participant: participant.String(),
		}
	}
	return state
}

// --- Transition Schedule ---

// SetMaintenanceTransition stores a transition entry for exact block-height lookup in BeginBlock.
// transitionType: 1 = activate, 2 = complete (maps to MaintenanceTransitionType enum values).
func (k Keeper) SetMaintenanceTransition(ctx context.Context, blockHeight int64, reservationID uint64, transitionType uint32) error {
	return k.MaintenanceTransitions.Set(ctx, collections.Join(blockHeight, reservationID), transitionType)
}

// DeleteMaintenanceTransition removes a consumed transition entry.
func (k Keeper) DeleteMaintenanceTransition(ctx context.Context, blockHeight int64, reservationID uint64) error {
	return k.MaintenanceTransitions.Remove(ctx, collections.Join(blockHeight, reservationID))
}

// IterateMaintenanceTransitionsAtHeight iterates over all transitions scheduled for the exact given height.
func (k Keeper) IterateMaintenanceTransitionsAtHeight(ctx context.Context, blockHeight int64, fn func(reservationID uint64, transitionType uint32) (stop bool, err error)) error {
	rng := collections.NewPrefixedPairRange[int64, uint64](blockHeight)
	iter, err := k.MaintenanceTransitions.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		kv, err := iter.KeyValue()
		if err != nil {
			return err
		}
		reservationID := kv.Key.K2()
		transitionType := kv.Value
		stop, err := fn(reservationID, transitionType)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// --- Start Height Index (for scheduling overlap checks) ---

// SetMaintenanceStartHeightIndex adds an index entry for a reservation by its start height.
func (k Keeper) SetMaintenanceStartHeightIndex(ctx context.Context, startHeight int64, reservationID uint64) error {
	return k.MaintenanceStartHeightIndex.Set(ctx, collections.Join(startHeight, reservationID), reservationID)
}

// DeleteMaintenanceStartHeightIndex removes a start-height index entry.
func (k Keeper) DeleteMaintenanceStartHeightIndex(ctx context.Context, startHeight int64, reservationID uint64) error {
	return k.MaintenanceStartHeightIndex.Remove(ctx, collections.Join(startHeight, reservationID))
}

// IterateMaintenanceStartHeightRange iterates reservations whose start height falls in [fromHeight, toHeight].
// Used for bounded overlap checks during scheduling.
func (k Keeper) IterateMaintenanceStartHeightRange(ctx context.Context, fromHeight, toHeight int64, fn func(reservationID uint64) (stop bool, err error)) error {
	for h := fromHeight; h <= toHeight; h++ {
		err := k.iterateStartHeightPrefix(ctx, h, fn)
		if err != nil {
			return err
		}
	}
	return nil
}

func (k Keeper) iterateStartHeightPrefix(ctx context.Context, height int64, fn func(reservationID uint64) (stop bool, err error)) error {
	rng := collections.NewPrefixedPairRange[int64, uint64](height)
	iter, err := k.MaintenanceStartHeightIndex.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		kv, err := iter.KeyValue()
		if err != nil {
			return err
		}
		reservationID := kv.Value
		stop, err := fn(reservationID)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// --- Credit accrual ---

// GrantMaintenanceCredit grants maintenance credit to a participant after a
// successful reward claim. Credit is not granted if maintenance was activated
// for that participant in the claimed epoch.
//
// Lives on Keeper (not msgServer) so other modules and BeginBlock/EndBlock
// hooks can reuse it. Returns the bech32 decode error to the caller instead
// of swallowing it — the caller is responsible for logging.
func (k Keeper) GrantMaintenanceCredit(ctx context.Context, participant string, epochIndex uint64) error {
	mp := k.GetMaintenanceParams(ctx)
	if mp == nil || !mp.MaintenanceEnabled || mp.MaintenanceCreditEarnPerSuccessfulEpochBlocks == 0 {
		return nil
	}

	participantAddr, err := sdk.AccAddressFromBech32(participant)
	if err != nil {
		return fmt.Errorf("invalid participant address %q: %w", participant, err)
	}

	state := k.GetOrCreateMaintenanceState(ctx, participantAddr)

	// Do not grant credit if maintenance was activated in this epoch
	if state.LastMaintenanceEpoch == epochIndex && epochIndex != 0 {
		k.LogDebug("Skipping maintenance credit: maintenance was used in this epoch",
			types.Maintenance, "participant", participant, "epoch", epochIndex)
		return nil
	}

	state.CreditBlocks += mp.MaintenanceCreditEarnPerSuccessfulEpochBlocks
	if state.CreditBlocks > mp.MaintenanceCreditCapBlocks {
		state.CreditBlocks = mp.MaintenanceCreditCapBlocks
	}

	if err := k.SetMaintenanceState(ctx, state); err != nil {
		return fmt.Errorf("failed to grant maintenance credit: %w", err)
	}
	return nil
}

// --- Convenience: check if a participant is in active maintenance ---

// IsParticipantInActiveMaintenance returns true if the participant has an active maintenance window.
func (k Keeper) IsParticipantInActiveMaintenance(ctx context.Context, participant sdk.AccAddress) bool {
	state, found := k.GetMaintenanceState(ctx, participant)
	if !found {
		return false
	}
	if state.ActiveReservationId == 0 {
		return false
	}
	r, found := k.GetMaintenanceReservation(ctx, state.ActiveReservationId)
	if !found {
		return false
	}
	return r.Status == types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_ACTIVE
}

// IsParticipantAddressInActiveMaintenance is a convenience wrapper that accepts
// a bech32 address string instead of sdk.AccAddress.
func (k Keeper) IsParticipantAddressInActiveMaintenance(ctx context.Context, address string) bool {
	addr, err := sdk.AccAddressFromBech32(address)
	if err != nil {
		return false
	}
	return k.IsParticipantInActiveMaintenance(ctx, addr)
}

// filterOutMaintenanceParticipants removes group members that are currently in
// an active maintenance window. Used by GetRandomExecutor to prevent assigning
// new inference work to maintenance-covered participants.
//
// Implementation: build a single set of currently-active maintenance addresses
// (bounded by MaintenanceMaxConcurrentValidators) up-front, then O(1) lookup
// per member. This avoids one collection read per member.
func (k Keeper) filterOutMaintenanceParticipants(ctx context.Context, members []*group.GroupMember) []*group.GroupMember {
	mp := k.GetMaintenanceParams(ctx)
	if mp == nil || !mp.MaintenanceEnabled {
		return members
	}

	activeAddrs := k.collectActiveMaintenanceAddresses(ctx)
	if len(activeAddrs) == 0 {
		return members
	}

	filtered := make([]*group.GroupMember, 0, len(members))
	for _, member := range members {
		if member == nil || member.Member == nil {
			continue
		}
		if _, inMaintenance := activeAddrs[member.Member.Address]; inMaintenance {
			k.LogDebug("Excluding maintenance-covered participant from assignment",
				types.Maintenance, "participant", member.Member.Address)
			continue
		}
		filtered = append(filtered, member)
	}
	return filtered
}

// collectActiveMaintenanceAddresses returns the bech32 addresses of every
// participant currently in an ACTIVE maintenance window. The result size is
// bounded by MaintenanceMaxConcurrentValidators.
func (k Keeper) collectActiveMaintenanceAddresses(ctx context.Context) map[string]struct{} {
	addrs := make(map[string]struct{})
	iter, err := k.MaintenanceActiveIndex.Iterate(ctx, nil)
	if err != nil {
		return addrs
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		reservationID, err := iter.Key()
		if err != nil {
			continue
		}
		r, found := k.GetMaintenanceReservation(ctx, reservationID)
		if !found {
			continue
		}
		if r.Status != types.MaintenanceReservationStatus_MAINTENANCE_RESERVATION_STATUS_ACTIVE {
			continue
		}
		addrs[r.Participant] = struct{}{}
	}
	return addrs
}
