package blockpool

import (
	"math/big"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/errs"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
)

// the blockpool's model of a peer
type peer struct {
	lock sync.RWMutex

	// last known blockchain status
	td               *big.Int
	tdAdvertised     bool
	currentBlockHash common.Hash
	currentBlock     *types.Block
	parentHash       common.Hash
	headSection      *section

	id string

	// peer callbacks
	requestBlockHashes func(common.Hash) error
	requestBlocks      func([]common.Hash) error
	peerError          func(*errs.Error)
	errors             *errs.Errors

	sections []common.Hash

	// channels to push new head block and head section for peer a
	currentBlockC chan *types.Block
	headSectionC  chan *section

	// channels to signal peer switch and peer quit to section processes
	idleC   chan bool
	switchC chan bool

	bp *BlockPool

	// timers for head section process
	blockHashesRequestTimer <-chan time.Time
	blocksRequestTimer      <-chan time.Time
	headInfoTimer           <-chan time.Time
	bestIdleTimer           <-chan time.Time

	addToBlacklist func(id string)

	idle bool
}

// peers is the component keeping a record of peers in a hashmap
//
type peers struct {
	lock   sync.RWMutex
	bllock sync.Mutex

	bp        *BlockPool
	errors    *errs.Errors
	peers     map[string]*peer
	best      *peer
	status    *status
	blacklist map[string]time.Time
}

// peer constructor
func (self *peers) newPeer(
	td *big.Int,
	currentBlockHash common.Hash,
	id string,
	requestBlockHashes func(common.Hash) error,
	requestBlocks func([]common.Hash) error,
	peerError func(*errs.Error),
) (p *peer) {

	p = &peer{
		errors:             self.errors,
		td:                 td,
		currentBlockHash:   currentBlockHash,
		id:                 id,
		requestBlockHashes: requestBlockHashes,
		requestBlocks:      requestBlocks,
		peerError:          peerError,
		currentBlockC:      make(chan *types.Block),
		headSectionC:       make(chan *section),
		switchC:            make(chan bool),
		bp:                 self.bp,
		idle:               true,
		addToBlacklist:     self.addToBlacklist,
	}
	close(p.switchC) //! hack :((((
	// at creation the peer is recorded in the peer pool
	self.peers[id] = p
	return
}

// dispatches an error to a peer if still connected, adds it to the blacklist
func (self *peers) peerError(id string, code int, format string, params ...interface{}) {
	self.lock.RLock()
	peer, ok := self.peers[id]
	self.lock.RUnlock()
	if ok {
		peer.addError(code, format, params...)
	} else {
		self.addToBlacklist(id)
	}
}

// record time of offence in blacklist to implement suspension for PeerSuspensionInterval
func (self *peers) addToBlacklist(id string) {
	self.bllock.Lock()
	defer self.bllock.Unlock()
	self.blacklist[id] = time.Now()
}

// suspended checks if peer is still suspended, caller should hold peers.lock
func (self *peers) suspended(id string) (s bool) {
	self.bllock.Lock()
	defer self.bllock.Unlock()
	if suspendedAt, ok := self.blacklist[id]; ok {
		if s = suspendedAt.Add(self.bp.Config.PeerSuspensionInterval).After(time.Now()); !s {
			// no longer suspended, delete entry
			delete(self.blacklist, id)
		}
	}
	return
}

func (self *peer) addError(code int, format string, params ...interface{}) {
	err := self.errors.New(code, format, params...)
	self.peerError(err)
	if err.Fatal() {
		self.addToBlacklist(self.id)
	} else {
		go self.bp.peers.removePeer(self.id, false)
	}
}

// caller must hold peer lock
func (self *peer) setChainInfo(td *big.Int, currentBlockHash common.Hash) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.currentBlockHash != currentBlockHash {
		previousBlockHash := self.currentBlockHash
		glog.V(logger.Debug).Infof("addPeer: Update peer <%s> with td %v (was %v) and current block %s (was %v)", self.id, td, self.td, hex(currentBlockHash), hex(previousBlockHash))

		self.td = td
		self.currentBlockHash = currentBlockHash
		self.currentBlock = nil
		self.parentHash = common.Hash{}
		self.headSection = nil
	}
	self.tdAdvertised = true
}

func (self *peer) setChainInfoFromBlock(block *types.Block) (td *big.Int, currentBlockHash common.Hash) {
	hash := block.Hash()
	// this happens when block came in a newblock message but
	// also if sent in a blockmsg (for instance, if we requested, only if we
	// dont apply on blockrequests the restriction of flood control)
	currentBlockHash = self.currentBlockHash
	if currentBlockHash == hash {
		if self.currentBlock == nil {
			// signal to head section process
			glog.V(logger.Detail).Infof("AddBlock: head block %s for peer <%s> (head: %s) received\n", hex(hash), self.id, hex(currentBlockHash))
			td = self.td
		} else {
			glog.V(logger.Detail).Infof("AddBlock: head block %s for peer <%s> (head: %s) already known", hex(hash), self.id, hex(currentBlockHash))
		}
	}
	return
}

// this will use the TD given by the first peer to update peer td, this helps second best peer selection
func (self *peer) setChainInfoFromNode(n *node) {
	// in case best peer is lost
	block := n.block
	hash := block.Hash()
	if n.td != nil && n.td.Cmp(self.td) > 0 {
		glog.V(logger.Detail).Infof("AddBlock: update peer <%s> - head: %v->%v - TD: %v->%v", self.id, hex(self.currentBlockHash), hex(hash), self.td, n.td)
		self.td = n.td
		self.currentBlockHash = block.Hash()
		self.parentHash = block.ParentHash()
		self.currentBlock = block
		self.headSection = nil
	}
}

// distribute block request among known peers
func (self *peers) requestBlocks(attempts int, hashes []common.Hash) {
	self.lock.RLock()

	defer self.lock.RUnlock()
	peerCount := len(self.peers)
	// on first attempt use the best peer
	if attempts == 0 && self.best != nil {
		glog.V(logger.Detail).Infof("request %v missing blocks from best peer <%s>", len(hashes), self.best.id)
		self.best.requestBlocks(hashes)
		return
	}
	repetitions := self.bp.Config.BlocksRequestRepetition
	if repetitions > peerCount {
		repetitions = peerCount
	}
	i := 0
	indexes := rand.Perm(peerCount)[0:repetitions]
	sort.Ints(indexes)

	glog.V(logger.Detail).Infof("request %v missing blocks from %v/%v peers", len(hashes), repetitions, peerCount)
	for _, peer := range self.peers {
		if i == indexes[0] {
			glog.V(logger.Detail).Infof("request length: %v", len(hashes))
			glog.V(logger.Detail).Infof("request %v missing blocks [%x/%x] from peer <%s>", len(hashes), hashes[0][:4], hashes[len(hashes)-1][:4], peer.id)
			peer.requestBlocks(hashes)
			indexes = indexes[1:]
			if len(indexes) == 0 {
				break
			}
		}
		i++
	}
	self.bp.putHashSlice(hashes)
}

// addPeer implements the logic for blockpool.AddPeer
// returns 2 bool values
// 1. true iff peer is promoted as best peer in the pool
// 2. true iff peer is still suspended
func (self *peers) addPeer(
	td *big.Int,
	currentBlockHash common.Hash,
	id string,
	requestBlockHashes func(common.Hash) error,
	requestBlocks func([]common.Hash) error,
	peerError func(*errs.Error),
) (best bool, suspended bool) {

	self.lock.Lock()
	defer self.lock.Unlock()
	var previousBlockHash common.Hash
	if self.suspended(id) {
		suspended = true
		return
	}
	p, found := self.peers[id]
	if found {
		// when called on an already connected peer, it means a newBlockMsg is received
		// peer head info is updated
		p.setChainInfo(td, currentBlockHash)
		self.status.lock.Lock()
		self.status.values.NewBlocks++
		self.status.lock.Unlock()
	} else {
		p = self.newPeer(td, currentBlockHash, id, requestBlockHashes, requestBlocks, peerError)

		self.status.lock.Lock()

		self.status.peers[id]++
		self.status.values.NewBlocks++
		self.status.lock.Unlock()

		glog.V(logger.Debug).Infof("addPeer: add new peer <%v> with td %v and current block %s", id, td, hex(currentBlockHash))
	}

	// check if peer's current head block is known
	if self.bp.hasBlock(currentBlockHash) {
		// peer not ahead
		glog.V(logger.Debug).Infof("addPeer: peer <%v> with td %v and current block %s is behind", id, td, hex(currentBlockHash))
		return false, false
	}

	if self.best == p {
		// new block update for active current best peer -> request hashes
		glog.V(logger.Debug).Infof("addPeer: <%s> already the best peer. Request new head section info from %s", id, hex(currentBlockHash))

		if (previousBlockHash != common.Hash{}) {
			glog.V(logger.Detail).Infof("addPeer: <%s> head changed: %s -> %s ", id, hex(previousBlockHash), hex(currentBlockHash))
			p.headSectionC <- nil
			if entry := self.bp.get(previousBlockHash); entry != nil {
				glog.V(logger.Detail).Infof("addPeer: <%s> previous head : %v found in pool, activate", id, hex(previousBlockHash))
				self.bp.activateChain(entry.section, p, p.switchC, nil)
				p.sections = append(p.sections, previousBlockHash)
			}
		}
		best = true
	} else {
		// baseline is our own TD
		currentTD := self.bp.getTD()
		bestpeer := self.best
		if bestpeer != nil {
			bestpeer.lock.RLock()
			defer bestpeer.lock.RUnlock()
			currentTD = self.best.td
		}
		if td.Cmp(currentTD) > 0 {
			self.status.lock.Lock()
			self.status.bestPeers[p.id]++
			self.status.lock.Unlock()
			glog.V(logger.Debug).Infof("addPeer: peer <%v> (td: %v > current td %v) promoted best peer", id, td, currentTD)
			// fmt.Printf("best peer %v - \n", bestpeer, id)
			self.bp.switchPeer(bestpeer, p)
			self.best = p
			best = true
		}
	}

	return
}

// removePeer is called (via RemovePeer) by the eth protocol when the peer disconnects
func (self *peers) removePeer(id string, del bool) {
	self.lock.Lock()
	defer self.lock.Unlock()

	p, found := self.peers[id]
	if !found {
		return
	}
	p.lock.Lock()
	defer p.lock.Unlock()

	if del {
		delete(self.peers, id)
		glog.V(logger.Debug).Infof("addPeer: remove peer <%v> (td: %v)", id, p.td)
	}
	// if current best peer is removed, need to find a better one
	if self.best == p {
		var newp *peer
		// only peers that are ahead of us are considered
		max := self.bp.getTD()
		// peer with the highest self-acclaimed TD is chosen
		for _, pp := range self.peers {
			// demoted peer's td should be 0
			if pp.id == id {
				pp.td = common.Big0
				pp.currentBlockHash = common.Hash{}
				continue
			}
			pp.lock.RLock()
			if pp.td.Cmp(max) > 0 {
				max = pp.td
				newp = pp
			}
			pp.lock.RUnlock()
		}
		if newp != nil {
			self.status.lock.Lock()
			self.status.bestPeers[p.id]++
			self.status.lock.Unlock()
			glog.V(logger.Debug).Infof("addPeer: peer <%v> (td: %v) promoted best peer", newp.id, newp.td)
		} else {
			glog.V(logger.Warn).Infof("addPeer: no suitable peers found")
		}
		self.best = newp
		// fmt.Printf("remove peer %v - %v\n", p.id, newp)
		self.bp.switchPeer(p, newp)
	}
}

// switchPeer launches section processes
func (self *BlockPool) switchPeer(oldp, newp *peer) {

	// first quit AddBlockHashes, requestHeadSection and activateChain
	// by closing the old peer's switchC channel
	if oldp != nil {
		glog.V(logger.Detail).Infof("<%s> quit peer processes", oldp.id)
		// fmt.Printf("close %v - %v\n", oldp.id, newp)
		close(oldp.switchC)
	}
	if newp != nil {
		// if new best peer has no head section yet, create it and run it
		// otherwise head section is an element of peer.sections
		newp.idleC = make(chan bool)
		newp.switchC = make(chan bool)
		if newp.headSection == nil {
			glog.V(logger.Detail).Infof("[%s] head section for [%s] not created, requesting info", newp.id, hex(newp.currentBlockHash))

			if newp.idle {
				self.wg.Add(1)
				newp.idle = false
				self.syncing()
			}

			go func() {
				newp.run()
				if !newp.idle {
					self.wg.Done()
					newp.idle = true
				}
			}()

		}

		var connected = make(map[common.Hash]*section)
		var sections []common.Hash
		for _, hash := range newp.sections {
			glog.V(logger.Detail).Infof("activate chain starting from section [%s]", hex(hash))
			// if section not connected (ie, top of a contiguous sequence of sections)
			if connected[hash] == nil {
				// if not deleted, then reread from pool (it can be orphaned top half of a split section)
				if entry := self.get(hash); entry != nil {
					self.activateChain(entry.section, newp, newp.switchC, connected)
					connected[hash] = entry.section
					sections = append(sections, hash)
				}
			}
		}
		glog.V(logger.Detail).Infof("<%s> section processes (%v non-contiguous sequences, was %v before)", newp.id, len(sections), len(newp.sections))
		// need to lock now that newp is exposed to section processesr
		newp.lock.Lock()
		newp.sections = sections
		newp.lock.Unlock()
	}
	// finally deactivate section process for sections where newp didnt activate
	// newp activating section process changes the quit channel for this reason
	if oldp != nil {
		glog.V(logger.Detail).Infof("<%s> quit section processes", oldp.id)
		close(oldp.idleC)
	}
}

// getPeer looks up peer by id, returns peer and a bool value
// that is true iff peer is current best peer
func (self *peers) getPeer(id string) (p *peer, best bool) {
	self.lock.RLock()
	defer self.lock.RUnlock()
	if self.best != nil && self.best.id == id {
		return self.best, true
	}
	p = self.peers[id]
	return
}

// head section process

func (self *peer) handleSection(sec *section) {
	self.lock.Lock()
	defer self.lock.Unlock()
	glog.V(logger.Detail).Infof("HeadSection: <%s> (head: %s) head section received [%s]-[%s]", self.id, hex(self.currentBlockHash), sectionhex(self.headSection), sectionhex(sec))

	self.headSection = sec
	self.blockHashesRequestTimer = nil

	if sec == nil {
		if self.idle {
			self.idle = false
			self.bp.wg.Add(1)
			self.bp.syncing()
		}

		self.headInfoTimer = time.After(self.bp.Config.BlockHashesTimeout)
		self.bestIdleTimer = nil

		glog.V(logger.Detail).Infof("HeadSection: <%s> head block hash changed (mined block received). New head %s", self.id, hex(self.currentBlockHash))
	} else {
		if !self.idle {
			self.idle = true
			self.bp.wg.Done()
		}

		self.headInfoTimer = nil
		self.bestIdleTimer = time.After(self.bp.Config.IdleBestPeerTimeout)
		glog.V(logger.Detail).Infof("HeadSection: <%s> (head: %s) head section [%s] created. Idle...", self.id, hex(self.currentBlockHash), sectionhex(sec))
	}
}

func (self *peer) getCurrentBlock(currentBlock *types.Block) {
	// called by update or after AddBlock signals that head block of current peer is received
	self.lock.Lock()
	defer self.lock.Unlock()
	if currentBlock == nil {
		if entry := self.bp.get(self.currentBlockHash); entry != nil {
			entry.node.lock.Lock()
			currentBlock = entry.node.block
			entry.node.lock.Unlock()
		}
		if currentBlock != nil {
			glog.V(logger.Detail).Infof("HeadSection: <%s> head block %s found in blockpool", self.id, hex(self.currentBlockHash))
		} else {
			glog.V(logger.Detail).Infof("HeadSection: <%s> head block %s not found... requesting it", self.id, hex(self.currentBlockHash))
			self.requestBlocks([]common.Hash{self.currentBlockHash})
			self.blocksRequestTimer = time.After(self.bp.Config.BlocksRequestInterval)
			return
		}
	} else {
		glog.V(logger.Detail).Infof("HeadSection: <%s> head block %s received (parent: %s)", self.id, hex(self.currentBlockHash), hex(currentBlock.ParentHash()))
	}

	self.currentBlock = currentBlock
	self.parentHash = currentBlock.ParentHash()
	glog.V(logger.Detail).Infof("HeadSection: <%s> head block %s found (parent: %s)... requesting  hashes", self.id, hex(self.currentBlockHash), hex(self.parentHash))
	self.blockHashesRequestTimer = time.After(0)
	self.blocksRequestTimer = nil
}

func (self *peer) getBlockHashes() bool {
	self.lock.Lock()
	defer self.lock.Unlock()
	//if connecting parent is found
	if self.bp.hasBlock(self.parentHash) {
		glog.V(logger.Detail).Infof("HeadSection: <%s> parent block %s found in blockchain", self.id, hex(self.parentHash))
		err := self.bp.insertChain(types.Blocks([]*types.Block{self.currentBlock}))

		self.bp.status.lock.Lock()
		self.bp.status.values.BlocksInChain++
		self.bp.status.values.BlocksInPool--
		if err != nil {
			self.addError(ErrInvalidBlock, "%v", err)
			self.bp.status.badPeers[self.id]++
		} else {
			// XXX added currentBlock check (?)
			if self.currentBlock != nil && self.currentBlock.Td != nil && !self.currentBlock.Queued() {
				glog.V(logger.Detail).Infof("HeadSection: <%s> inserted %s to blockchain... check TD %v =?= %v", self.id, hex(self.parentHash), self.td, self.currentBlock.Td)
				if self.td.Cmp(self.currentBlock.Td) != 0 {
					self.addError(ErrIncorrectTD, "on block %x %v =?= %v", hex(self.parentHash), self.td, self.currentBlock.Td)
					self.bp.status.badPeers[self.id]++
				}
			}

			headKey := self.parentHash
			height := self.bp.status.chain[headKey] + 1
			self.bp.status.chain[self.currentBlockHash] = height
			if height > self.bp.status.values.LongestChain {
				self.bp.status.values.LongestChain = height
			}
			delete(self.bp.status.chain, headKey)
		}
		self.bp.status.lock.Unlock()
	} else {
		if parent := self.bp.get(self.parentHash); parent != nil {
			if self.bp.get(self.currentBlockHash) == nil {
				glog.V(logger.Detail).Infof("HeadSection: <%s> connecting parent %s found in pool... creating singleton section", self.id, hex(self.parentHash))
				self.bp.nodeCacheLock.Lock()
				n, ok := self.bp.nodeCache[self.currentBlockHash]
				if !ok {
					panic("not found in nodeCache")
				}
				self.bp.nodeCacheLock.Unlock()
				self.bp.newSection([]*node{n}).activate(self)
			} else {
				glog.V(logger.Detail).Infof("HeadSection: <%s> connecting parent %s found in pool...head section [%s] exists...not requesting hashes", self.id, hex(self.parentHash), sectionhex(parent.section))
				self.bp.activateChain(parent.section, self, self.switchC, nil)
			}
		} else {
			glog.V(logger.Detail).Infof("HeadSection: <%s> section [%s] requestBlockHashes", self.id, sectionhex(self.headSection))
			self.requestBlockHashes(self.currentBlockHash)
			self.blockHashesRequestTimer = time.After(self.bp.Config.BlockHashesRequestInterval)
			return false
		}
	}
	self.blockHashesRequestTimer = nil
	if !self.idle {
		self.idle = true
		self.headInfoTimer = nil
		self.bestIdleTimer = time.After(self.bp.Config.IdleBestPeerTimeout)
		self.bp.wg.Done()
	}
	return true
}

// main loop for head section process
func (self *peer) run() {

	self.blocksRequestTimer = time.After(0)
	self.headInfoTimer = time.After(self.bp.Config.BlockHashesTimeout)
	self.bestIdleTimer = nil

	var ping = time.NewTicker(5 * time.Second)

LOOP:
	for {
		select {
		// to minitor section process behaviour
		case <-ping.C:
			glog.V(logger.Detail).Infof("HeadSection: <%s> section with head %s, idle: %v", self.id, hex(self.currentBlockHash), self.idle)

		// signal from AddBlockHashes that head section for current best peer is created
		// if sec == nil, it signals that chain info has updated (new block message)
		case sec := <-self.headSectionC:
			self.handleSection(sec)

		// periodic check for block hashes or parent block/section
		case <-self.blockHashesRequestTimer:
			self.getBlockHashes()

		// signal from AddBlock that head block of current best peer has been received
		case currentBlock := <-self.currentBlockC:
			self.getCurrentBlock(currentBlock)

		// keep requesting until found or timed out
		case <-self.blocksRequestTimer:
			self.getCurrentBlock(nil)

		// quitting on timeout
		case <-self.headInfoTimer:
			self.peerError(self.bp.peers.errors.New(ErrInsufficientChainInfo, "timed out without providing block hashes or head block (td: %v, head: %s)", self.td, hex(self.currentBlockHash)))

			self.bp.status.lock.Lock()
			self.bp.status.badPeers[self.id]++
			self.bp.status.lock.Unlock()
			// there is no persistence here, so GC will just take care of cleaning up

		// signal for peer switch, quit
		case <-self.switchC:
			var complete = "incomplete "
			if self.idle {
				complete = "complete"
			}
			glog.V(logger.Detail).Infof("HeadSection: <%s> section with head %s %s... quit request loop due to peer switch", self.id, hex(self.currentBlockHash), complete)
			break LOOP

		// global quit for blockpool
		case <-self.bp.quit:
			break LOOP

		// best
		case <-self.bestIdleTimer:
			self.peerError(self.bp.peers.errors.New(ErrIdleTooLong, "timed out without providing new blocks (td: %v, head: %s)...quitting", self.td, hex(self.currentBlockHash)))

			self.bp.status.lock.Lock()
			self.bp.status.badPeers[self.id]++
			self.bp.status.lock.Unlock()
			glog.V(logger.Detail).Infof("HeadSection: <%s> (headsection [%s]) quit channel closed : timed out without providing new blocks...quitting", self.id, sectionhex(self.headSection))
		}
	}

	if !self.idle {
		self.idle = true
		self.bp.wg.Done()
	}
}
