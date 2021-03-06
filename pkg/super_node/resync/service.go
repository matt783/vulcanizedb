// VulcanizeDB
// Copyright © 2019 Vulcanize

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.

// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package resync

import (
	"fmt"
	"sync/atomic"

	"github.com/sirupsen/logrus"

	"github.com/vulcanize/vulcanizedb/libraries/shared/storage/utils"
	"github.com/vulcanize/vulcanizedb/pkg/super_node"
	"github.com/vulcanize/vulcanizedb/pkg/super_node/shared"
)

type Resync interface {
	Resync() error
}

type Service struct {
	// Interface for converting payloads into IPLD object payloads
	Converter shared.PayloadConverter
	// Interface for publishing the IPLD payloads to IPFS
	Publisher shared.IPLDPublisher
	// Interface for indexing the CIDs of the published IPLDs in Postgres
	Indexer shared.CIDIndexer
	// Interface for searching and retrieving CIDs from Postgres index
	Retriever shared.CIDRetriever
	// Interface for fetching payloads over at historical blocks; over http
	Fetcher shared.PayloadFetcher
	// Interface for cleaning out data before resyncing (if clearOldCache is on)
	Cleaner shared.Cleaner
	// Size of batch fetches
	BatchSize uint64
	// Number of goroutines
	BatchNumber int64
	// Channel for receiving quit signal
	QuitChan chan bool
	// Chain type
	chain shared.ChainType
	// Resync data type
	data shared.DataType
	// Resync ranges
	ranges [][2]uint64
	// Flag to turn on or off old cache destruction
	clearOldCache bool
	// Flag to turn on or off validation level reset
	resetValidation bool
}

// NewResyncService creates and returns a resync service from the provided settings
func NewResyncService(settings *Config) (Resync, error) {
	publisher, err := super_node.NewIPLDPublisher(settings.Chain, settings.IPFSPath)
	if err != nil {
		return nil, err
	}
	indexer, err := super_node.NewCIDIndexer(settings.Chain, settings.DB)
	if err != nil {
		return nil, err
	}
	converter, err := super_node.NewPayloadConverter(settings.Chain)
	if err != nil {
		return nil, err
	}
	retriever, err := super_node.NewCIDRetriever(settings.Chain, settings.DB)
	if err != nil {
		return nil, err
	}
	fetcher, err := super_node.NewPaylaodFetcher(settings.Chain, settings.HTTPClient, settings.Timeout)
	if err != nil {
		return nil, err
	}
	cleaner, err := super_node.NewCleaner(settings.Chain, settings.DB)
	if err != nil {
		return nil, err
	}
	batchSize := settings.BatchSize
	if batchSize == 0 {
		batchSize = super_node.DefaultMaxBatchSize
	}
	batchNumber := int64(settings.BatchNumber)
	if batchNumber == 0 {
		batchNumber = super_node.DefaultMaxBatchNumber
	}
	return &Service{
		Indexer:         indexer,
		Converter:       converter,
		Publisher:       publisher,
		Retriever:       retriever,
		Fetcher:         fetcher,
		Cleaner:         cleaner,
		BatchSize:       batchSize,
		BatchNumber:     int64(batchNumber),
		QuitChan:        settings.Quit,
		chain:           settings.Chain,
		ranges:          settings.Ranges,
		data:            settings.ResyncType,
		clearOldCache:   settings.ClearOldCache,
		resetValidation: settings.ResetValidation,
	}, nil
}

func (rs *Service) Resync() error {
	if rs.resetValidation {
		logrus.Infof("resetting validation level")
		if err := rs.Cleaner.ResetValidation(rs.ranges); err != nil {
			return fmt.Errorf("validation reset failed: %v", err)
		}
	}
	if rs.clearOldCache {
		logrus.Infof("cleaning out old data from Postgres")
		if err := rs.Cleaner.Clean(rs.ranges, rs.data); err != nil {
			return fmt.Errorf("%s %s data resync cleaning error: %v", rs.chain.String(), rs.data.String(), err)
		}
	}
	for _, rng := range rs.ranges {
		if err := rs.resync(rng[0], rng[1]); err != nil {
			return fmt.Errorf("%s %s data resync initialization error: %v", rs.chain.String(), rs.data.String(), err)
		}
	}
	return nil
}

func (rs *Service) resync(startingBlock, endingBlock uint64) error {
	logrus.Infof("resyncing %s data from %d to %d", rs.chain.String(), startingBlock, endingBlock)
	if endingBlock < startingBlock {
		return fmt.Errorf("%s resync range ending block number needs to be greater than the starting block number", rs.chain.String())
	}
	// break the range up into bins of smaller ranges
	blockRangeBins, err := utils.GetBlockHeightBins(startingBlock, endingBlock, rs.BatchSize)
	if err != nil {
		return err
	}
	// int64 for atomic incrementing and decrementing to track the number of active processing goroutines we have
	var activeCount int64
	// channel for processing goroutines to signal when they are done
	processingDone := make(chan bool)
	forwardDone := make(chan bool)

	// for each block range bin spin up a goroutine to batch fetch and process state diffs for that range
	go func() {
		for _, blockHeights := range blockRangeBins {
			// if we have reached our limit of active goroutines
			// wait for one to finish before starting the next
			if atomic.AddInt64(&activeCount, 1) > rs.BatchNumber {
				// this blocks until a process signals it has finished
				<-forwardDone
			}
			go func(blockHeights []uint64) {
				payloads, err := rs.Fetcher.FetchAt(blockHeights)
				if err != nil {
					logrus.Errorf("%s resync fetcher error: %s", rs.chain.String(), err.Error())
				}
				for _, payload := range payloads {
					ipldPayload, err := rs.Converter.Convert(payload)
					if err != nil {
						logrus.Errorf("%s resync converter error: %s", rs.chain.String(), err.Error())
					}
					cidPayload, err := rs.Publisher.Publish(ipldPayload)
					if err != nil {
						logrus.Errorf("%s resync publisher error: %s", rs.chain.String(), err.Error())
					}
					if err := rs.Indexer.Index(cidPayload); err != nil {
						logrus.Errorf("%s resync indexer error: %s", rs.chain.String(), err.Error())
					}
				}
				// when this goroutine is done, send out a signal
				logrus.Infof("finished %s resync section from %d to %d", rs.chain.String(), blockHeights[0], blockHeights[len(blockHeights)-1])
				processingDone <- true
			}(blockHeights)
		}
	}()

	// listen on the processingDone chan and
	// keep track of the number of processing goroutines that have finished
	// when they have all finished, sends the final signal out
	goroutinesFinished := 0
	for {
		select {
		case <-processingDone:
			atomic.AddInt64(&activeCount, -1)
			select {
			// if we are waiting for a process to finish, signal that one has
			case forwardDone <- true:
			default:
			}
			goroutinesFinished++
			if goroutinesFinished >= len(blockRangeBins) {
				return nil
			}
		}
	}
}
