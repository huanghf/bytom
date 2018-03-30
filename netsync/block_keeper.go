package netsync

import (
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/errors"
	"github.com/bytom/p2p"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/types"
)

const (
	maxKnownTxs    = 32768 // Maximum transactions hashes to keep in the known list (prevent DOS)
	maxKnownBlocks = 1024  // Maximum block hashes to keep in the known list (prevent DOS)

	syncTimeout        = 30 * time.Second
	requestRetryTicker = 15 * time.Second

	maxBlocksPending = 1024
	maxtxsPending    = 32768
)

type BlockRequestMessage struct {
	Height  uint64
	RawHash [32]byte
}

type pendingResponse struct {
	block *types.Block
	src   *p2p.Peer
}

type txsNotify struct {
	tx  *types.Tx
	src *p2p.Peer
}

//TODO: add retry mechanism
type blockKeeper struct {
	chain *protocol.Chain
	sw    *p2p.Switch
	peers map[string]*blockKeeperPeer

	pendingProcessCh chan *pendingResponse
	txsProcessCh     chan *txsNotify
	quitReqBlockCh   chan *string

	mtx sync.RWMutex
}

func newBlockKeeper(chain *protocol.Chain, sw *p2p.Switch) *blockKeeper {
	bk := &blockKeeper{
		chain:            chain,
		sw:               sw,
		peers:            make(map[string]*blockKeeperPeer),
		pendingProcessCh: make(chan *pendingResponse, maxBlocksPending),
		txsProcessCh:     make(chan *txsNotify, maxtxsPending),
		quitReqBlockCh:   make(chan *string),
	}
	go bk.blockProcessWorker()
	go bk.txsProcessWorker()
	return bk
}

func (bk *blockKeeper) AddBlock(block *types.Block, src *p2p.Peer) {
	bk.pendingProcessCh <- &pendingResponse{block: block, src: src}
}

func (bk *blockKeeper) AddTX(tx *types.Tx, src *p2p.Peer) {
	bk.txsProcessCh <- &txsNotify{tx: tx, src: src}
}

func (bk *blockKeeper) IsCaughtUp() bool {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()
	_, height := bk.BestPeer()
	return bk.chain.Height() < height
}

func (bk *blockKeeper) RemovePeer(peerID string) {
	bk.mtx.Lock()
	delete(bk.peers, peerID)
	bk.mtx.Unlock()
	log.WithField("ID", peerID).Info("Delete peer from blockKeeper")
	bk.quitReqBlockCh <- &peerID
}

func (bk *blockKeeper) AddPeer(peer *p2p.Peer) {
	bk.mtx.Lock()
	defer bk.mtx.Unlock()

	if tmp := bk.peers[peer.Key]; tmp == nil {
		keeperPeer := newBlockKeeperPeer(0, nil)
		bk.peers[peer.Key] = keeperPeer
		bk.peers[peer.Key].peer = peer
		log.WithFields(log.Fields{"ID": peer.Key}).Info("Add new peer to blockKeeper")
		return
	}
	log.WithField("ID", peer.Key).Warning("Add existing peer to blockKeeper")
}

func (bk *blockKeeper) SetPeerHeight(peerID string, height uint64, hash *bc.Hash) {
	bk.mtx.Lock()
	defer bk.mtx.Unlock()

	if peer, ok := bk.peers[peerID]; ok {
		peer.SetStatus(height, hash)
	}
}

func (bk *blockKeeper) BestPeer() (*p2p.Peer, uint64) {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()

	var bestPeer *p2p.Peer
	var bestHeight uint64

	for _, p := range bk.peers {
		if bestPeer == nil || p.height > bestHeight {
			bestPeer, bestHeight = p.peer, p.height
		}
	}

	return bestPeer, bestHeight
}

// MarkTransaction marks a transaction as known for the peer, ensuring that it
// will never be propagated to this particular peer.
func (bk *blockKeeper) MarkTransaction(peerID string, hash [32]byte) error {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()

	bkPeer := bk.peers[peerID]
	if bkPeer == nil {
		return errors.New("Can't find block keeper peer.")
	}

	bkPeer.MarkTransaction(hash)
	return nil
}

// MarkBlock marks a block as known for the peer, ensuring that the block will
// never be propagated to this particular peer.
func (bk *blockKeeper) MarkBlock(peerID string, hash [32]byte) error {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()

	bkPeer := bk.peers[peerID]
	if bkPeer == nil {
		return errors.New("Can't find block keeper peer.")
	}

	bkPeer.MarkBlock(hash)
	return nil
}

// PeersWithoutTx retrieves a list of peers that do not have a given transaction
// in their set of known hashes.
func (bk *blockKeeper) PeersWithoutTx(hash [32]byte) []*p2p.Peer {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()

	list := make([]*p2p.Peer, 0, len(bk.peers))
	for _, p := range bk.peers {
		if !p.knownTxs.Has(hash) {
			list = append(list, p.peer)
		}
	}
	return list
}

// PeersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes.
func (bk *blockKeeper) PeersWithoutBlock(hash [32]byte) []*p2p.Peer {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()

	list := make([]*p2p.Peer, 0, len(bk.peers))
	for _, p := range bk.peers {
		if !p.knownBlocks.Has(hash) {
			list = append(list, p.peer)
		}
	}
	return list
}

func (bk *blockKeeper) requestBlockByHash(peerID string, hash *bc.Hash) error {
	bkPeer := bk.peers[peerID]
	if bkPeer == nil {
		return errors.New("Can't find block keeper peer.")
	}

	if err := bkPeer.requestBlockByHash(hash); err != nil {
		return err
	}

	return nil
}

func (bk *blockKeeper) requestBlockByHeight(peerID string, height uint64) error {
	bkPeer := bk.peers[peerID]
	if bkPeer == nil {
		return errors.New("Can't find block keeper peer.")
	}

	if err := bkPeer.requestBlockByHeight(height); err != nil {
		return err
	}

	return nil
}

func (bk *blockKeeper) BlockRequestWorker(peer *p2p.Peer, maxPeerHeight uint64) {
	chainHeight := bk.chain.Height()

	for i := chainHeight + 1; i <= maxPeerHeight; i++ {
		bk.requestBlockByHeight(peer.Key, i)
		waiter := bk.chain.BlockWaiter(i)
		retryTicker := time.Tick(requestRetryTicker)
		syncWait := time.NewTimer(syncTimeout)

	retryLoop:
		for {
			select {
			case <-waiter:
				break retryLoop
			case <-retryTicker:
				bk.requestBlockByHeight(peer.Key, i)
			case <-syncWait.C:
				log.Info("Request block timeout")
				return
			case peerid := <-bk.quitReqBlockCh:
				if strings.Compare(*peerid, peer.Key) == 0 {
					log.Info("Quite block request worker")
					return
				}
			}
		}
	}
}

func (bk *blockKeeper) blockProcessWorker() {
	for pendingResponse := range bk.pendingProcessCh {

		block := pendingResponse.block
		blockHash := block.Hash()
		isOrphan, err := bk.chain.ProcessBlock(block)
		if err != nil {
			bk.sw.AddScamPeer(pendingResponse.src)
			log.WithField("hash", blockHash.String()).Errorf("blockKeeper fail process block %v", err)
			continue
		}
		log.WithFields(log.Fields{
			"height":   block.Height,
			"hash":     blockHash.String(),
			"isOrphan": isOrphan,
		}).Info("blockKeeper processed block")

		if isOrphan {
			bk.requestBlockByHash(pendingResponse.src.Key, &block.PreviousBlockHash)
		}
	}
}

func (bk *blockKeeper) txsProcessWorker() {
	for txsResponse := range bk.txsProcessCh {
		tx:=txsResponse.tx
		bk.MarkTransaction(txsResponse.src.Key, tx.ID.Byte32())
		if err := bk.chain.ValidateTx(tx); err != nil {
			bk.sw.AddScamPeer(txsResponse.src)
		}
	}
}