package app_test

import (
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	authztypes "github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/stretchr/testify/require"

	inferencetypes "github.com/productscience/inference/x/inference/types"
)

func TestNetworkDuty_CheckTx_BLS_MixedFeeWiring(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		name := "direct"
		if wrapped {
			name = "warm key"
		}
		t.Run(name, func(t *testing.T) {
			f := setupBLSDuty(t)
			ctx := f.testApp.NewUncachedContext(false, cmtproto.Header{
				Height: f.testApp.LastBlockHeight(), ChainID: TallyTestChainID, Time: time.Now().UTC(),
			})
			params, err := f.testApp.InferenceKeeper.GetParams(ctx)
			require.NoError(t, err)
			params.FeeParams = inferencetypes.DefaultFeeParams()
			params.FeeParams.EnabledFeeGroups = []string{inferencetypes.FeeGroupBLS}
			params.FeeParams.Groups = append(params.FeeParams.Groups, &inferencetypes.FeeGroup{Name: inferencetypes.FeeGroupBLS, MinGasPrice: 10})
			require.NoError(t, f.testApp.InferenceKeeper.SetParams(ctx, params))
			expires := time.Now().UTC().Add(time.Hour)
			require.NoError(t, f.testApp.AuthzKeeper.SaveGrant(ctx, f.grantee, f.participant,
				authztypes.NewGenericAuthorization(sdk.MsgTypeURL(&banktypes.MsgSend{})), &expires))
			_, err = f.testApp.FinalizeBlock(&abci.RequestFinalizeBlock{Height: f.testApp.LastBlockHeight() + 1, Time: time.Now().UTC()})
			require.NoError(t, err)
			_, err = f.testApp.Commit()
			require.NoError(t, err)
			for _, member := range []bool{true, false} {
				actor, priv := f.registered, f.registeredPriv
				if member {
					actor, priv = f.participant, f.partPriv
				}
				msgs := []sdk.Msg{
					&banktypes.MsgSend{FromAddress: actor.String(), ToAddress: f.outsider.String(), Amount: sdk.NewCoins(sdk.NewInt64Coin("ngonka", 1))},
					verificationVectorFor(actor, dutyEpochIndex),
				}
				if wrapped {
					exec := authztypes.NewMsgExec(f.grantee, msgs)
					msgs = []sdk.Msg{&exec}
					actor, priv = f.grantee, f.granteePriv
				}
				resp := runDutyCheckTx(t, f, msgs, actor, priv)
				if member {
					requireAdmitted(t, resp, "mixed DKG member duty plus disabled bank group")
				} else {
					require.Equal(t, sdkerrors.ErrInsufficientFee.ABCICode(), resp.Code, resp.Log)
				}
			}
		})
	}
}
