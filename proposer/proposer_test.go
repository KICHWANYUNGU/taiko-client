package proposer

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/suite"
	"github.com/taikoxyz/taiko-client/bindings"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/pkg/jwt"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
	"github.com/taikoxyz/taiko-client/testutils"
)

type ProposerTestSuite struct {
	testutils.ClientSuite
	p         *Proposer
	cancel    context.CancelFunc
	RpcClient *rpc.Client
}

func (s *ProposerTestSuite) SetupTest() {
	s.ClientSuite.SetupTest()
	jwtSecret, err := jwt.ParseSecretFromFile(testutils.JwtSecretFile)
	s.NoError(err)
	s.RpcClient, err = rpc.NewClient(context.Background(), &rpc.ClientConfig{
		L1Endpoint:        s.L1.WsEndpoint(),
		L2Endpoint:        s.L2.WsEndpoint(),
		TaikoL1Address:    testutils.TaikoL1Address,
		TaikoTokenAddress: testutils.TaikoL1TokenAddress,
		TaikoL2Address:    testutils.TaikoL2Address,
		L2EngineEndpoint:  s.L2.AuthEndpoint(),
		JwtSecret:         string(jwtSecret),
		RetryInterval:     backoff.DefaultMaxInterval,
	})
	s.NoError(err)
	l1ProposerPrivKey := testutils.ProposerPrivKey
	s.Nil(err)

	p := new(Proposer)

	ctx, cancel := context.WithCancel(context.Background())
	proposeInterval := 1024 * time.Hour // No need to periodically propose transactions list in unit tests

	s.Nil(InitFromConfig(ctx, p, (&Config{
		L1Endpoint:                          s.L1.WsEndpoint(),
		L2Endpoint:                          s.L2.HttpEndpoint(),
		TaikoL1Address:                      testutils.TaikoL1Address,
		TaikoL2Address:                      testutils.TaikoL2Address,
		TaikoTokenAddress:                   testutils.TaikoL1TokenAddress,
		L1ProposerPrivKey:                   l1ProposerPrivKey,
		L2SuggestedFeeRecipient:             testutils.ProposerAddress,
		ProposeInterval:                     &proposeInterval,
		MaxProposedTxListsPerEpoch:          1,
		ProposeBlockTxReplacementMultiplier: 2,
		WaitReceiptTimeout:                  10 * time.Second,
		ProverEndpoints:                     s.ProverEndpoints,
		BlockProposalFee:                    common.Big256,
		BlockProposalFeeIncreasePercentage:  common.Big2,
		BlockProposalFeeIterations:          3,
	})))

	s.p = p
	s.cancel = cancel
}

func (s *ProposerTestSuite) TearDownTest() {
	s.RpcClient.Close()
	s.ClientSuite.TearDownTest()
}

func (s *ProposerTestSuite) TestName() {
	s.Equal("proposer", s.p.Name())
}

func (s *ProposerTestSuite) TestProposeOp() {
	// Propose txs in L2 execution engine's mempool
	sink := make(chan *bindings.TaikoL1ClientBlockProposed)

	sub, err := s.p.rpc.TaikoL1.WatchBlockProposed(nil, sink, nil, nil)
	s.Nil(err)
	defer func() {
		sub.Unsubscribe()
		close(sink)
	}()

	nonce, err := s.p.rpc.L2.PendingNonceAt(context.Background(), testutils.ProposerAddress)
	s.Nil(err)

	gaslimit := 21000

	parent, err := s.p.rpc.L2.BlockByNumber(context.Background(), nil)
	s.Nil(err)

	baseFee, err := s.p.rpc.TaikoL2.GetBasefee(nil, 1, uint32(parent.GasUsed()))
	s.Nil(err)

	to := common.BytesToAddress(testutils.RandomBytes(32))
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   s.RpcClient.L2ChainID,
		Nonce:     nonce,
		GasTipCap: common.Big0,
		GasFeeCap: new(big.Int).SetUint64(baseFee.Uint64() * 2),
		Gas:       uint64(gaslimit),
		To:        &to,
		Value:     common.Big1,
	})

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(s.p.rpc.L2ChainID), testutils.ProposerPrivKey)
	s.Nil(err)
	s.Nil(s.p.rpc.L2.SendTransaction(context.Background(), signedTx))

	s.Nil(s.p.ProposeOp(context.Background()))

	event := <-sink

	_, isPending, err := s.p.rpc.L1.TransactionByHash(context.Background(), event.Raw.TxHash)
	s.Nil(err)
	s.False(isPending)
	s.Equal(s.p.l2SuggestedFeeRecipient, event.Meta.Proposer)

	receipt, err := s.p.rpc.L1.TransactionReceipt(context.Background(), event.Raw.TxHash)
	s.Nil(err)
	s.Equal(types.ReceiptStatusSuccessful, receipt.Status)
}

func (s *ProposerTestSuite) TestProposeEmptyBlockOp() {
	s.Nil(s.p.ProposeEmptyBlockOp(context.Background()))
}

func (s *ProposerTestSuite) TestCustomProposeOpHook() {
	flag := false

	s.p.CustomProposeOpHook = func() error {
		flag = true
		return nil
	}

	s.Nil(s.p.ProposeOp(context.Background()))
	s.True(flag)
}

func (s *ProposerTestSuite) TestSendProposeBlockTx() {
	fee := big.NewInt(10000)
	opts, err := getTxOpts(
		context.Background(),
		s.p.rpc.L1,
		s.p.l1ProposerPrivKey,
		s.RpcClient.L1ChainID,
		fee,
	)
	s.Nil(err)
	s.Greater(opts.GasTipCap.Uint64(), uint64(0))

	nonce, err := s.RpcClient.L1.PendingNonceAt(context.Background(), s.p.l1ProposerAddress)
	s.Nil(err)

	tx := types.NewTransaction(
		nonce,
		common.BytesToAddress([]byte{}),
		common.Big1,
		100000,
		opts.GasTipCap,
		[]byte{},
	)

	s.SetL1Automine(false)
	defer s.SetL1Automine(true)

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(s.RpcClient.L1ChainID), s.p.l1ProposerPrivKey)
	s.Nil(err)
	s.Nil(s.RpcClient.L1.SendTransaction(context.Background(), signedTx))

	var emptyTxs []types.Transaction
	encoded, err := rlp.EncodeToBytes(emptyTxs)
	s.Nil(err)

	meta := &encoding.TaikoL1BlockMetadataInput{
		Proposer:        s.p.L2SuggestedFeeRecipient(),
		TxListHash:      crypto.Keccak256Hash(encoded),
		TxListByteStart: common.Big0,
		TxListByteEnd:   new(big.Int).SetUint64(uint64(len(encoded))),
		CacheTxListInfo: false,
	}

	assignment, fee, err := s.p.proverSelector.AssignProver(context.Background(), meta)
	s.Nil(err)

	newTx, err := s.p.sendProposeBlockTx(
		context.Background(),
		meta,
		encoded,
		&nonce,
		assignment,
		fee,
		true,
	)
	s.Nil(err)
	s.Greater(newTx.GasTipCap().Uint64(), tx.GasTipCap().Uint64())
}

func (s *ProposerTestSuite) TestAssignProverSuccessFirstRound() {
	meta := &encoding.TaikoL1BlockMetadataInput{
		Proposer:        s.p.L2SuggestedFeeRecipient(),
		TxListHash:      testutils.RandomHash(),
		TxListByteStart: common.Big0,
		TxListByteEnd:   common.Big0,
		CacheTxListInfo: false,
	}

	s.SetL1Automine(false)
	defer s.SetL1Automine(true)

	_, fee, err := s.p.proverSelector.AssignProver(context.Background(), meta)

	s.Nil(err)
	s.Equal(fee.Uint64(), s.p.cfg.BlockProposalFee.Uint64())
}

func (s *ProposerTestSuite) TestUpdateProposingTicker() {
	oneHour := 1 * time.Hour
	s.p.proposingInterval = &oneHour
	s.NotPanics(s.p.updateProposingTicker)

	s.p.proposingInterval = nil
	s.NotPanics(s.p.updateProposingTicker)
}

func (s *ProposerTestSuite) TestStartClose() {
	s.Nil(s.p.Start())
	s.cancel()
	s.NotPanics(func() { s.p.Close(context.Background()) })
}

func TestProposerTestSuite(t *testing.T) {
	suite.Run(t, new(ProposerTestSuite))
}
