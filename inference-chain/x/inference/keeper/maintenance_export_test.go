package keeper

import (
	"context"

	"github.com/cosmos/cosmos-sdk/x/group"
)

// FilterOutMaintenanceParticipants exposes the unexported method for tests.
func (k Keeper) FilterOutMaintenanceParticipants(ctx context.Context, members []*group.GroupMember) []*group.GroupMember {
	return k.filterOutMaintenanceParticipants(ctx, members)
}

// CreateTestGroupMembers creates mock group members for testing.
func (k Keeper) CreateTestGroupMembers(addresses ...string) []*group.GroupMember {
	members := make([]*group.GroupMember, len(addresses))
	for i, addr := range addresses {
		members[i] = &group.GroupMember{
			Member: &group.Member{
				Address: addr,
				Weight:  "1",
			},
		}
	}
	return members
}
