package netsync

import (
	"reflect"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	cmn "github.com/tendermint/tmlibs/common"

	"github.com/bytom/errors"
	"github.com/bytom/netsync/fetcher"
	"github.com/bytom/p2p"
	"github.com/bytom/p2p/trust"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/types"
)

const (
	// BlockchainChannel is a channel for blocks and status updates
	BlockchainChannel         = byte(0x40)
	protocolHandshakeTimeout  = time.Second * 10
)

var (
	ErrProtocalHandshakeTimeout = errors.New("Protocal handshake timeout")
)


// Response describes the response standard.
type Response struct {
	Status string      `json:"status,omitempty"`
	Msg    string      `json:"msg,omitempty"`
	Data   interface{} `json:"data,omitempty"`
}

type initalPeerStatus struct {
	peerID string
	height uint64
	hash   *bc.Hash
}

//ProtocalReactor handles long-term catchup syncing.
type ProtocalReactor struct {
	p2p.BaseReactor

	chain       *protocol.Chain
	blockKeeper *blockKeeper
	txPool      *protocol.TxPool
	sw          *p2p.Switch
	fetcher     *fetcher.Fetcher

	newPeerCh    chan struct{}
	peerStatusCh chan *initalPeerStatus
}

// NewProtocalReactor returns the reactor of whole blockchain.
func NewProtocalReactor(chain *protocol.Chain, txPool *protocol.TxPool, sw *p2p.Switch, fetcher *fetcher.Fetcher) *ProtocalReactor {
	pr := &ProtocalReactor{
		chain:        chain,
		blockKeeper:  newBlockKeeper(chain, sw),
		txPool:       txPool,
		sw:           sw,
		fetcher:      fetcher,
		newPeerCh:    make(chan struct{}),
		peerStatusCh: make(chan *initalPeerStatus),
	}
	pr.BaseReactor = *p2p.NewBaseReactor("ProtocalReactor", pr)
	return pr
}

// GetChannels implements Reactor
func (pr *ProtocalReactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		&p2p.ChannelDescriptor{
			ID:                BlockchainChannel,
			Priority:          5,
			SendQueueCapacity: 100,
		},
	}
}

// GetChannels implements Reactor
func (pr *ProtocalReactor) GetNewPeerChan() chan struct{} {
	return pr.newPeerCh
}

// OnStart implements BaseService
func (pr *ProtocalReactor) OnStart() error {
	pr.BaseReactor.OnStart()
	return nil
}

// OnStop implements BaseService
func (pr *ProtocalReactor) OnStop() {
	pr.BaseReactor.OnStop()
}

// AddPeer implements Reactor by sending our state to peer.
func (pr *ProtocalReactor) AddPeer(peer *p2p.Peer) error {
	peer.Send(BlockchainChannel, struct{ BlockchainMessage }{&StatusRequestMessage{}})
	handshakeWait := time.NewTimer(protocolHandshakeTimeout)
	for {
		select {
		case status := <-pr.peerStatusCh:
			if strings.Compare(status.peerID, peer.Key) == 0 {
				pr.blockKeeper.AddPeer(peer)
				pr.blockKeeper.SetPeerHeight(status.peerID, status.height, status.hash)
				pr.newPeerCh <- struct{}{}
				return nil
			}
		case <-handshakeWait.C:
			return ErrProtocalHandshakeTimeout
		}
	}
}

// RemovePeer implements Reactor by removing peer from the pool.
func (pr *ProtocalReactor) RemovePeer(peer *p2p.Peer, reason interface{}) {
	pr.blockKeeper.RemovePeer(peer.Key)
}

// Receive implements Reactor by handling 4 types of messages (look below).
func (pr *ProtocalReactor) Receive(chID byte, src *p2p.Peer, msgBytes []byte) {
	var tm *trust.TrustMetric
	key := src.Connection().RemoteAddress.IP.String()
	if tm = pr.sw.TrustMetricStore.GetPeerTrustMetric(key); tm == nil {
		log.Errorf("Can't get peer trust metric")
		return
	}

	_, msg, err := DecodeMessage(msgBytes)
	if err != nil {
		log.Errorf("Error decoding messagek %v", err)
		return
	}
	log.WithFields(log.Fields{"peerID": src.Key, "msg": msg}).Info("Receive request")

	switch msg := msg.(type) {
	case *BlockRequestMessage:
		var block *types.Block
		var err error
		if msg.Height != 0 {
			block, err = pr.chain.GetBlockByHeight(msg.Height)
		} else {
			block, err = pr.chain.GetBlockByHash(msg.GetHash())
		}
		if err != nil {
			log.Errorf("Fail on BlockRequestMessage get block: %v", err)
			return
		}
		response, err := NewBlockResponseMessage(block)
		if err != nil {
			log.Errorf("Fail on BlockRequestMessage create resoinse: %v", err)
			return
		}
		src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{response})

	case *BlockResponseMessage:
		log.Info("BlockResponseMessage height:", msg.GetBlock().Height)
		pr.blockKeeper.AddBlock(msg.GetBlock(), src)

	case *StatusRequestMessage:
		block := pr.chain.BestBlock()
		src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{NewStatusResponseMessage(block)})

	case *StatusResponseMessage:
		peerStatus := &initalPeerStatus{
			peerID: src.Key,
			height: msg.Height,
			hash:   msg.GetHash(),
		}
		pr.peerStatusCh <- peerStatus

	case *TransactionNotifyMessage:
		tx, err := msg.GetTransaction()
		if err != nil {
			log.Errorf("Error decoding new tx %v", err)
			return
		}
		pr.blockKeeper.AddTX(tx,src)

	case *MineBlockMessage:
		block, err := msg.GetMineBlock()
		if err != nil {
			log.Errorf("Error decoding mined block %v", err)
			return
		}
		// Mark the peer as owning the block and schedule it for import
		pr.blockKeeper.MarkBlock(src.Key, block.Hash().Byte32())
		pr.fetcher.Enqueue(src.Key, block)
		hash := block.Hash()
		pr.blockKeeper.peers[src.Key].SetStatus(block.Height, &hash)

	default:
		log.Error(cmn.Fmt("Unknown message type %v", reflect.TypeOf(msg)))
	}
}
