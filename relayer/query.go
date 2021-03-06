package relayer

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authTypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	bankTypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/cosmos-sdk/x/ibc/02-client/exported"
	clientExported "github.com/cosmos/cosmos-sdk/x/ibc/02-client/exported"
	clientTypes "github.com/cosmos/cosmos-sdk/x/ibc/02-client/types"
	connState "github.com/cosmos/cosmos-sdk/x/ibc/03-connection/exported"
	connTypes "github.com/cosmos/cosmos-sdk/x/ibc/03-connection/types"
	chanState "github.com/cosmos/cosmos-sdk/x/ibc/04-channel/exported"
	chanTypes "github.com/cosmos/cosmos-sdk/x/ibc/04-channel/types"
	tmclient "github.com/cosmos/cosmos-sdk/x/ibc/07-tendermint/types"
	commitmenttypes "github.com/cosmos/cosmos-sdk/x/ibc/23-commitment/types"
	ibctypes "github.com/cosmos/cosmos-sdk/x/ibc/types"
	abci "github.com/tendermint/tendermint/abci/types"
	rpcclient "github.com/tendermint/tendermint/rpc/client"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tmtypes "github.com/tendermint/tendermint/types"
)

// NOTE: This file contains logic for querying the Tendermint RPC port of a configured chain
// All the operations here hit the network and data coming back may be untrusted.
// These functions by convention are named Query*

// TODO: Validate all info coming back from these queries using the verifier

// QueryBalance returns the amount of coins in the relayer account
func (c *Chain) QueryBalance(keyName string) (sdk.Coins, error) {
	var (
		bz    []byte
		err   error
		coins sdk.Coins
		addr  sdk.AccAddress
		route = fmt.Sprintf("custom/%s/%s", bankTypes.QuerierRoute, bankTypes.QueryAllBalances)
	)
	if keyName == "" {
		addr = c.MustGetAddress()
	} else {
		info, err := c.Keybase.Get(keyName)
		if err != nil {
			return nil, err
		}
		addr = info.GetAddress()
	}

	if bz, err = c.Cdc.MarshalJSON(bankTypes.NewQueryAllBalancesParams(addr)); err != nil {
		return nil, qBalErr(addr, err)
	}

	if bz, _, err = c.QueryWithData(route, bz); err != nil {
		return nil, qBalErr(addr, err)
	}

	if err = c.Cdc.UnmarshalJSON(bz, &coins); err != nil {
		return nil, qBalErr(addr, err)
	}

	return coins, nil
}

func qBalErr(acc sdk.AccAddress, err error) error {
	return fmt.Errorf("query balance for acct %s failed: %w", acc.String(), err)
}

//////////////////////////////
//    ICS 02 -> CLIENTS     //
//////////////////////////////

// QueryConsensusState returns a consensus state for a given chain to be used as a
// client in another chain, fetches latest height when passed 0 as arg
func (c *Chain) QueryConsensusState(height int64) (*tmclient.ConsensusState, error) {
	var (
		commit     *ctypes.ResultCommit
		validators *ctypes.ResultValidators
		err        error
	)

	if height == 0 {
		commit, err = c.Client.Commit(nil)
		if err != nil {
			return nil, qConsStateErr(err)
		}
		validators, err = c.Client.Validators(nil, 1, 10000)
	} else {
		commit, err = c.Client.Commit(&height)
		if err != nil {
			return nil, qConsStateErr(err)
		}
		validators, err = c.Client.Validators(nil, 1, 10000)
	}

	if err != nil {
		return nil, qConsStateErr(err)
	}

	state := &tmclient.ConsensusState{
		Timestamp:    commit.Time,
		Root:         commitmenttypes.NewMerkleRoot(commit.AppHash),
		ValidatorSet: tmtypes.NewValidatorSet(validators.Validators),
	}

	return state, nil
}

func qConsStateErr(err error) error { return fmt.Errorf("query cons state failed: %w", err) }

// QueryClientConsensusState retrevies the latest consensus state for a client in state at a given height
// NOTE: dstHeight is the height from dst that is stored on src, it is needed to construct the appropriate store query
func (c *Chain) QueryClientConsensusState(srcHeight, srcClientConsHeight int64) (clientTypes.ConsensusStateResponse, error) {
	var conStateRes clientTypes.ConsensusStateResponse
	if !c.PathSet() {
		return conStateRes, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Height: int64(srcHeight),
		Data:   ibctypes.KeyConsensusState(c.PathEnd.ClientID, uint64(srcClientConsHeight)),
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return conStateRes, qClntConsStateErr(err)
	} else if res.Value == nil {
		// TODO: Better way to handle this?
		return clientTypes.NewConsensusStateResponse("notfound", nil, nil, 0), nil
	}

	var cs exported.ConsensusState
	if err := c.Amino.UnmarshalBinaryLengthPrefixed(res.Value, &cs); err != nil {
		return conStateRes, qClntConsStateErr(err)
	}

	return clientTypes.NewConsensusStateResponse(c.PathEnd.ClientID, cs, res.Proof, res.Height), nil
}

type csstates struct {
	sync.Mutex
	Map  map[string]clientTypes.ConsensusStateResponse
	Errs errs
}

type chh struct {
	c   *Chain
	h   int64
	csh int64
}

// QueryClientConsensusStatePair allows for the querying of multiple client states at the same time
func QueryClientConsensusStatePair(src, dst *Chain, srcH, dstH, srcClientConsH, dstClientConsH int64) (map[string]clientTypes.ConsensusStateResponse, error) {
	hs := &csstates{
		Map:  make(map[string]clientTypes.ConsensusStateResponse),
		Errs: []error{},
	}

	var wg sync.WaitGroup

	chps := []chh{
		{src, srcH, srcClientConsH},
		{dst, dstH, dstClientConsH},
	}

	for _, chain := range chps {
		wg.Add(1)
		go func(hs *csstates, wg *sync.WaitGroup, chp chh) {
			conn, err := chp.c.QueryClientConsensusState(chp.h, chp.csh)
			if err != nil {
				hs.Lock()
				hs.Errs = append(hs.Errs, err)
				hs.Unlock()
			}
			hs.Lock()
			hs.Map[chp.c.ChainID] = conn
			hs.Unlock()
			wg.Done()
		}(hs, &wg, chain)
	}
	wg.Wait()
	return hs.Map, hs.Errs.err()
}

func qClntConsStateErr(err error) error { return fmt.Errorf("query client cons state failed: %w", err) }

// QueryClientState retrevies the latest consensus state for a client in state at a given height
func (c *Chain) QueryClientState() (clientTypes.StateResponse, error) {
	var conStateRes clientTypes.StateResponse
	if !c.PathSet() {
		return conStateRes, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:  "store/ibc/key",
		Data:  ibctypes.KeyClientState(c.PathEnd.ClientID),
		Prove: true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return conStateRes, qClntStateErr(err)
	} else if res.Value == nil {
		// TODO: Better way to handle this?
		return clientTypes.StateResponse{}, nil
	}

	var cs exported.ClientState
	if err := c.Amino.UnmarshalBinaryLengthPrefixed(res.Value, &cs); err != nil {
		return conStateRes, qClntStateErr(err)
	}

	return clientTypes.NewClientStateResponse(c.PathEnd.ClientID, cs, res.Proof, res.Height), nil
}

type cstates struct {
	sync.Mutex
	Map  map[string]clientTypes.StateResponse
	Errs errs
}

// QueryClientStatePair returns a pair of connection responses
func QueryClientStatePair(src, dst *Chain) (map[string]clientTypes.StateResponse, error) {
	hs := &cstates{
		Map:  make(map[string]clientTypes.StateResponse),
		Errs: []error{},
	}

	var wg sync.WaitGroup

	chps := []*Chain{src, dst}

	for _, chain := range chps {
		wg.Add(1)
		go func(hs *cstates, wg *sync.WaitGroup, c *Chain) {
			conn, err := c.QueryClientState()
			if err != nil {
				hs.Lock()
				hs.Errs = append(hs.Errs, err)
				hs.Unlock()
			}
			hs.Lock()
			hs.Map[c.ChainID] = conn
			hs.Unlock()
			wg.Done()
		}(hs, &wg, chain)
	}
	wg.Wait()
	return hs.Map, hs.Errs.err()
}

func qClntStateErr(err error) error { return fmt.Errorf("query client state failed: %w", err) }

// QueryClients queries all the clients!
func (c *Chain) QueryClients(page, limit int) ([]clientExported.ClientState, error) {
	var (
		bz      []byte
		err     error
		clients []clientExported.ClientState
	)

	if bz, err = c.Cdc.MarshalJSON(clientTypes.NewQueryAllClientsParams(page, limit)); err != nil {
		return nil, qClntsErr(err)
	}

	if bz, _, err = c.QueryWithData(ibcQuerierRoute(clientTypes.QuerierRoute, clientTypes.QueryAllClients), bz); err != nil {
		return nil, qClntsErr(err)
	}

	if err = c.Cdc.UnmarshalJSON(bz, &clients); err != nil {
		return nil, qClntsErr(err)
	}

	return clients, nil
}

func qClntsErr(err error) error { return fmt.Errorf("query clients failed: %w", err) }

//////////////////////////////
//  ICS 03 -> CONNECTIONS   //
//////////////////////////////

// QueryConnections gets any connections on a chain
func (c *Chain) QueryConnections(page, limit int) (conns []connTypes.ConnectionEnd, err error) {
	var bz []byte
	if bz, err = c.Cdc.MarshalJSON(connTypes.NewQueryAllConnectionsParams(page, limit)); err != nil {
		return nil, qConnsErr(err)
	}

	if bz, _, err = c.QueryWithData(ibcQuerierRoute(connTypes.QuerierRoute, connTypes.QueryAllConnections), bz); err != nil {
		return nil, qConnsErr(err)
	}

	if err = c.Cdc.UnmarshalJSON(bz, &conns); err != nil {
		return nil, qConnsErr(err)
	}

	return conns, nil
}

func qConnsErr(err error) error { return fmt.Errorf("query connections failed: %w", err) }

// QueryConnectionsUsingClient gets any connections that exist between chain and counterparty
func (c *Chain) QueryConnectionsUsingClient(height int64) (clientConns connTypes.ClientConnectionsResponse, err error) {
	if !c.PathSet() {
		return clientConns, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Height: height,
		Data:   ibctypes.KeyClientConnections(c.PathEnd.ClientID),
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return clientConns, qConnsUsingClntsErr(err)
	}

	var paths []string
	if err := c.Amino.UnmarshalBinaryLengthPrefixed(res.Value, &paths); err != nil {
		return clientConns, qConnsUsingClntsErr(err)
	}

	return connTypes.NewClientConnectionsResponse(c.PathEnd.ClientID, paths, res.Proof, res.Height), nil
}

func qConnsUsingClntsErr(err error) error {
	return fmt.Errorf("query connections using clients failed: %w", err)
}

// QueryConnection returns the remote end of a given connection
func (c *Chain) QueryConnection(height int64) (connTypes.ConnectionResponse, error) {
	if !c.PathSet() {
		return connTypes.ConnectionResponse{}, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Data:   ibctypes.KeyConnection(c.PathEnd.ConnectionID),
		Height: height,
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return connTypes.ConnectionResponse{}, qConnErr(err)
	} else if res.Value == nil {
		// NOTE: This is returned so that the switch statement in ConnectionStep works properly
		return emptyConnRes, nil
	}

	var connection connTypes.ConnectionEnd
	if err := c.Amino.UnmarshalBinaryLengthPrefixed(res.Value, &connection); err != nil {
		return connTypes.ConnectionResponse{}, qConnErr(err)
	}

	return connTypes.NewConnectionResponse(c.PathEnd.ConnectionID, connection, res.Proof, res.Height), nil
}

type conns struct {
	sync.Mutex
	Map  map[string]connTypes.ConnectionResponse
	Errs errs
}

type chpair struct {
	c *Chain
	h int64
}

// QueryConnectionPair returns a pair of connection responses
func QueryConnectionPair(src, dst *Chain, srcH, dstH int64) (map[string]connTypes.ConnectionResponse, error) {
	hs := &conns{
		Map:  make(map[string]connTypes.ConnectionResponse),
		Errs: []error{},
	}

	var wg sync.WaitGroup

	chps := []chpair{
		{src, srcH},
		{dst, dstH},
	}

	for _, chain := range chps {
		wg.Add(1)
		go func(hs *conns, wg *sync.WaitGroup, chp chpair) {
			conn, err := chp.c.QueryConnection(chp.h)
			if err != nil {
				hs.Lock()
				hs.Errs = append(hs.Errs, err)
				hs.Unlock()
			}
			hs.Lock()
			hs.Map[chp.c.ChainID] = conn
			hs.Unlock()
			wg.Done()
		}(hs, &wg, chain)
	}
	wg.Wait()
	return hs.Map, hs.Errs.err()
}

// // QueryLatestHeights returns the heights of multiple chains at once
// func QueryLatestHeights(chains ...*Chain) (map[string]int64, error) {

// }

func qConnErr(err error) error { return fmt.Errorf("query connection failed: %w", err) }

var emptyConnRes = connTypes.ConnectionResponse{Connection: connTypes.ConnectionEnd{State: connState.UNINITIALIZED}}

//////////////////////////////
//    ICS 04 -> CHANNEL     //
//////////////////////////////

// QueryChannel returns the channel associated with a channelID
func (c *Chain) QueryChannel(height int64) (chanRes chanTypes.ChannelResponse, err error) {
	if !c.PathSet() {
		return chanRes, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Data:   ibctypes.KeyChannel(c.PathEnd.PortID, c.PathEnd.ChannelID),
		Height: height,
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return chanRes, qChanErr(err)
	} else if res.Value == nil {
		// NOTE: This is returned so that the switch statement in ChannelStep works properly
		return emptyChanRes, nil
	}

	var channel chanTypes.Channel
	if err := c.Amino.UnmarshalBinaryLengthPrefixed(res.Value, &channel); err != nil {
		return chanRes, qChanErr(err)
	}

	return chanTypes.NewChannelResponse(c.PathEnd.PortID, c.PathEnd.ChannelID, channel, res.Proof, res.Height), nil
}

type chans struct {
	sync.Mutex
	Map  map[string]chanTypes.ChannelResponse
	Errs errs
}

// QueryChannelPair returns a pair of connection responses
func QueryChannelPair(src, dst *Chain, srcH, dstH int64) (map[string]chanTypes.ChannelResponse, error) {
	hs := &chans{
		Map:  make(map[string]chanTypes.ChannelResponse),
		Errs: []error{},
	}

	var wg sync.WaitGroup

	chps := []chpair{
		{src, srcH},
		{dst, dstH},
	}

	for _, chain := range chps {
		wg.Add(1)
		go func(hs *chans, wg *sync.WaitGroup, chp chpair) {
			conn, err := chp.c.QueryChannel(chp.h)
			if err != nil {
				hs.Lock()
				hs.Errs = append(hs.Errs, err)
				hs.Unlock()
			}
			hs.Lock()
			hs.Map[chp.c.ChainID] = conn
			hs.Unlock()
			wg.Done()
		}(hs, &wg, chain)
	}
	wg.Wait()
	return hs.Map, hs.Errs.err()
}

func qChanErr(err error) error { return fmt.Errorf("query channel failed: %w", err) }

var emptyChanRes = chanTypes.ChannelResponse{Channel: chanTypes.Channel{State: chanState.UNINITIALIZED}}

// QueryChannels returns all the channels that are registered on a chain
func (c *Chain) QueryChannels(page, limit int) ([]chanTypes.Channel, error) {
	var (
		bz       []byte
		err      error
		channels []chanTypes.Channel
	)

	if bz, err = c.Cdc.MarshalJSON(chanTypes.NewQueryAllChannelsParams(page, limit)); err != nil {
		return nil, qChansErr(err)
	}

	if bz, _, err = c.QueryWithData(ibcQuerierRoute(chanTypes.QuerierRoute, chanTypes.QueryAllChannels), bz); err != nil {
		return nil, qChansErr(err)
	}

	if err = c.Cdc.UnmarshalJSON(bz, &channels); err != nil {
		return nil, qChansErr(err)
	}

	return channels, nil
}

func qChansErr(err error) error { return fmt.Errorf("query channels failed: %w", err) }

// WaitForNBlocks blocks until the next block on a given chain
func (c *Chain) WaitForNBlocks(n int) error {
	var initial int64
	h, err := c.Client.Status()
	if err != nil {
		return err
	}
	if !h.SyncInfo.CatchingUp {
		initial = h.SyncInfo.LatestBlockHeight
	}
	for {
		h, err = c.Client.Status()
		if err != nil {
			return err
		}
		if h.SyncInfo.LatestBlockHeight > initial {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// QueryNextSeqRecv returns the next seqRecv for a configured channel
func (c *Chain) QueryNextSeqRecv(height int64) (recvRes chanTypes.RecvResponse, err error) {
	if !c.PathSet() {
		return recvRes, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Data:   ibctypes.KeyNextSequenceRecv(c.PathEnd.PortID, c.PathEnd.ChannelID),
		Height: height,
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return recvRes, err
	} else if res.Value == nil {
		// TODO: figure out how to return not found error
		return recvRes, nil
	}

	return chanTypes.NewRecvResponse(
		c.PathEnd.PortID,
		c.PathEnd.ChannelID,
		binary.BigEndian.Uint64(res.Value),
		res.Proof,
		res.Height,
	), nil
}

// QueryNextSeqSend returns the next seqSend for a configured channel
func (c *Chain) QueryNextSeqSend(height int64) (uint64, error) {
	if !c.PathSet() {
		return 0, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Data:   ibctypes.KeyNextSequenceSend(c.PathEnd.PortID, c.PathEnd.ChannelID),
		Height: height,
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return 0, err
	} else if res.Value == nil {
		// NOTE: figure out how to return not found error
		return 0, nil
	}

	return binary.BigEndian.Uint64(res.Value), nil
}

// QueryPacketCommitment returns the packet commitment proof at a given height
func (c *Chain) QueryPacketCommitment(height, seq int64) (comRes CommitmentResponse, err error) {
	if !c.PathSet() {
		return comRes, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Data:   ibctypes.KeyPacketCommitment(c.PathEnd.PortID, c.PathEnd.ChannelID, uint64(seq)),
		Height: height,
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return comRes, qPacketCommitmentErr(err)
	} else if res.Value == nil {
		// TODO: Is this the not found error we want to return here?
		return comRes, nil
	}

	return CommitmentResponse{
		Data:  res.Value,
		Proof: commitmenttypes.MerkleProof{Proof: res.Proof},
		ProofPath: commitmenttypes.NewMerklePath(
			strings.Split(
				string(ibctypes.KeyPacketCommitment(c.PathEnd.PortID, c.PathEnd.ChannelID, uint64(seq))),
				"/",
			),
		),
		ProofHeight: uint64(res.Height),
	}, nil
}

func qPacketCommitmentErr(err error) error {
	return fmt.Errorf("query packet commitment failed: %w", err)
}

// CommitmentResponse returns the commiment hash along with the proof data
// NOTE: CommitmentResponse is used to wrap query response from querying PacketCommitment AND PacketAcknowledgement
type CommitmentResponse struct {
	Data        []byte                      `json:"data" yaml:"data"`
	Proof       commitmenttypes.MerkleProof `json:"proof,omitempty" yaml:"proof,omitempty"`
	ProofPath   commitmenttypes.MerklePath  `json:"proof_path,omitempty" yaml:"proof_path,omitempty"`
	ProofHeight uint64                      `json:"proof_height,omitempty" yaml:"proof_height,omitempty"`
}

// QueryPacketAck returns the packet commitment proof at a given height
func (c *Chain) QueryPacketAck(height, seq int64) (comRes CommitmentResponse, err error) {
	if !c.PathSet() {
		return comRes, c.ErrPathNotSet()
	}

	req := abci.RequestQuery{
		Path:   "store/ibc/key",
		Data:   ibctypes.KeyPacketAcknowledgement(c.PathEnd.PortID, c.PathEnd.ChannelID, uint64(seq)),
		Height: height,
		Prove:  true,
	}

	res, err := c.QueryABCI(req)
	if err != nil {
		return comRes, qPacketAckErr(err)
	} else if res.Value == nil {
		return comRes, nil
	}

	return CommitmentResponse{
		Data:  res.Value,
		Proof: commitmenttypes.MerkleProof{Proof: res.Proof},
		ProofPath: commitmenttypes.NewMerklePath(
			strings.Split(
				string(ibctypes.KeyPacketAcknowledgement(c.PathEnd.PortID, c.PathEnd.ChannelID, uint64(seq))),
				"/",
			),
		),
		ProofHeight: uint64(res.Height),
	}, nil
}

func qPacketAckErr(err error) error {
	return fmt.Errorf("query packet acknowledgement failed: %w", err)
}

// QueryTx takes a transaction hash and returns the transaction
func (c *Chain) QueryTx(hashHex string) (sdk.TxResponse, error) {
	hash, err := hex.DecodeString(hashHex)
	if err != nil {
		return sdk.TxResponse{}, err
	}

	resTx, err := c.Client.Tx(hash, true)
	if err != nil {
		return sdk.TxResponse{}, err
	}

	// TODO: validate data coming back with local lite client

	resBlocks, err := c.queryBlocksForTxResults([]*ctypes.ResultTx{resTx})
	if err != nil {
		return sdk.TxResponse{}, err
	}

	out, err := c.formatTxResult(resTx, resBlocks[resTx.Height])
	if err != nil {
		return out, err
	}

	return out, nil
}

// QueryTxs returns an array of transactions given a tag
func (c *Chain) QueryTxs(height uint64, page, limit int, events []string) (*sdk.SearchTxsResult, error) {
	if len(events) == 0 {
		return nil, errors.New("must declare at least one event to search")
	}

	if page <= 0 {
		return nil, errors.New("page must greater than 0")
	}

	if limit <= 0 {
		return nil, errors.New("limit must greater than 0")
	}

	resTxs, err := c.Client.TxSearch(strings.Join(events, " AND "), true, page, limit, "")
	if err != nil {
		return nil, err
	}

	// TODO: Enable lite client validation
	// for _, tx := range resTxs.Txs {
	// 	if err = c.ValidateTxResult(tx); err != nil {
	// 		return nil, err
	// 	}
	// }

	resBlocks, err := c.queryBlocksForTxResults(resTxs.Txs)
	if err != nil {
		return nil, err
	}

	txs, err := c.formatTxResults(resTxs.Txs, resBlocks)
	if err != nil {
		return nil, err
	}

	res := sdk.NewSearchTxsResult(resTxs.TotalCount, len(txs), page, limit, txs)
	return &res, nil
}

// QueryABCI is an affordance for querying the ABCI server associated with a chain
// Similar to cliCtx.QueryABCI
func (c *Chain) QueryABCI(req abci.RequestQuery) (res abci.ResponseQuery, err error) {
	opts := rpcclient.ABCIQueryOptions{
		Height: req.GetHeight(),
		Prove:  req.Prove,
	}

	result, err := c.Client.ABCIQueryWithOptions(req.Path, req.Data, opts)
	if err != nil {
		return res, err
	}

	if !result.Response.IsOK() {
		return res, errors.New(result.Response.Log)
	}

	// data from trusted node or subspace query doesn't need verification
	if !isQueryStoreWithProof(req.Path) {
		return result.Response, nil
	}

	if err = c.VerifyProof(req.Path, result.Response); err != nil {
		return res, err
	}

	return result.Response, nil
}

// QueryWithData satisfies auth.NodeQuerier interface and used for fetching account details
func (c *Chain) QueryWithData(p string, d []byte) (byt []byte, i int64, err error) {
	var res abci.ResponseQuery
	if res, err = c.QueryABCI(abci.RequestQuery{Path: p, Height: 0, Data: d}); err != nil {
		return byt, i, err
	}

	return res.Value, res.Height, nil
}

// QueryLatestHeight queries the chain for the latest height and returns it
func (c *Chain) QueryLatestHeight() (int64, error) {
	res, err := c.Client.Status()
	if err != nil {
		return -1, err
	} else if res.SyncInfo.CatchingUp {
		return -1, fmt.Errorf("node at %s running chain %s not caught up", c.RPCAddr, c.ChainID)
	}

	return res.SyncInfo.LatestBlockHeight, nil
}

type heights struct {
	sync.Mutex
	Map  map[string]int64
	Errs errs
}

type errs []error

func (e errs) err() error {
	var out error
	for _, err := range e {
		out = fmt.Errorf("err: %w ", err)
	}
	return out
}

// QueryLatestHeights returns the heights of multiple chains at once
func QueryLatestHeights(chains ...*Chain) (map[string]int64, error) {
	hs := &heights{Map: make(map[string]int64), Errs: []error{}}
	var wg sync.WaitGroup
	for _, chain := range chains {
		wg.Add(1)
		go func(hs *heights, wg *sync.WaitGroup, chain *Chain) {
			height, err := chain.QueryLatestHeight()

			if err != nil {
				hs.Lock()
				hs.Errs = append(hs.Errs, err)
				hs.Unlock()
			}
			hs.Lock()
			hs.Map[chain.ChainID] = height
			hs.Unlock()
			wg.Done()
		}(hs, &wg, chain)
	}
	wg.Wait()
	return hs.Map, hs.Errs.err()
}

// QueryLatestHeader returns the latest header from the chain
func (c *Chain) QueryLatestHeader() (out *tmclient.Header, err error) {
	var h int64
	if h, err = c.QueryLatestHeight(); err != nil {
		return nil, err
	}
	if out, err = c.QueryHeaderAtHeight(h); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryHeaderAtHeight returns the header at a given height
func (c *Chain) QueryHeaderAtHeight(height int64) (*tmclient.Header, error) {
	if height <= 0 {
		return nil, fmt.Errorf("must pass in valid height, %d not valid", height)
	}

	res, err := c.Client.Commit(&height)
	if err != nil {
		return nil, err
	}

	val, err := c.Client.Validators(&height, 0, 10000)
	if err != nil {
		return nil, err
	}

	return &tmclient.Header{
		// NOTE: This is not a SignedHeader
		// We are missing a lite.Commit type here
		SignedHeader: res.SignedHeader,
		ValidatorSet: tmtypes.NewValidatorSet(val.Validators),
	}, nil
}

// isQueryStoreWithProof expects a format like /<queryType>/<storeName>/<subpath>
// queryType must be "store" and subpath must be "key" to require a proof.
func isQueryStoreWithProof(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}

	paths := strings.SplitN(path[1:], "/", 3)
	switch {
	case len(paths) != 3:
		return false
	case paths[0] != "store":
		return false
	case rootmulti.RequireProof("/" + paths[2]):
		return true
	}

	return false
}

// queryBlocksForTxResults returns a map[blockHeight]txResult
func (c *Chain) queryBlocksForTxResults(resTxs []*ctypes.ResultTx) (map[int64]*ctypes.ResultBlock, error) {
	resBlocks := make(map[int64]*ctypes.ResultBlock)
	for _, resTx := range resTxs {
		if _, ok := resBlocks[resTx.Height]; !ok {
			resBlock, err := c.Client.Block(&resTx.Height)
			if err != nil {
				return nil, err
			}
			resBlocks[resTx.Height] = resBlock
		}
	}
	return resBlocks, nil
}

// formatTxResults parses the indexed txs into a slice of TxResponse objects.
func (c *Chain) formatTxResults(resTxs []*ctypes.ResultTx, resBlocks map[int64]*ctypes.ResultBlock) ([]sdk.TxResponse, error) {
	var err error
	out := make([]sdk.TxResponse, len(resTxs))
	for i := range resTxs {
		out[i], err = c.formatTxResult(resTxs[i], resBlocks[resTxs[i].Height])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// formatTxResult parses a tx into a TxResponse object
func (c *Chain) formatTxResult(resTx *ctypes.ResultTx, resBlock *ctypes.ResultBlock) (sdk.TxResponse, error) {
	tx, err := parseTx(c.Amino, resTx.Tx)
	if err != nil {
		return sdk.TxResponse{}, err
	}
	res := sdk.NewResponseResultTx(resTx, tx, resBlock.Block.Time.Format(time.RFC3339))
	if !c.debug {
		res.RawLog = ""
	}
	return res, nil
}

// Takes some bytes and a codec and returns an sdk.Tx
func parseTx(cdc *codec.Codec, txBytes []byte) (sdk.Tx, error) {
	var tx authTypes.StdTx

	err := cdc.UnmarshalBinaryLengthPrefixed(txBytes, &tx)
	if err != nil {
		return nil, err
	}

	return tx, nil
}

func ibcQuerierRoute(module, path string) string {
	return fmt.Sprintf("custom/%s/%s/%s", ibctypes.QuerierRoute, module, path)
}
