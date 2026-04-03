package v0_2_12

import (
	"context"

	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

func CreateUpgradeHandler(
	mm *module.Manager,
	configurator module.Configurator,
	k keeper.Keeper,
) upgradetypes.UpgradeHandler {
	return func(ctx context.Context, plan upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		k.LogInfo("starting upgrade", types.Upgrades, "version", UpgradeName)

		if _, ok := fromVM["capability"]; !ok {
			fromVM["capability"] = mm.Modules["capability"].(module.HasConsensusVersion).ConsensusVersion()
		}

		// Initialize maintenance params with defaults for existing chains.
		// All participants start with zero credit — credit is earned going forward.
		initMaintenanceParams(ctx, k)

		toVM, err := mm.RunMigrations(ctx, configurator, fromVM)
		if err != nil {
			return toVM, err
		}

		k.LogInfo("successfully upgraded", types.Upgrades, "version", UpgradeName)
		return toVM, nil
	}
}

// initMaintenanceParams initializes the MaintenanceParams sub-struct with defaults.
// The feature starts disabled; governance can enable it once the network is ready.
// No per-participant state initialization is needed because:
//   - MaintenanceState is lazily created via GetOrCreateMaintenanceState
//   - Credit starts at zero (the default for a missing entry)
//   - Maintenance collections (reservations, transitions, indexes) start empty
func initMaintenanceParams(ctx context.Context, k keeper.Keeper) {
	params, err := k.GetParams(ctx)
	if err != nil {
		k.LogError("failed to get params during upgrade", types.Upgrades, "error", err)
		return
	}

	if params.MaintenanceParams == nil {
		params.MaintenanceParams = types.DefaultMaintenanceParams()
	}

	if err := k.SetParams(ctx, params); err != nil {
		k.LogError("failed to set maintenance params during upgrade", types.Upgrades, "error", err)
		return
	}

	k.LogInfo("initialized maintenance params", types.Upgrades,
		"maintenance_enabled", params.MaintenanceParams.MaintenanceEnabled,
		"min_schedule_lead_blocks", params.MaintenanceParams.MaintenanceMinScheduleLeadBlocks,
		"max_window_blocks", params.MaintenanceParams.MaintenanceMaxWindowBlocks,
		"max_concurrent_validators", params.MaintenanceParams.MaintenanceMaxConcurrentValidators,
		"max_concurrent_power_bps", params.MaintenanceParams.MaintenanceMaxConcurrentPowerBps,
		"credit_cap_blocks", params.MaintenanceParams.MaintenanceCreditCapBlocks,
		"credit_earn_per_epoch_blocks", params.MaintenanceParams.MaintenanceCreditEarnPerSuccessfulEpochBlocks,
	)
}
