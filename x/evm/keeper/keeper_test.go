package keeper_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	"math"
	"math/big"
	"os"
	didtypes "swisstronik/x/did/types"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/simapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	tmjson "github.com/tendermint/tendermint/libs/json"
	feemarkettypes "swisstronik/x/feemarket/types"

	"swisstronik/app"
	"swisstronik/crypto/ethsecp256k1"
	"swisstronik/encoding"
	"swisstronik/server/config"
	"swisstronik/tests"
	evmcommontypes "swisstronik/types"
	"swisstronik/x/evm/types"
	evmtypes "swisstronik/x/evm/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"

	"github.com/SigmaGmbH/librustgo"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/tmhash"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	tmversion "github.com/tendermint/tendermint/proto/tendermint/version"
	"github.com/tendermint/tendermint/version"
)

type KeeperTestSuite struct {
	suite.Suite

	ctx         sdk.Context
	app         *app.App
	queryClient types.QueryClient
	address     common.Address
	consAddress sdk.ConsAddress

	// for generate test tx
	clientCtx client.Context
	ethSigner ethtypes.Signer

	appCodec codec.Codec
	signer   keyring.Signer

	enableFeemarket  bool
	enableLondonHF   bool
	mintFeeCollector bool
	denom            string

	privateKey    []byte
	nodePublicKey []byte
}

var s *KeeperTestSuite

func TestKeeperTestSuite(t *testing.T) {
	if os.Getenv("benchmark") != "" {
		t.Skip("Skipping Gingko Test")
	}
	s = new(KeeperTestSuite)
	s.enableFeemarket = false
	s.enableLondonHF = true
	suite.Run(t, s)

	// Run Ginkgo integration tests
	RegisterFailHandler(Fail)
	RunSpecs(t, "Keeper Suite")
}

func (suite *KeeperTestSuite) SetupTest() {
	checkTx := false
	suite.app = app.Setup(checkTx, nil)
	suite.SetupApp(checkTx)
}

func (suite *KeeperTestSuite) SetupTestWithT(t require.TestingT) {
	checkTx := false
	suite.app = app.Setup(checkTx, nil)
	suite.SetupAppWithT(checkTx, t)
}

func (suite *KeeperTestSuite) SetupApp(checkTx bool) {
	// Initialize enclave
	err := librustgo.InitializeMasterKey(false)
	require.NoError(suite.T(), err)

	suite.SetupAppWithT(checkTx, suite.T())
}

// SetupApp setup test environment, it uses`require.TestingT` to support both `testing.T` and `testing.B`.
func (suite *KeeperTestSuite) SetupAppWithT(checkTx bool, t require.TestingT) {
	// obtain node public key
	res, err := librustgo.GetNodePublicKey()
	suite.Require().NoError(err)
	suite.nodePublicKey = res.PublicKey

	// account key, use a constant account to keep unit test deterministic.
	ecdsaPriv, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	require.NoError(t, err)
	priv := &ethsecp256k1.PrivKey{
		Key: crypto.FromECDSA(ecdsaPriv),
	}

	suite.privateKey = priv.Bytes()
	suite.address = common.BytesToAddress(priv.PubKey().Address().Bytes())
	suite.signer = tests.NewSigner(priv)

	// consensus key
	priv, err = ethsecp256k1.GenerateKey()
	require.NoError(t, err)
	suite.consAddress = sdk.ConsAddress(priv.PubKey().Address())

	suite.app = app.Setup(checkTx, func(app *app.App, genesis simapp.GenesisState) simapp.GenesisState {
		feemarketGenesis := feemarkettypes.DefaultGenesisState()
		if suite.enableFeemarket {
			feemarketGenesis.Params.EnableHeight = 1
			feemarketGenesis.Params.NoBaseFee = false
		} else {
			feemarketGenesis.Params.NoBaseFee = true
		}
		genesis[feemarkettypes.ModuleName] = app.AppCodec().MustMarshalJSON(feemarketGenesis)
		if !suite.enableLondonHF {
			evmGenesis := types.DefaultGenesisState()
			maxInt := sdkmath.NewInt(math.MaxInt64)
			evmGenesis.Params.ChainConfig.LondonBlock = &maxInt
			evmGenesis.Params.ChainConfig.ArrowGlacierBlock = &maxInt
			evmGenesis.Params.ChainConfig.GrayGlacierBlock = &maxInt
			evmGenesis.Params.ChainConfig.MergeNetsplitBlock = &maxInt
			evmGenesis.Params.ChainConfig.ShanghaiBlock = &maxInt
			evmGenesis.Params.ChainConfig.CancunBlock = &maxInt
			genesis[types.ModuleName] = app.AppCodec().MustMarshalJSON(evmGenesis)
		}
		return genesis
	})

	if suite.mintFeeCollector {
		// mint some coin to fee collector
		coins := sdk.NewCoins(sdk.NewCoin(types.DefaultEVMDenom, sdkmath.NewInt(int64(params.TxGas)-1)))
		genesisState := app.NewTestGenesisState(suite.app.AppCodec())
		balances := []banktypes.Balance{
			{
				Address: suite.app.AccountKeeper.GetModuleAddress(authtypes.FeeCollectorName).String(),
				Coins:   coins,
			},
		}
		var bankGenesis banktypes.GenesisState
		suite.app.AppCodec().MustUnmarshalJSON(genesisState[banktypes.ModuleName], &bankGenesis)
		// Update balances and total supply
		bankGenesis.Balances = append(bankGenesis.Balances, balances...)
		bankGenesis.Supply = bankGenesis.Supply.Add(coins...)
		genesisState[banktypes.ModuleName] = suite.app.AppCodec().MustMarshalJSON(&bankGenesis)

		// we marshal the genesisState of all module to a byte array
		stateBytes, err := tmjson.MarshalIndent(genesisState, "", " ")
		require.NoError(t, err)

		// Initialize the chain
		suite.app.InitChain(
			abci.RequestInitChain{
				ChainId:         "ethermint_9000-1",
				Validators:      []abci.ValidatorUpdate{},
				ConsensusParams: app.DefaultConsensusParams,
				AppStateBytes:   stateBytes,
			},
		)
	}

	suite.ctx = suite.app.BaseApp.NewContext(checkTx, tmproto.Header{
		Height:          1,
		ChainID:         "ethermint_9000-1",
		Time:            time.Now().UTC(),
		ProposerAddress: suite.consAddress.Bytes(),
		Version: tmversion.Consensus{
			Block: version.BlockProtocol,
		},
		LastBlockId: tmproto.BlockID{
			Hash: tmhash.Sum([]byte("block_id")),
			PartSetHeader: tmproto.PartSetHeader{
				Total: 11,
				Hash:  tmhash.Sum([]byte("partset_header")),
			},
		},
		AppHash:            tmhash.Sum([]byte("app")),
		DataHash:           tmhash.Sum([]byte("data")),
		EvidenceHash:       tmhash.Sum([]byte("evidence")),
		ValidatorsHash:     tmhash.Sum([]byte("validators")),
		NextValidatorsHash: tmhash.Sum([]byte("next_validators")),
		ConsensusHash:      tmhash.Sum([]byte("consensus")),
		LastResultsHash:    tmhash.Sum([]byte("last_result")),
	})

	queryHelper := baseapp.NewQueryServerTestHelper(suite.ctx, suite.app.InterfaceRegistry())
	types.RegisterQueryServer(queryHelper, suite.app.EvmKeeper)
	suite.queryClient = types.NewQueryClient(queryHelper)

	acc := &evmcommontypes.EthAccount{
		BaseAccount: authtypes.NewBaseAccount(sdk.AccAddress(suite.address.Bytes()), nil, 0, 0),
		CodeHash:    common.BytesToHash(crypto.Keccak256(nil)).String(),
	}

	suite.app.AccountKeeper.SetAccount(suite.ctx, acc)

	valAddr := sdk.ValAddress(suite.address.Bytes())
	validator, err := stakingtypes.NewValidator(valAddr, priv.PubKey(), stakingtypes.Description{})
	require.NoError(t, err)
	err = suite.app.StakingKeeper.SetValidatorByConsAddr(suite.ctx, validator)
	require.NoError(t, err)
	err = suite.app.StakingKeeper.SetValidatorByConsAddr(suite.ctx, validator)
	require.NoError(t, err)
	suite.app.StakingKeeper.SetValidator(suite.ctx, validator)

	encodingConfig := encoding.MakeConfig(app.ModuleBasics)
	suite.clientCtx = client.Context{}.WithTxConfig(encodingConfig.TxConfig)
	suite.ethSigner = ethtypes.LatestSignerForChainID(suite.app.EvmKeeper.ChainID())
	suite.appCodec = encodingConfig.Codec
	suite.denom = evmtypes.DefaultEVMDenom
}

func (suite *KeeperTestSuite) EvmDenom() string {
	ctx := sdk.WrapSDKContext(suite.ctx)
	rsp, _ := suite.queryClient.Params(ctx, &types.QueryParamsRequest{})
	return rsp.Params.EvmDenom
}

// Commit and begin new block
func (suite *KeeperTestSuite) Commit() {
	_ = suite.app.Commit()
	header := suite.ctx.BlockHeader()
	header.Height += 1
	suite.app.BeginBlock(abci.RequestBeginBlock{
		Header: header,
	})

	// update ctx
	suite.ctx = suite.app.BaseApp.NewContext(false, header)

	queryHelper := baseapp.NewQueryServerTestHelper(suite.ctx, suite.app.InterfaceRegistry())
	types.RegisterQueryServer(queryHelper, suite.app.EvmKeeper)
	suite.queryClient = types.NewQueryClient(queryHelper)
}

// DeployVCContract deploy a test contract for verifying credentials and returns the contract address
func (suite *KeeperTestSuite) DeployVCContract(t require.TestingT) common.Address {
	ctx := sdk.WrapSDKContext(suite.ctx)
	chainID := suite.app.EvmKeeper.ChainID()

	hexTxData := "0x608060405234801561001057600080fd5b5061046f806100206000396000f3fe608060405234801561001057600080fd5b50600436106100365760003560e01c80631d73814b1461003b578063fe9fbb8014610057575b600080fd5b61005560048036038101906100509190610256565b610087565b005b610071600480360381019061006c9190610301565b610192565b60405161007e9190610349565b60405180910390f35b600061040373ffffffffffffffffffffffffffffffffffffffff1683836040516100b29291906103a3565b600060405180830381855afa9150503d80600081146100ed576040519150601f19603f3d011682016040523d82523d6000602084013e6100f2565b606091505b5050905080610136576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040161012d90610419565b60405180910390fd5b60016000803373ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060006101000a81548160ff021916908315150217905550505050565b60008060008373ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200190815260200160002060009054906101000a900460ff169050919050565b600080fd5b600080fd5b600080fd5b600080fd5b600080fd5b60008083601f840112610216576102156101f1565b5b8235905067ffffffffffffffff811115610233576102326101f6565b5b60208301915083600182028301111561024f5761024e6101fb565b5b9250929050565b6000806020838503121561026d5761026c6101e7565b5b600083013567ffffffffffffffff81111561028b5761028a6101ec565b5b61029785828601610200565b92509250509250929050565b600073ffffffffffffffffffffffffffffffffffffffff82169050919050565b60006102ce826102a3565b9050919050565b6102de816102c3565b81146102e957600080fd5b50565b6000813590506102fb816102d5565b92915050565b600060208284031215610317576103166101e7565b5b6000610325848285016102ec565b91505092915050565b60008115159050919050565b6103438161032e565b82525050565b600060208201905061035e600083018461033a565b92915050565b600081905092915050565b82818337600083830152505050565b600061038a8385610364565b935061039783858461036f565b82840190509392505050565b60006103b082848661037e565b91508190509392505050565b600082825260208201905092915050565b7f43616e6e6f74207665726966792063726564656e7469616c0000000000000000600082015250565b60006104036018836103bc565b915061040e826103cd565b602082019050919050565b60006020820190508181036000830152610432816103f6565b905091905056fea264697066735822122037cd764a4530efb679d82663e84edd198f7eb1a0c2259e71f3db9059313f500464736f6c63430008120033"
	ctorArgs, err := hexutil.Decode(hexTxData)

	require.NoError(t, err)

	nonce := suite.app.EvmKeeper.GetNonce(suite.ctx, suite.address)

	data := append(types.ERC20Contract.Bin, ctorArgs...)
	args, err := json.Marshal(&types.TransactionArgs{
		From: &suite.address,
		Data: (*hexutil.Bytes)(&data),
	})
	require.NoError(t, err)
	res, err := suite.queryClient.EstimateGas(ctx, &types.EthCallRequest{
		Args:            args,
		GasCap:          uint64(config.DefaultGasCap),
		ProposerAddress: suite.ctx.BlockHeader().ProposerAddress,
	})
	require.NoError(t, err)

	var deployTx *types.MsgHandleTx
	if suite.enableFeemarket {
		deployTx = types.NewTxContract(
			chainID,
			nonce,
			nil,     // amount
			res.Gas, // gasLimit
			nil,     // gasPrice
			suite.app.FeeMarketKeeper.GetBaseFee(suite.ctx),
			big.NewInt(1),
			data,                   // input
			&ethtypes.AccessList{}, // accesses
		)
	} else {
		deployTx = types.NewTxContract(
			chainID,
			nonce,
			nil,     // amount
			res.Gas, // gasLimit
			nil,     // gasPrice
			nil, nil,
			data, // input
			nil,  // accesses
		)
	}

	deployTx.From = suite.address.Hex()
	err = deployTx.Sign(ethtypes.LatestSignerForChainID(chainID), suite.signer)
	require.NoError(t, err)

	ethTx := &types.MsgHandleTx{
		Data: deployTx.Data,
		Hash: deployTx.Hash,
		From: deployTx.From,
	}
	rsp, err := suite.app.EvmKeeper.HandleTx(ctx, ethTx)
	require.NoError(t, err)
	require.Empty(t, rsp.VmError)
	return crypto.CreateAddress(suite.address, nonce)
}

// Sends sample transaction to verify VC
func (suite *KeeperTestSuite) SendVerifiableCredentialTx(t require.TestingT, contractAddr common.Address) {
	ctx := sdk.WrapSDKContext(suite.ctx)
	chainID := suite.app.EvmKeeper.ChainID()

	txDataHex := "0x1d73814b000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000001d3b901d065794a68624763694f694a465a45525451534973496e523563434936496b705856434a392e65794a325979493665794a4159323975644756346443493657794a6f64485277637a6f764c336433647935334d793576636d63764d6a41784f43396a636d566b5a57353061574673637939324d534a644c434a306558426c496a7062496c5a6c636d6c6d6157466962475644636d566b5a57353061574673496c3073496d4e795a57526c626e52705957785464574a715a574e30496a7037496d74355931426863334e6c5a43493664484a315a5377695a4746305a55396d516d6c79644767694f6949774d5441784d546b354d434a396653776963335669496a6f695a476c6b4f6e4e33644849364d6b68544d32687256556f324e446c5a5446464b5a6d524254444a7264534973496d35695a6949364d5459354e544d354d7a4d304f43776961584e7a496a6f695a476c6b4f6e4e33644849364d6b68544d32687256556f324e446c5a5446464b5a6d524254444a7264534a392e6f50374d56624931465a30466458737534593171706d457271545659794359516947352d396e6e622d596669357958445267665759627945612d48426c6c63433953776c4c4d76566568557250765841444c5161427700000000000000000000000000"
	txData, err := hexutil.Decode(txDataHex)
	require.NoError(t, err)

	nonce := suite.app.EvmKeeper.GetNonce(suite.ctx, suite.address)

	var tx *types.MsgHandleTx
	if suite.enableFeemarket {
		tx = evmtypes.NewSGXVMTx(
			chainID,
			nonce,
			&contractAddr,
			nil,
			500_000,
			nil,
			suite.app.FeeMarketKeeper.GetBaseFee(suite.ctx),
			big.NewInt(1),
			txData,
			&ethtypes.AccessList{},
			suite.privateKey,
			suite.nodePublicKey,
		)
	} else {
		tx = evmtypes.NewSGXVMTx(
			chainID,
			nonce,
			&contractAddr,
			nil,
			500_000,
			nil,
			nil,
			nil,
			txData,
			nil,
			suite.privateKey,
			suite.nodePublicKey,
		)
	}

	tx.From = suite.address.Hex()
	err = tx.Sign(ethtypes.LatestSignerForChainID(chainID), suite.signer)
	require.NoError(t, err)

	ethTx := &types.MsgHandleTx{
		Data: tx.Data,
		Hash: tx.Hash,
		From: tx.From,
	}
	rsp, err := suite.app.EvmKeeper.HandleTx(ctx, ethTx)
	require.NoError(t, err)
	require.Empty(t, rsp.VmError)
}

// DeployTestContract deploy a test erc20 contract and returns the contract address
func (suite *KeeperTestSuite) DeployTestContract(t require.TestingT, owner common.Address, supply *big.Int) common.Address {
	ctx := sdk.WrapSDKContext(suite.ctx)
	chainID := suite.app.EvmKeeper.ChainID()

	ctorArgs, err := types.ERC20Contract.ABI.Pack("", owner, supply)
	require.NoError(t, err)

	nonce := suite.app.EvmKeeper.GetNonce(suite.ctx, suite.address)

	data := append(types.ERC20Contract.Bin, ctorArgs...)
	args, err := json.Marshal(&types.TransactionArgs{
		From: &suite.address,
		Data: (*hexutil.Bytes)(&data),
	})
	require.NoError(t, err)
	res, err := suite.queryClient.EstimateGas(ctx, &types.EthCallRequest{
		Args:            args,
		GasCap:          uint64(config.DefaultGasCap),
		ProposerAddress: suite.ctx.BlockHeader().ProposerAddress,
	})
	require.NoError(t, err)

	var erc20DeployTx *types.MsgHandleTx
	if suite.enableFeemarket {
		erc20DeployTx = types.NewTxContract(
			chainID,
			nonce,
			nil,     // amount
			res.Gas, // gasLimit
			nil,     // gasPrice
			suite.app.FeeMarketKeeper.GetBaseFee(suite.ctx),
			big.NewInt(1),
			data,                   // input
			&ethtypes.AccessList{}, // accesses
		)
	} else {
		erc20DeployTx = types.NewTxContract(
			chainID,
			nonce,
			nil,     // amount
			res.Gas, // gasLimit
			nil,     // gasPrice
			nil, nil,
			data, // input
			nil,  // accesses
		)
	}

	erc20DeployTx.From = suite.address.Hex()
	err = erc20DeployTx.Sign(ethtypes.LatestSignerForChainID(chainID), suite.signer)
	require.NoError(t, err)

	ethTx := &types.MsgHandleTx{
		Data: erc20DeployTx.Data,
		Hash: erc20DeployTx.Hash,
		From: erc20DeployTx.From,
	}
	rsp, err := suite.app.EvmKeeper.HandleTx(ctx, ethTx)
	require.NoError(t, err)
	require.Empty(t, rsp.VmError)
	return crypto.CreateAddress(suite.address, nonce)
}

// DeployTestMessageCall deploy a test erc20 contract and returns the contract address
func (suite *KeeperTestSuite) DeployTestMessageCall(t require.TestingT) common.Address {
	ctx := sdk.WrapSDKContext(suite.ctx)
	chainID := suite.app.EvmKeeper.ChainID()

	data := types.TestMessageCall.Bin
	args, err := json.Marshal(&types.TransactionArgs{
		From: &suite.address,
		Data: (*hexutil.Bytes)(&data),
	})
	require.NoError(t, err)

	res, err := suite.queryClient.EstimateGas(ctx, &types.EthCallRequest{
		Args:            args,
		GasCap:          uint64(config.DefaultGasCap),
		ProposerAddress: suite.ctx.BlockHeader().ProposerAddress,
	})
	require.NoError(t, err)

	nonce := suite.app.EvmKeeper.GetNonce(suite.ctx, suite.address)

	var erc20DeployTx *types.MsgHandleTx
	if suite.enableFeemarket {
		erc20DeployTx = types.NewTxContract(
			chainID,
			nonce,
			nil,     // amount
			res.Gas, // gasLimit
			nil,     // gasPrice
			suite.app.FeeMarketKeeper.GetBaseFee(suite.ctx),
			big.NewInt(1),
			data,                   // input
			&ethtypes.AccessList{}, // accesses
		)
	} else {
		erc20DeployTx = types.NewTxContract(
			chainID,
			nonce,
			nil,     // amount
			res.Gas, // gasLimit
			nil,     // gasPrice
			nil, nil,
			data, // input
			nil,  // accesses
		)
	}

	erc20DeployTx.From = suite.address.Hex()
	err = erc20DeployTx.Sign(ethtypes.LatestSignerForChainID(chainID), suite.signer)
	require.NoError(t, err)

	ethTx := &types.MsgHandleTx{
		Data: erc20DeployTx.Data,
		Hash: erc20DeployTx.Hash,
		From: erc20DeployTx.From,
	}
	rsp, err := suite.app.EvmKeeper.HandleTx(ctx, ethTx)
	require.NoError(t, err)
	require.Empty(t, rsp.VmError)
	return crypto.CreateAddress(suite.address, nonce)
}

func (suite *KeeperTestSuite) TestBaseFee() {
	testCases := []struct {
		name            string
		enableLondonHF  bool
		enableFeemarket bool
		expectBaseFee   *big.Int
	}{
		{"not enable london HF, not enable feemarket", false, false, nil},
		{"enable london HF, not enable feemarket", true, false, big.NewInt(0)},
		{"enable london HF, enable feemarket", true, true, big.NewInt(1000000000)},
		{"not enable london HF, enable feemarket", false, true, nil},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.enableFeemarket = tc.enableFeemarket
			suite.enableLondonHF = tc.enableLondonHF
			suite.SetupTest()
			suite.app.EvmKeeper.BeginBlock(suite.ctx, abci.RequestBeginBlock{})
			evmParams := suite.app.EvmKeeper.GetParams(suite.ctx)
			ethCfg := evmParams.ChainConfig.EthereumConfig(suite.app.EvmKeeper.ChainID())
			baseFee := suite.app.EvmKeeper.GetBaseFee(suite.ctx, ethCfg)
			suite.Require().Equal(tc.expectBaseFee, baseFee)
		})
	}
	suite.enableFeemarket = false
	suite.enableLondonHF = true
}

func (suite *KeeperTestSuite) TestGetAccountStorage() {
	testCases := []struct {
		name     string
		malleate func()
		expRes   []int
	}{
		{
			"Only one account that's not a contract (no storage)",
			func() {},
			[]int{0},
		},
		{
			"Two accounts - one contract (with storage), one wallet",
			func() {
				supply := big.NewInt(100)
				suite.DeployTestContract(suite.T(), suite.address, supply)
			},
			[]int{2, 0},
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.SetupTest()
			tc.malleate()
			i := 0
			suite.app.AccountKeeper.IterateAccounts(suite.ctx, func(account authtypes.AccountI) bool {
				ethAccount, ok := account.(evmcommontypes.EthAccountI)
				if !ok {
					// ignore non EthAccounts
					return false
				}

				addr := ethAccount.EthAddress()
				storage := suite.app.EvmKeeper.GetAccountStorage(suite.ctx, addr)

				suite.Require().Equal(tc.expRes[i], len(storage))
				i++
				return false
			})
		})
	}
}

func (suite *KeeperTestSuite) TestGetAccountOrEmpty() {
	empty := types.Account{
		Balance:  new(big.Int),
		CodeHash: types.EmptyCodeHash,
	}

	supply := big.NewInt(100)
	contractAddr := suite.DeployTestContract(suite.T(), suite.address, supply)

	testCases := []struct {
		name     string
		addr     common.Address
		expEmpty bool
	}{
		{
			"unexisting account - get empty",
			common.Address{},
			true,
		},
		{
			"existing contract account",
			contractAddr,
			false,
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			res := suite.app.EvmKeeper.GetAccountOrEmpty(suite.ctx, tc.addr)
			if tc.expEmpty {
				suite.Require().Equal(empty, res)
			} else {
				suite.Require().NotEqual(empty, res)
			}
		})
	}
}

func (suite *KeeperTestSuite) TestGetNonce() {
	testCases := []struct {
		name          string
		address       common.Address
		expectedNonce uint64
		malleate      func()
	}{
		{
			"account not found",
			tests.GenerateAddress(),
			0,
			func() {},
		},
		{
			"existing account",
			suite.address,
			1,
			func() {
				suite.Require().NoError(
					suite.app.EvmKeeper.SetNonce(suite.ctx, suite.address, 1),
				)
			},
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			tc.malleate()

			nonce := suite.app.EvmKeeper.GetNonce(suite.ctx, tc.address)
			suite.Require().Equal(tc.expectedNonce, nonce)
		})
	}
}

func (suite *KeeperTestSuite) TestSetNonce() {
	testCases := []struct {
		name     string
		address  common.Address
		nonce    uint64
		malleate func()
	}{
		{
			"new account",
			tests.GenerateAddress(),
			10,
			func() {},
		},
		{
			"existing account",
			suite.address,
			99,
			func() {},
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.Require().NoError(
				suite.app.EvmKeeper.SetNonce(suite.ctx, tc.address, tc.nonce),
			)
			nonce := suite.app.EvmKeeper.GetNonce(suite.ctx, tc.address)
			suite.Require().Equal(tc.nonce, nonce)
		})
	}
}

func (suite *KeeperTestSuite) TestGetCodeHash() {
	addr := tests.GenerateAddress()
	baseAcc := &authtypes.BaseAccount{Address: sdk.AccAddress(addr.Bytes()).String()}
	suite.app.AccountKeeper.SetAccount(suite.ctx, baseAcc)

	testCases := []struct {
		name     string
		address  common.Address
		expHash  common.Hash
		malleate func()
	}{
		{
			"account not found",
			tests.GenerateAddress(),
			common.BytesToHash(types.EmptyCodeHash),
			func() {},
		},
		{
			"account not EthAccount type, EmptyCodeHash",
			addr,
			common.BytesToHash(types.EmptyCodeHash),
			func() {},
		},
		{
			"existing account",
			suite.address,
			crypto.Keccak256Hash([]byte("codeHash")),
			func() {
				err := suite.app.EvmKeeper.SetAccountCode(suite.ctx, suite.address, []byte("codeHash"))
				suite.Require().NoError(err)
			},
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			tc.malleate()

			hash := suite.app.EvmKeeper.GetAccountOrEmpty(suite.ctx, tc.address).CodeHash
			suite.Require().Equal(tc.expHash, common.BytesToHash(hash))
		})
	}
}

func (suite *KeeperTestSuite) TestSetCode() {
	addr := tests.GenerateAddress()
	baseAcc := &authtypes.BaseAccount{Address: sdk.AccAddress(addr.Bytes()).String()}
	suite.app.AccountKeeper.SetAccount(suite.ctx, baseAcc)

	testCases := []struct {
		name    string
		address common.Address
		code    []byte
		isNoOp  bool
	}{
		{
			"account not found",
			tests.GenerateAddress(),
			[]byte("code"),
			false,
		},
		{
			"account not EthAccount type",
			addr,
			nil,
			true,
		},
		{
			"existing account",
			suite.address,
			[]byte("code"),
			false,
		},
		{
			"existing account, code deleted from store",
			suite.address,
			nil,
			false,
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			prev, err := suite.app.EvmKeeper.GetAccountCode(suite.ctx, tc.address)
			suite.Require().NoError(err)

			err = suite.app.EvmKeeper.SetAccountCode(suite.ctx, tc.address, tc.code)
			suite.Require().NoError(err)

			post, err := suite.app.EvmKeeper.GetAccountCode(suite.ctx, tc.address)
			suite.Require().NoError(err)

			if tc.isNoOp {
				suite.Require().Equal(prev, post)
			} else {
				suite.Require().Equal(tc.code, post)
			}
		})
	}
}

func (suite *KeeperTestSuite) TestKeeperSetAccountCode() {
	addr := tests.GenerateAddress()
	baseAcc := &authtypes.BaseAccount{Address: sdk.AccAddress(addr.Bytes()).String()}
	suite.app.AccountKeeper.SetAccount(suite.ctx, baseAcc)

	testCases := []struct {
		name string
		code []byte
	}{
		{
			"set code",
			[]byte("codecodecode"),
		},
		{
			"delete code",
			nil,
		},
	}

	suite.SetupTest()

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			err := suite.app.EvmKeeper.SetAccountCode(suite.ctx, addr, tc.code)
			suite.Require().NoError(err)

			acct := suite.app.EvmKeeper.GetAccountWithoutBalance(suite.ctx, addr)
			suite.Require().NotNil(acct)

			if tc.code != nil {
				suite.Require().True(acct.IsContract())
			}

			codeHash := crypto.Keccak256Hash(tc.code)
			suite.Require().Equal(codeHash.Bytes(), acct.CodeHash)

			code := suite.app.EvmKeeper.GetCode(suite.ctx, common.BytesToHash(acct.CodeHash))
			suite.Require().Equal(tc.code, code)
		})
	}
}

func (suite *KeeperTestSuite) TestKeeperSetCode() {
	addr := tests.GenerateAddress()
	baseAcc := &authtypes.BaseAccount{Address: sdk.AccAddress(addr.Bytes()).String()}
	suite.app.AccountKeeper.SetAccount(suite.ctx, baseAcc)

	testCases := []struct {
		name     string
		codeHash []byte
		code     []byte
	}{
		{
			"set code",
			[]byte("codeHash"),
			[]byte("this is the code"),
		},
		{
			"delete code",
			[]byte("codeHash"),
			nil,
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.app.EvmKeeper.SetCode(suite.ctx, tc.codeHash, tc.code)
			key := suite.app.GetKey(types.StoreKey)
			store := prefix.NewStore(suite.ctx.KVStore(key), types.KeyPrefixCode)
			code := store.Get(tc.codeHash)

			suite.Require().Equal(tc.code, code)
		})
	}
}

func (suite *KeeperTestSuite) TestState() {
	testCases := []struct {
		name       string
		key, value common.Hash
	}{
		{
			"set state - delete from store",
			common.BytesToHash([]byte("key")),
			common.Hash{},
		},
		{
			"set state - update value",
			common.BytesToHash([]byte("key")),
			common.BytesToHash([]byte("value")),
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.app.EvmKeeper.SetState(suite.ctx, suite.address, tc.key, tc.value.Bytes())
			value := suite.app.EvmKeeper.GetState(suite.ctx, suite.address, tc.key)
			suite.Require().Equal(tc.value, common.BytesToHash(value))
		})
	}
}

func (suite *KeeperTestSuite) TestSuicide() {
	code := []byte("code")
	err := suite.app.EvmKeeper.SetAccountCode(suite.ctx, suite.address, code)
	suite.Require().NoError(err)

	addedCode, err := suite.app.EvmKeeper.GetAccountCode(suite.ctx, suite.address)
	suite.Require().NoError(err)

	suite.Require().Equal(code, addedCode)
	// Add state to account
	for i := 0; i < 5; i++ {
		suite.app.EvmKeeper.SetState(suite.ctx, suite.address, common.BytesToHash([]byte(fmt.Sprintf("key%d", i))), []byte(fmt.Sprintf("value%d", i)))
	}

	// Generate 2nd address
	privkey, _ := ethsecp256k1.GenerateKey()
	key, err := privkey.ToECDSA()
	suite.Require().NoError(err)
	addr2 := crypto.PubkeyToAddress(key.PublicKey)

	// Add code and state to account 2
	err = suite.app.EvmKeeper.SetAccountCode(suite.ctx, addr2, code)
	suite.Require().NoError(err)

	addedCode2, err := suite.app.EvmKeeper.GetAccountCode(suite.ctx, addr2)
	suite.Require().Equal(code, addedCode2)
	for i := 0; i < 5; i++ {
		suite.app.EvmKeeper.SetState(suite.ctx, addr2, common.BytesToHash([]byte(fmt.Sprintf("key%d", i))), []byte(fmt.Sprintf("value%d", i)))
	}

	// Destroy first contract
	err = suite.app.EvmKeeper.DeleteAccount(suite.ctx, suite.address)
	suite.Require().NoError(err)

	// Check code is deleted
	accCode, err := suite.app.EvmKeeper.GetAccountCode(suite.ctx, suite.address)
	suite.Require().NoError(err)
	suite.Require().Nil(accCode)

	// Check state is deleted
	var storage types.Storage
	suite.app.EvmKeeper.ForEachStorage(suite.ctx, suite.address, func(key, value common.Hash) bool {
		storage = append(storage, types.NewState(key, value))
		return true
	})
	suite.Require().Equal(0, len(storage))

	// Check account is deleted
	acc := suite.app.EvmKeeper.GetAccountOrEmpty(suite.ctx, suite.address)
	suite.Require().Equal(types.EmptyCodeHash, acc.CodeHash)

	// Check code is still present in addr2 and suicided is false
	code2, err := suite.app.EvmKeeper.GetAccountCode(suite.ctx, addr2)
	suite.Require().NoError(err)
	suite.Require().NotNil(code2)
}

func (suite *KeeperTestSuite) TestExist() {
	testCases := []struct {
		name     string
		address  common.Address
		malleate func()
		exists   bool
	}{
		{"success, account exists", suite.address, func() {}, true},
		{"success, account doesn't exist", tests.GenerateAddress(), func() {}, false},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			tc.malleate()
			exists := suite.app.EvmKeeper.GetAccount(suite.ctx, tc.address) != nil
			suite.Require().Equal(tc.exists, exists)
		})
	}
}

func (suite *KeeperTestSuite) TestEmpty() {
	testCases := []struct {
		name     string
		address  common.Address
		malleate func()
		empty    bool
	}{
		{
			"not empty, positive balance",
			suite.address,
			func() {
				err := suite.app.EvmKeeper.SetBalance(suite.ctx, suite.address, big.NewInt(100))
				suite.Require().NoError(err)
			},
			false,
		},
		{"empty, account doesn't exist", tests.GenerateAddress(), func() {}, true},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.SetupTest()
			tc.malleate()
			isEmpty := suite.app.EvmKeeper.GetAccount(suite.ctx, tc.address) == nil
			suite.Require().Equal(tc.empty, isEmpty)
		})
	}
}

func (suite *KeeperTestSuite) CreateTestTx(msg *types.MsgHandleTx, priv cryptotypes.PrivKey) authsigning.Tx {
	option, err := codectypes.NewAnyWithValue(&types.ExtensionOptionsEthereumTx{})
	suite.Require().NoError(err)

	txBuilder := suite.clientCtx.TxConfig.NewTxBuilder()
	builder, ok := txBuilder.(authtx.ExtensionOptionsTxBuilder)
	suite.Require().True(ok)

	builder.SetExtensionOptions(option)

	err = msg.Sign(suite.ethSigner, tests.NewSigner(priv))
	suite.Require().NoError(err)

	err = txBuilder.SetMsgs(msg)
	suite.Require().NoError(err)

	return txBuilder.GetTx()
}

func (suite *KeeperTestSuite) TestForEachStorage() {
	var storage types.Storage

	testCase := []struct {
		name      string
		malleate  func()
		callback  func(key, value common.Hash) (stop bool)
		expValues []common.Hash
	}{
		{
			"aggregate state",
			func() {
				for i := 0; i < 5; i++ {
					suite.app.EvmKeeper.SetState(suite.ctx, suite.address, common.BytesToHash([]byte(fmt.Sprintf("key%d", i))), []byte(fmt.Sprintf("value%d", i)))
				}
			},
			func(key, value common.Hash) bool {
				storage = append(storage, types.NewState(key, value))
				return true
			},
			[]common.Hash{
				common.BytesToHash([]byte("value0")),
				common.BytesToHash([]byte("value1")),
				common.BytesToHash([]byte("value2")),
				common.BytesToHash([]byte("value3")),
				common.BytesToHash([]byte("value4")),
			},
		},
		{
			"filter state",
			func() {
				suite.app.EvmKeeper.SetState(suite.ctx, suite.address, common.BytesToHash([]byte("key")), []byte("value"))
				suite.app.EvmKeeper.SetState(suite.ctx, suite.address, common.BytesToHash([]byte("filterkey")), []byte("filtervalue"))
			},
			func(key, value common.Hash) bool {
				if value == common.BytesToHash([]byte("filtervalue")) {
					storage = append(storage, types.NewState(key, value))
					return false
				}
				return true
			},
			[]common.Hash{
				common.BytesToHash([]byte("filtervalue")),
			},
		},
	}

	for _, tc := range testCase {
		suite.Run(tc.name, func() {
			suite.SetupTest() // reset
			tc.malleate()

			suite.app.EvmKeeper.ForEachStorage(suite.ctx, suite.address, tc.callback)
			suite.Require().Equal(len(tc.expValues), len(storage), fmt.Sprintf("Expected values:\n%v\nStorage Values\n%v", tc.expValues, storage))

			vals := make([]common.Hash, len(storage))
			for i := range storage {
				vals[i] = common.HexToHash(storage[i].Value)
			}

			suite.Require().ElementsMatch(tc.expValues, vals)
		})
		storage = types.Storage{}
	}
}

func (suite *KeeperTestSuite) TestSetBalance() {
	amount := big.NewInt(-10)

	testCases := []struct {
		name     string
		addr     common.Address
		malleate func()
		expErr   bool
	}{
		{
			"address without funds - invalid amount",
			suite.address,
			func() {},
			true,
		},
		{
			"mint to address",
			suite.address,
			func() {
				amount = big.NewInt(100)
			},
			false,
		},
		{
			"burn from address",
			suite.address,
			func() {
				amount = big.NewInt(60)
			},
			false,
		},
		{
			"address with funds - invalid amount",
			suite.address,
			func() {
				amount = big.NewInt(-10)
			},
			true,
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.SetupTest()
			tc.malleate()
			err := suite.app.EvmKeeper.SetBalance(suite.ctx, tc.addr, amount)
			if tc.expErr {
				suite.Require().Error(err)
			} else {
				balance := suite.app.EvmKeeper.GetBalance(suite.ctx, tc.addr)
				suite.Require().NoError(err)
				suite.Require().Equal(amount, balance)
			}
		})
	}
}

func (suite *KeeperTestSuite) TestDeleteAccount() {
	supply := big.NewInt(100)
	contractAddr := suite.DeployTestContract(suite.T(), suite.address, supply)

	testCases := []struct {
		name   string
		addr   common.Address
		expErr bool
	}{
		{
			"remove address",
			suite.address,
			false,
		},
		{
			"remove unexistent address - returns nil error",
			common.HexToAddress("unexistent_address"),
			false,
		},
		{
			"remove deployed contract",
			contractAddr,
			false,
		},
	}

	for _, tc := range testCases {
		suite.Run(tc.name, func() {
			suite.SetupTest()
			err := suite.app.EvmKeeper.DeleteAccount(suite.ctx, tc.addr)
			if tc.expErr {
				suite.Require().Error(err)
			} else {
				suite.Require().NoError(err)
				balance := suite.app.EvmKeeper.GetBalance(suite.ctx, tc.addr)
				suite.Require().Equal(new(big.Int), balance)
			}
		})
	}
}

func (suite *KeeperTestSuite) TestVerifyCredential() {
	// Add verifiable credential
	var err error

	// Create DID Document for issuer
	metadata := didtypes.Metadata{
		Created:   time.Now(),
		VersionId: "123e4567-e89b-12d3-a456-426655440000",
	}
	didUrl := "did:swtr:2MGhkRKWKi7ztnBFa8DpQ3"
	verificationMethods := []*didtypes.VerificationMethod{{
		Id:                     "did:swtr:2MGhkRKWKi7ztnBFa8DpQ3#6c1527f2f57601ea2f481a0ab1e605d196f3d952689299491638925cd6f26a7e-1",
		VerificationMethodType: "Ed25519VerificationKey2020",
		Controller:             didUrl,
		VerificationMaterial:   "z6MkmjAncvMFDiqyquFLt2G3CGYaqLfDjqKuzJnmUX3y68JZ",
	}}
	document := didtypes.DIDDocument{
		Id:                 didUrl,
		Controller:         []string{didUrl},
		VerificationMethod: verificationMethods,
		Authentication:     []string{"did:swtr:2MGhkRKWKi7ztnBFa8DpQ3#6c1527f2f57601ea2f481a0ab1e605d196f3d952689299491638925cd6f26a7e-1"},
	}
	didDocument := didtypes.DIDDocumentWithMetadata{
		Metadata: &metadata,
		DidDoc:   &document,
	}
	err = suite.app.EvmKeeper.DIDKeeper.AddNewDIDDocumentVersion(suite.ctx, &didDocument)
	suite.Require().NoError(err)

	// Deploy contract VC.sol
	vcContract := suite.DeployVCContract(suite.T())

	// Send transaction to verify credentials
	suite.SendVerifiableCredentialTx(suite.T(), vcContract)
}
