package v0_2_12

import (
	"context"
	"errors"

	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	distrkeeper "github.com/cosmos/cosmos-sdk/x/distribution/keeper"
	blskeeper "github.com/productscience/inference/x/bls/keeper"
	blstypes "github.com/productscience/inference/x/bls/types"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

func CreateUpgradeHandler(
	mm *module.Manager,
	configurator module.Configurator,
	k keeper.Keeper,
	distrKeeper distrkeeper.Keeper,
	blsKeeper blskeeper.Keeper,
) upgradetypes.UpgradeHandler {
	return func(ctx context.Context, plan upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		k.LogInfo("starting upgrade", types.Upgrades, "version", UpgradeName)

		// Keep capability module version explicit to avoid re-running InitGenesis
		// on chains where capability state already exists but version map is missing.
		if _, ok := fromVM["capability"]; !ok {
			fromVM["capability"] = mm.Modules["capability"].(module.HasConsensusVersion).ConsensusVersion()
		}

		err := removeTopMiner(ctx, k)
		if err != nil {
			return nil, err
		}

		err = clearTrainingState(ctx, k)
		if err != nil {
			return nil, err
		}

		err = adjustParameters(ctx, k)
		if err != nil {
			return nil, err
		}

		err = adjustBLSParameters(ctx, blsKeeper)
		if err != nil {
			return nil, err
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

func adjustParameters(ctx context.Context, k keeper.Keeper) error {
	// For start, a simple roundtrip for params to clear out now-removed values
	params, err := k.GetParams(ctx)
	if err != nil {
		return err
	}
	params.XXX_DiscardUnknown()

	if params.ValidationParams == nil {
		params.ValidationParams = types.DefaultValidationParams()
	}
	params.ValidationParams.LogprobsMode = types.DefaultLogprobsMode

	err = k.SetParams(ctx, params)
	if err != nil {
		return err
	}

	genesisParams, found := k.GetGenesisOnlyParams(ctx)
	if !found {
		return errors.New("genesis only params not found")
	}
	genesisParams.XXX_DiscardUnknown()
	err = k.SetGenesisOnlyParams(ctx, &genesisParams)
	if err != nil {
		return err
	}
	return nil
}

func adjustBLSParameters(ctx context.Context, blsKeeper blskeeper.Keeper) error {
	params, err := blsKeeper.GetParams(ctx)
	if err != nil {
		return err
	}

	defaults := blstypes.DefaultParams()
	if params.ITotalSlots == 0 {
		params = defaults
	}
	if params.DisputePhaseDurationBlocks <= 0 {
		params.DisputePhaseDurationBlocks = defaults.DisputePhaseDurationBlocks
	}
	if params.MaxSigningAttempts == 0 {
		params.MaxSigningAttempts = defaults.MaxSigningAttempts
	}

	return blsKeeper.SetParams(ctx, params)
}

func removeTopMiner(ctx context.Context, k keeper.Keeper) error {
	err := k.TopMiners.Clear(ctx, nil)
	if err != nil {
		return err
	}
	tokenomicsData, found := k.GetTokenomicsData(ctx)
	if !found {
		return errors.New("tokenomics data not found")
	}
	tokenomicsData.XXX_DiscardUnknown()
	err = k.SetTokenomicsData(ctx, tokenomicsData)
	if err != nil {
		return err
	}
	return nil
}

func clearTrainingState(ctx context.Context, k keeper.Keeper) error {
	return k.ClearTrainingState(ctx)
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
