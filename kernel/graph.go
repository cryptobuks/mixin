package kernel

import (
	"fmt"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
)

func (node *Node) handleSnapshotInput(s *common.Snapshot) error {
	o, err := node.store.SnapshotsReadSnapshotByTransactionHash(s.Transaction.PayloadHash())
	if err != nil {
		logger.Println("READ SNAPSHOT BY TRANSACTION ERROR", err)
		return nil
	}
	if o != nil {
		return nil
	}
	err = s.Transaction.Validate(node.store)
	if err != nil {
		logger.Println("VALIDATE TRANSACTION ERROR", err)
		return nil
	}

	defer node.Graph.UpdateFinalCache()
	node.clearConsensusSignatures(s)

	cache, final, err := node.signSnapshot(s)
	if err != nil {
		return err
	}

	var links map[crypto.Hash]uint64
	if s.NodeId != node.IdForNetwork || len(s.Signatures) > 1 {
		links, cache, final, err = node.verifySnapshot(s)
		if err != nil {
			return err
		}
	}

	if node.verifyFinalization(s) {
		cache.Snapshots = append(cache.Snapshots, s)
		cache.End = s.Timestamp
		topo := &common.SnapshotWithTopologicalOrder{
			Snapshot:         *s,
			TopologicalOrder: node.TopoCounter.Next(),
			RoundLinks:       links,
		}
		err := node.store.SnapshotsWriteSnapshot(topo)
		if err != nil {
			return err
		}
		node.Graph.CacheRound[s.NodeId] = cache
		node.Graph.FinalRound[s.NodeId] = final
		return nil
	}

	err = s.LockInputs(node.store)
	if err != nil {
		logger.Println("LOCK INPUTS ERROR", err)
		return nil
	}
	node.sign(s)

	if node.IdForNetwork == s.NodeId {
		for _, cn := range node.ConsensusNodes {
			if !cn.IsAccepted() {
				continue
			}
			peerId := cn.Account.Hash().ForNetwork(node.networkId)
			cacheId := s.PayloadHash().ForNetwork(peerId)
			if time.Now().Before(node.ConsensusCache[cacheId].Add(time.Duration(config.SnapshotRoundGap))) {
				continue
			}
			err = node.Peer.SendSnapshotMessage(peerId, s)
			if err != nil {
				return err
			}
			node.ConsensusCache[cacheId] = time.Now()
		}
	} else {
		// FIXME gossip peers are different from consensus nodes
		err := node.Peer.SendSnapshotMessage(s.NodeId, s)
		if err != nil {
			return err
		}
	}

	node.Graph.CacheRound[s.NodeId] = cache
	node.Graph.FinalRound[s.NodeId] = final
	return nil
}

func (node *Node) clearConsensusSignatures(s *common.Snapshot) {
	msg := s.Payload()
	sigs := make([]crypto.Signature, 0)
	filter := make(map[crypto.Signature]bool)
	for _, sig := range s.Signatures {
		if filter[sig] {
			continue
		}
		for _, cn := range node.ConsensusNodes {
			if !cn.IsAccepted() {
				continue
			}
			if cn.Account.PublicSpendKey.Verify(msg, sig) {
				sigs = append(sigs, sig)
			}
		}
		filter[sig] = true
	}
	s.Signatures = sigs
}

func (node *Node) verifyReferences(self FinalRound, s *common.Snapshot) (map[crypto.Hash]uint64, bool, error) {
	links := make(map[crypto.Hash]uint64)
	if len(s.References) != 2 {
		return links, true, fmt.Errorf("invalid reference count %d", len(s.References))
	}
	ref0, ref1 := s.References[0], s.References[1]
	if ref0 == ref1 {
		return links, true, fmt.Errorf("same references %s", s.Transaction.PayloadHash().String())
	}

	if ref0 != self.Hash {
		return links, true, fmt.Errorf("invalid self reference %s %s %s", s.Transaction.PayloadHash(), ref0, self.Hash)
	}
	if s.NodeId != self.NodeId {
		panic(*s)
	}

	for _, final := range node.Graph.FinalRound {
		if final.NodeId == s.NodeId || final.Hash != ref1 {
			continue
		}
		links[self.NodeId] = self.Number
		links[final.NodeId] = final.Number
		selfLink, err := node.store.SnapshotsReadRoundLink(s.NodeId, self.NodeId)
		if err != nil {
			return links, false, err
		}
		if links[self.NodeId] < selfLink {
			return links, true, fmt.Errorf("invalid self reference %d=>%d", selfLink, links[self.NodeId])
		}
		finalLink, err := node.store.SnapshotsReadRoundLink(s.NodeId, final.NodeId)
		if err != nil {
			return links, false, err
		}
		if links[final.NodeId] < finalLink {
			return links, true, fmt.Errorf("invalid final reference %d=>%d", finalLink, links[final.NodeId])
		}
		return links, true, nil
	}
	return links, true, fmt.Errorf("invalid references %s", s.Transaction.PayloadHash().String())
}

func (node *Node) verifyFinalization(s *common.Snapshot) bool {
	consensusThreshold := len(node.ConsensusNodes) * 2 / 3
	return len(s.Signatures) > consensusThreshold
}

func (node *Node) verifySnapshot(s *common.Snapshot) (map[crypto.Hash]uint64, *CacheRound, *FinalRound, error) {
	logger.Println("VERIFY SNAPSHOT", *s)
	cache := node.Graph.CacheRound[s.NodeId].Copy()
	final := node.Graph.FinalRound[s.NodeId].Copy()

	if osigs := node.SnapshotsPool[s.PayloadHash()]; len(osigs) > 0 || node.verifyFinalization(s) {
		links, handled, err := node.verifyReferences(*final, s)
		if err != nil {
			logger.Println(err)
			if !handled {
				return links, cache, final, err
			}
			return links, cache, final, nil
		}
		filter := make(map[crypto.Signature]bool)
		for _, sig := range s.Signatures {
			filter[sig] = true
		}
		for _, sig := range osigs {
			if filter[sig] {
				continue
			}
			s.Signatures = append(s.Signatures, sig)
			filter[sig] = true
		}
		node.SnapshotsPool[s.PayloadHash()] = append([]crypto.Signature{}, s.Signatures...)
		return links, cache, final, nil
	}

	if s.Timestamp >= config.SnapshotRoundGap+cache.Start {
		if len(cache.Snapshots) == 0 {
			cache.Start = s.Timestamp
		} else {
			for _, ps := range cache.Snapshots {
				if !node.verifyFinalization(ps) {
					panic("cache is the new final, round snapshots should have been finalized")
				}
			}

			final = cache.asFinal()
			cache = &CacheRound{
				NodeId: s.NodeId,
				Number: cache.Number + 1,
				Start:  s.Timestamp,
				End:    s.Timestamp,
			}
		}
	}

	if s.RoundNumber != cache.Number || s.Timestamp < cache.End {
		return nil, cache, final, nil
	}

	links, handled, err := node.verifyReferences(*final, s)
	if err != nil {
		logger.Println(err)
		if !handled {
			return links, cache, final, err
		}
		return links, cache, final, nil
	}
	return links, cache, final, nil
}

func (node *Node) signSnapshot(s *common.Snapshot) (*CacheRound, *FinalRound, error) {
	cache := node.Graph.CacheRound[s.NodeId].Copy()
	final := node.Graph.FinalRound[s.NodeId].Copy()

	if s.NodeId != node.IdForNetwork || len(s.Signatures) != 0 || s.Timestamp != 0 {
		return cache, final, nil
	}
	logger.Println("SIGN SNAPSHOT", *s)

	for {
		s.Timestamp = uint64(time.Now().UnixNano())
		if s.Timestamp > cache.End {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}
	if s.Timestamp >= config.SnapshotRoundGap+cache.Start {
		if len(cache.Snapshots) == 0 {
			cache.Start = s.Timestamp
		} else {
			for _, ps := range cache.Snapshots {
				if !node.verifyFinalization(ps) {
					panic("cache is the new final, round snapshots should have been finalized")
				}
			}

			final = cache.asFinal()
			cache = &CacheRound{
				NodeId: s.NodeId,
				Number: cache.Number + 1,
				Start:  s.Timestamp,
			}
		}
	}
	cache.End = s.Timestamp

	best := &FinalRound{NodeId: final.NodeId}
	for _, r := range node.Graph.FinalRound {
		if r.NodeId != s.NodeId && r.Start >= best.Start && r.End < uint64(time.Now().UnixNano()) {
			best = r
		}
	}
	if best.NodeId == final.NodeId {
		panic(node.IdForNetwork)
	}

	s.RoundNumber = cache.Number
	s.References = [2]crypto.Hash{final.Hash, best.Hash}
	return cache, final, nil
}

func (node *Node) sign(s *common.Snapshot) {
	s.Sign(node.Account.PrivateSpendKey)
	node.clearConsensusSignatures(s)
	node.SnapshotsPool[s.PayloadHash()] = append([]crypto.Signature{}, s.Signatures...)
}
