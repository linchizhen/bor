// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package bor

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/params"
	lru "github.com/hashicorp/golang-lru"
)

// Snapshot is the state of the authorization voting at a given point in time.
type Snapshot struct {
	config   *params.BorConfig // Consensus engine parameters to fine tune behavior
	ethAPI   *ethapi.PublicBlockChainAPI
	sigcache *lru.ARCCache // Cache of recent block signatures to speed up ecrecover

	Number       uint64                    `json:"number"`       // Block number where the snapshot was created
	Hash         common.Hash               `json:"hash"`         // Block hash where the snapshot was created
	ValidatorSet *ValidatorSet             `json:"validatorSet"` // Validator set at this moment
	Recents      map[uint64]common.Address `json:"recents"`      // Set of recent signers for spam protections
}

// signersAscending implements the sort interface to allow sorting a list of addresses
type signersAscending []common.Address

func (s signersAscending) Len() int           { return len(s) }
func (s signersAscending) Less(i, j int) bool { return bytes.Compare(s[i][:], s[j][:]) < 0 }
func (s signersAscending) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// newSnapshot creates a new snapshot with the specified startup parameters. This
// method does not initialize the set of recent signers, so only ever use if for
// the genesis block.
func newSnapshot(
	config *params.BorConfig,
	sigcache *lru.ARCCache,
	number uint64,
	hash common.Hash,
	validators []*Validator,
	ethAPI *ethapi.PublicBlockChainAPI,
) *Snapshot {
	snap := &Snapshot{
		config:       config,
		ethAPI:       ethAPI,
		sigcache:     sigcache,
		Number:       number,
		Hash:         hash,
		ValidatorSet: NewValidatorSet(validators),
		Recents:      make(map[uint64]common.Address),
	}
	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *params.BorConfig, sigcache *lru.ARCCache, db ethdb.Database, hash common.Hash, ethAPI *ethapi.PublicBlockChainAPI) (*Snapshot, error) {
	blob, err := db.Get(append([]byte("bor-"), hash[:]...))
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.config = config
	snap.sigcache = sigcache
	snap.ethAPI = ethAPI

	// update total voting power
	snap.ValidatorSet.updateTotalVotingPower()

	return snap, nil
}

// store inserts the snapshot into the database.
func (s *Snapshot) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append([]byte("bor-"), s.Hash[:]...), blob)
}

// copy creates a deep copy of the snapshot, though not the individual votes.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{
		config:       s.config,
		ethAPI:       s.ethAPI,
		sigcache:     s.sigcache,
		Number:       s.Number,
		Hash:         s.Hash,
		ValidatorSet: s.ValidatorSet.Copy(),
		Recents:      make(map[uint64]common.Address),
	}
	for block, signer := range s.Recents {
		cpy.Recents[block] = signer
	}

	return cpy
}

// // validVote returns whether it makes sense to cast the specified vote in the
// // given snapshot context (e.g. don't try to add an already authorized signer).
// func (s *Snapshot) validVote(address common.Address, authorize bool) bool {
// 	_, signer := s.Signers[address]
// 	return (signer && !authorize) || (!signer && authorize)
// }

// // cast adds a new vote into the tally.
// func (s *Snapshot) cast(address common.Address, authorize bool) bool {
// 	// Ensure the vote is meaningful
// 	if !s.validVote(address, authorize) {
// 		return false
// 	}
// 	// Cast the vote into an existing or new tally
// 	if old, ok := s.Tally[address]; ok {
// 		old.Votes++
// 		s.Tally[address] = old
// 	} else {
// 		s.Tally[address] = Tally{Authorize: authorize, Votes: 1}
// 	}
// 	return true
// }

// // uncast removes a previously cast vote from the tally.
// func (s *Snapshot) uncast(address common.Address, authorize bool) bool {
// 	// If there's no tally, it's a dangling vote, just drop
// 	tally, ok := s.Tally[address]
// 	if !ok {
// 		return false
// 	}
// 	// Ensure we only revert counted votes
// 	if tally.Authorize != authorize {
// 		return false
// 	}
// 	// Otherwise revert the vote
// 	if tally.Votes > 1 {
// 		tally.Votes--
// 		s.Tally[address] = tally
// 	} else {
// 		delete(s.Tally, address)
// 	}
// 	return true
// }

func (s *Snapshot) apply(headers []*types.Header) (*Snapshot, error) {
	// Allow passing in no headers for cleaner code
	if len(headers) == 0 {
		return s, nil
	}
	// Sanity check that the headers can be applied
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].Number.Uint64() != headers[i].Number.Uint64()+1 {
			return nil, errOutOfRangeChain
		}
	}
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, errOutOfRangeChain
	}
	// Iterate through the headers and create a new snapshot
	snap := s.copy()

	for _, header := range headers {
		// Remove any votes on checkpoint blocks
		number := header.Number.Uint64()

		// Delete the oldest signer from the recent list to allow it signing again
		if number >= s.config.Sprint && number-s.config.Sprint >= 0 {
			delete(snap.Recents, number-s.config.Sprint)
		}

		// Resolve the authorization key and check against signers
		signer, err := ecrecover(header, s.sigcache)
		if err != nil {
			return nil, err
		}

		// change validator set and change proposer
		if number > 0 && (number+1)%s.config.Sprint == 0 {
			validatorBytes := header.Extra[extraVanity : len(header.Extra)-extraSeal]

			// get validators from headers and use that for new validator set
			newVals, _ := ParseValidators(validatorBytes)
			v := getUpdatedValidatorSet(snap.ValidatorSet.Copy(), newVals)
			v.IncrementProposerPriority(1)
			snap.ValidatorSet = v

			// log new validator set
			fmt.Println("Current validator set", "number", snap.Number, "validatorSet", snap.ValidatorSet)
		}

		// check if signer is in validator set
		if !snap.ValidatorSet.HasAddress(signer.Bytes()) {
			return nil, errUnauthorizedSigner
		}

		//
		// Check validator
		//

		validators := snap.ValidatorSet.Validators
		// proposer will be the last signer if block is not epoch block
		proposer := snap.ValidatorSet.GetProposer().Address
		// if number%s.config.Sprint != 0 {
		// 	proposer = snap.Recents[number-1]
		// }
		proposerIndex, _ := snap.ValidatorSet.GetByAddress(proposer)
		signerIndex, _ := snap.ValidatorSet.GetByAddress(signer)
		limit := len(validators) - (len(validators)/2 + 1)

		// temp index
		tempIndex := signerIndex
		if proposerIndex != tempIndex && limit > 0 {
			if tempIndex < proposerIndex {
				tempIndex = tempIndex + len(validators)
			}

			if tempIndex-proposerIndex > limit {
				return nil, errRecentlySigned
			}
		}

		// add recents
		snap.Recents[number] = signer
		// TODO remove
		fmt.Println("Recent signer", "number", number, "signer", signer.Hex())
	}
	snap.Number += uint64(len(headers))
	snap.Hash = headers[len(headers)-1].Hash()

	return snap, nil
}

// signers retrieves the list of authorized signers in ascending order.
func (s *Snapshot) signers() []common.Address {
	sigs := make([]common.Address, 0, len(s.ValidatorSet.Validators))
	for _, sig := range s.ValidatorSet.Validators {
		sigs = append(sigs, sig.Address)
	}
	return sigs
}

// inturn returns if a signer at a given block height is in-turn or not.
func (s *Snapshot) inturn(number uint64, signer common.Address, epoch uint64) uint64 {
	// if signer is empty
	if bytes.Compare(signer.Bytes(), common.Address{}.Bytes()) == 0 {
		return 1
	}

	validators := s.ValidatorSet.Validators
	proposer := s.ValidatorSet.GetProposer().Address
	totalValidators := len(validators)

	// proposer will be the last signer if block is not epoch block
	// proposer := snap.ValidatorSet.GetProposer().Address
	// if number%epoch != 0 {
	// 	proposer = snap.Recents[number-1]
	// }
	proposerIndex, _ := s.ValidatorSet.GetByAddress(proposer)
	signerIndex, _ := s.ValidatorSet.GetByAddress(signer)

	// temp index
	tempIndex := signerIndex
	if tempIndex < proposerIndex {
		tempIndex = tempIndex + totalValidators
	}

	return uint64(totalValidators - (tempIndex - proposerIndex))

	// signers, offset := s.signers(), 0
	// for offset < len(signers) && signers[offset] != signer {
	// 	offset++
	// }
	// return ((number / producerPeriod) % uint64(len(signers))) == uint64(offset)

	// // if block is epoch start block, proposer will be inturn signer
	// if s.Number%epoch == 0 {
	// 	if bytes.Compare(proposer.Address.Bytes(), signer.Bytes()) == 0 {
	// 		return true
	// 	}
	// 	// if block is not epoch block, last block signer will be inturn
	// } else if bytes.Compare(lastSigner.Bytes(), signer.Bytes()) == 0 {
	// 	return false
	// }
	// return false
}
