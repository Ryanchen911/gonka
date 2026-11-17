package app

import (
	"errors"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/stretchr/testify/require"

	blstypes "github.com/productscience/inference/x/bls/types"
	inferencetypes "github.com/productscience/inference/x/inference/types"
)

func TestGonkaFeeChecker_BLSDutyMembership(t *testing.T) {
	actor := dutyActor()
	ik, ctx := registeredInactiveDutyKeeper(t, actor)
	params, err := ik.GetParams(ctx)
	require.NoError(t, err)
	params.FeeParams = inferencetypes.DefaultFeeParams()
	params.FeeParams.EnabledFeeGroups = []string{inferencetypes.FeeGroupBLS, inferencetypes.FeeGroupCosmos}
	params.FeeParams.Groups = append(params.FeeParams.Groups, &inferencetypes.FeeGroup{Name: inferencetypes.FeeGroupBLS, MinGasPrice: 10})
	params.FeeParams.GroupByName(inferencetypes.FeeGroupCosmos).MinGasPrice = 2
	require.NoError(t, ik.SetParams(ctx, params))
	for name, msg := range map[string]sdk.Msg{
		"dealer":     &blstypes.MsgSubmitDealerPart{Creator: actor.String(), EpochId: 10},
		"vector":     &blstypes.MsgSubmitVerificationVector{Creator: actor.String(), EpochId: 10},
		"complaints": &blstypes.MsgRespondDealerComplaints{Creator: actor.String(), EpochId: 10},
		"group key":  &blstypes.MsgSubmitGroupKeyValidationSignature{Creator: actor.String(), NewEpochId: 11},
		"signature":  &blstypes.MsgSubmitPartialSignature{Creator: actor.String(), RequestId: []byte("r")},
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				keeper    BLSMembershipKeeper
				wantError bool
			}{
				{"member", &fakeBLSMembership{members: map[uint64][]string{10: {actor.String()}}, request: map[string]uint64{"r": 10}}, false},
				{"non-member", &fakeBLSMembership{members: map[uint64][]string{10: {}}, request: map[string]uint64{"r": 10}}, true},
				{"missing keeper", nil, true},
				{"read error", &fakeBLSMembership{err: errors.New("store"), request: map[string]uint64{"r": 10}}, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// A paid ordinary message prevents the all-duty shortcut. A
					// member pays its price; an ineligible duty raises it to 10.
					tx := testFeeTx{msgs: []sdk.Msg{&banktypes.MsgSend{}, msg}, gas: 100_000,
						fee: sdk.NewCoins(sdk.NewInt64Coin("ngonka", 200_000))}
					_, _, err := GonkaFeeChecker(ik, tc.keeper)(ctx, tx)
					if tc.wantError {
						require.ErrorIs(t, err, sdkerrors.ErrInsufficientFee)
					} else {
						require.NoError(t, err)
					}
				})
			}
		})
	}
}
