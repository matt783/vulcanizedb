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

package btc

import (
	"fmt"
	"math/big"

	"github.com/vulcanize/vulcanizedb/libraries/shared/storage/utils"

	"github.com/lib/pq"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"

	"github.com/vulcanize/vulcanizedb/pkg/postgres"
	"github.com/vulcanize/vulcanizedb/pkg/super_node/shared"
)

// CIDRetriever satisfies the CIDRetriever interface for bitcoin
type CIDRetriever struct {
	db *postgres.DB
}

// NewCIDRetriever returns a pointer to a new CIDRetriever which supports the CIDRetriever interface
func NewCIDRetriever(db *postgres.DB) *CIDRetriever {
	return &CIDRetriever{
		db: db,
	}
}

// RetrieveFirstBlockNumber is used to retrieve the first block number in the db
func (ecr *CIDRetriever) RetrieveFirstBlockNumber() (int64, error) {
	var blockNumber int64
	err := ecr.db.Get(&blockNumber, "SELECT block_number FROM btc.header_cids ORDER BY block_number ASC LIMIT 1")
	return blockNumber, err
}

// RetrieveLastBlockNumber is used to retrieve the latest block number in the db
func (ecr *CIDRetriever) RetrieveLastBlockNumber() (int64, error) {
	var blockNumber int64
	err := ecr.db.Get(&blockNumber, "SELECT block_number FROM btc.header_cids ORDER BY block_number DESC LIMIT 1 ")
	return blockNumber, err
}

// Retrieve is used to retrieve all of the CIDs which conform to the passed StreamFilters
func (ecr *CIDRetriever) Retrieve(filter shared.SubscriptionSettings, blockNumber int64) ([]shared.CIDsForFetching, bool, error) {
	streamFilter, ok := filter.(*SubscriptionSettings)
	if !ok {
		return nil, true, fmt.Errorf("btc retriever expected filter type %T got %T", &SubscriptionSettings{}, filter)
	}
	log.Debug("retrieving cids")
	tx, err := ecr.db.Beginx()
	if err != nil {
		return nil, true, err
	}

	// Retrieve cached header CIDs
	headers, err := ecr.RetrieveHeaderCIDs(tx, blockNumber)
	if err != nil {
		if err := tx.Rollback(); err != nil {
			log.Error(err)
		}
		log.Error("header cid retrieval error")
		return nil, true, err
	}
	cws := make([]shared.CIDsForFetching, len(headers))
	empty := true
	for i, header := range headers {
		cw := new(CIDWrapper)
		cw.BlockNumber = big.NewInt(blockNumber)
		if !streamFilter.HeaderFilter.Off {
			cw.Header = header
			empty = false
		}
		// Retrieve cached trx CIDs
		if !streamFilter.TxFilter.Off {
			cw.Transactions, err = ecr.RetrieveTxCIDs(tx, streamFilter.TxFilter, header.ID)
			if err != nil {
				if err := tx.Rollback(); err != nil {
					log.Error(err)
				}
				log.Error("transaction cid retrieval error")
				return nil, true, err
			}
			if len(cw.Transactions) > 0 {
				empty = false
			}
		}
		cws[i] = cw
	}

	return cws, empty, tx.Commit()
}

// RetrieveHeaderCIDs retrieves and returns all of the header cids at the provided blockheight
func (ecr *CIDRetriever) RetrieveHeaderCIDs(tx *sqlx.Tx, blockNumber int64) ([]HeaderModel, error) {
	log.Debug("retrieving header cids for block ", blockNumber)
	headers := make([]HeaderModel, 0)
	pgStr := `SELECT * FROM btc.header_cids
				WHERE block_number = $1`
	return headers, tx.Select(&headers, pgStr, blockNumber)
}

// RetrieveTxCIDs retrieves and returns all of the trx cids at the provided blockheight that conform to the provided filter parameters
// also returns the ids for the returned transaction cids
func (ecr *CIDRetriever) RetrieveTxCIDs(tx *sqlx.Tx, txFilter TxFilter, headerID int64) ([]TxModel, error) {
	log.Debug("retrieving transaction cids for header id ", headerID)
	args := make([]interface{}, 0, 3)
	results := make([]TxModel, 0)
	id := 1
	pgStr := fmt.Sprintf(`SELECT transaction_cids.id, transaction_cids.header_id,
 			transaction_cids.tx_hash, transaction_cids.cid,
 			transaction_cids.segwit, transaction_cids.witness_hash, transaction_cids.index
 			FROM btc.transaction_cids, btc.header_cids, btc.tx_inputs, btc.tx_outputs
			WHERE transaction_cids.header_id = header_cids.id
			AND tx_inputs.tx_id = transaction_cids.id
			AND tx_outputs.tx_id = transaction_cids.id
			AND header_cids.id = $%d`, id)
	args = append(args, headerID)
	id++
	if txFilter.Segwit {
		pgStr += ` AND transaction_cids.segwit = true`
	}
	if txFilter.MultiSig {
		pgStr += ` AND tx_outputs.required_sigs > 1`
	}
	if len(txFilter.WitnessHashes) > 0 {
		pgStr += fmt.Sprintf(` AND transaction_cids.witness_hash = ANY($%d::VARCHAR(66)[])`, id)
		args = append(args, pq.Array(txFilter.WitnessHashes))
		id++
	}
	if len(txFilter.Addresses) > 0 {
		pgStr += fmt.Sprintf(` AND tx_outputs.addresses && $%d::VARCHAR(66)[]`, id)
		args = append(args, pq.Array(txFilter.Addresses))
		id++
	}
	if len(txFilter.Indexes) > 0 {
		pgStr += fmt.Sprintf(` AND transaction_cids.index = ANY($%d::INTEGER[])`, id)
		args = append(args, pq.Array(txFilter.Indexes))
		id++
	}
	if len(txFilter.PkScriptClasses) > 0 {
		pgStr += fmt.Sprintf(` AND tx_outputs.script_class = ANY($%d::INTEGER[])`, id)
		args = append(args, pq.Array(txFilter.PkScriptClasses))
	}
	return results, tx.Select(&results, pgStr, args...)
}

// RetrieveGapsInData is used to find the the block numbers at which we are missing data in the db
func (ecr *CIDRetriever) RetrieveGapsInData(validationLevel int) ([]shared.Gap, error) {
	pgStr := `SELECT header_cids.block_number + 1 AS start, min(fr.block_number) - 1 AS stop FROM btc.header_cids
				LEFT JOIN btc.header_cids r on btc.header_cids.block_number = r.block_number - 1
				LEFT JOIN btc.header_cids fr on btc.header_cids.block_number < fr.block_number
				WHERE r.block_number is NULL and fr.block_number IS NOT NULL
				GROUP BY header_cids.block_number, r.block_number`
	results := make([]struct {
		Start uint64 `db:"start"`
		Stop  uint64 `db:"stop"`
	}, 0)
	if err := ecr.db.Select(&results, pgStr); err != nil {
		return nil, err
	}
	emptyGaps := make([]shared.Gap, len(results))
	for i, res := range results {
		emptyGaps[i] = shared.Gap{
			Start: res.Start,
			Stop:  res.Stop,
		}
	}

	// Find sections of blocks where we are below the validation level
	// There will be no overlap between these "gaps" and the ones above
	pgStr = `SELECT block_number FROM btc.header_cids
			WHERE times_validated < $1
			ORDER BY block_number`
	var heights []uint64
	if err := ecr.db.Select(&heights, pgStr, validationLevel); err != nil {
		return nil, err
	}
	if len(heights) == 0 {
		return emptyGaps, nil
	}
	return append(emptyGaps, utils.MissingHeightsToGaps(heights)...), nil
}

// RetrieveBlockByHash returns all of the CIDs needed to compose an entire block, for a given block hash
func (ecr *CIDRetriever) RetrieveBlockByHash(blockHash common.Hash) (HeaderModel, []TxModel, error) {
	log.Debug("retrieving block cids for block hash ", blockHash.String())
	tx, err := ecr.db.Beginx()
	if err != nil {
		return HeaderModel{}, nil, err
	}
	headerCID, err := ecr.RetrieveHeaderCIDByHash(tx, blockHash)
	if err != nil {
		if err := tx.Rollback(); err != nil {
			log.Error(err)
		}
		log.Error("header cid retrieval error")
		return HeaderModel{}, nil, err
	}
	txCIDs, err := ecr.RetrieveTxCIDsByHeaderID(tx, headerCID.ID)
	if err != nil {
		if err := tx.Rollback(); err != nil {
			log.Error(err)
		}
		log.Error("tx cid retrieval error")
		return HeaderModel{}, nil, err
	}
	return headerCID, txCIDs, tx.Commit()
}

// RetrieveBlockByNumber returns all of the CIDs needed to compose an entire block, for a given block number
func (ecr *CIDRetriever) RetrieveBlockByNumber(blockNumber int64) (HeaderModel, []TxModel, error) {
	log.Debug("retrieving block cids for block number ", blockNumber)
	tx, err := ecr.db.Beginx()
	if err != nil {
		return HeaderModel{}, nil, err
	}
	headerCID, err := ecr.RetrieveHeaderCIDs(tx, blockNumber)
	if err != nil {
		if err := tx.Rollback(); err != nil {
			log.Error(err)
		}
		log.Error("header cid retrieval error")
		return HeaderModel{}, nil, err
	}
	if len(headerCID) < 1 {
		return HeaderModel{}, nil, fmt.Errorf("header cid retrieval error, no header CIDs found at block %d", blockNumber)
	}
	txCIDs, err := ecr.RetrieveTxCIDsByHeaderID(tx, headerCID[0].ID)
	if err != nil {
		if err := tx.Rollback(); err != nil {
			log.Error(err)
		}
		log.Error("tx cid retrieval error")
		return HeaderModel{}, nil, err
	}
	return headerCID[0], txCIDs, tx.Commit()
}

// RetrieveHeaderCIDByHash returns the header for the given block hash
func (ecr *CIDRetriever) RetrieveHeaderCIDByHash(tx *sqlx.Tx, blockHash common.Hash) (HeaderModel, error) {
	log.Debug("retrieving header cids for block hash ", blockHash.String())
	pgStr := `SELECT * FROM btc.header_cids
			WHERE block_hash = $1`
	var headerCID HeaderModel
	return headerCID, tx.Get(&headerCID, pgStr, blockHash.String())
}

// RetrieveTxCIDsByHeaderID retrieves all tx CIDs for the given header id
func (ecr *CIDRetriever) RetrieveTxCIDsByHeaderID(tx *sqlx.Tx, headerID int64) ([]TxModel, error) {
	log.Debug("retrieving tx cids for block id ", headerID)
	pgStr := `SELECT * FROM btc.transaction_cids
			WHERE header_id = $1`
	var txCIDs []TxModel
	return txCIDs, tx.Select(&txCIDs, pgStr, headerID)
}
