package consumer

import (
	"context"
	"errors"
	"time"

	"github.com/wormhole-foundation/wormhole-explorer/common/domain"
	"github.com/wormhole-foundation/wormhole-explorer/txtracker/chains"
	"github.com/wormhole-foundation/wormhole-explorer/txtracker/config"
	sdk "github.com/wormhole-foundation/wormhole/sdk/vaa"
	"go.uber.org/zap"
)

const (
	maxAttempts = 1
	retryDelay  = 5 * time.Minute
)

var ErrAlreadyProcessed = errors.New("VAA was already processed")

// ProcessSourceTxParams is a struct that contains the parameters for the ProcessSourceTx method.
type ProcessSourceTxParams struct {
	ChainId  sdk.ChainID
	VaaId    string
	Emitter  string
	Sequence string
	TxHash   string
	// Overwrite indicates whether to reprocess a VAA that has already been processed.
	//
	// In the context of backfilling, sometimes you want to overwrite old data (e.g.: because
	// the schema changed).
	// In the context of the service, you usually don't want to overwrite existing data
	// to avoid processing the same VAA twice, which would result in performance degradation.
	Overwrite bool
}

func ProcessSourceTx(
	ctx context.Context,
	logger *zap.Logger,
	rpcServiceProviderSettings *config.RpcProviderSettings,
	repository *Repository,
	params *ProcessSourceTxParams,
) error {

	// Get transaction details from the emitter blockchain
	//
	// If the transaction is not found, will retry a few times before giving up.
	var txStatus domain.SourceTxStatus
	var txDetail *chains.TxDetail
	var err error
	for attempts := 1; attempts <= maxAttempts; attempts++ {

		if !params.Overwrite {
			// If the message has already been processed, skip it.
			//
			// Sometimes the SQS visibility timeout expires and the message is put back into the queue,
			// even if the RPC nodes have been hit and data has been written to MongoDB.
			// In those cases, when we fetch the message for the second time,
			// we don't want to hit the RPC nodes again for performance reasons.
			processed, err := repository.AlreadyProcessed(ctx, params.VaaId)
			if err != nil {
				return err
			} else if err == nil && processed {
				return ErrAlreadyProcessed
			}
		}

		txDetail, err = chains.FetchTx(ctx, rpcServiceProviderSettings, params.ChainId, params.TxHash)

		switch {
		// If the transaction is not found, retry after a delay
		case errors.Is(err, chains.ErrTransactionNotFound):
			txStatus = domain.SourceTxStatusInternalError
			logger.Warn("transaction not found, will retry after a delay",
				zap.String("vaaId", params.VaaId),
				zap.Duration("retryDelay", retryDelay),
				zap.Int("attempts", attempts),
				zap.Int("maxAttempts", maxAttempts),
			)
			time.Sleep(retryDelay)
			continue

		// If the chain ID is not supported, we're done.
		case errors.Is(err, chains.ErrChainNotSupported):
			return err

		// If the context was cancelled, do not attempt to save the result on the database
		case errors.Is(err, context.Canceled):
			return err

		// If there is an internal error, give up
		case err != nil:
			logger.Error("Failed to fetch source transaction details",
				zap.String("vaaId", params.VaaId),
				zap.Error(err),
			)
			txStatus = domain.SourceTxStatusInternalError
			break

		// Success
		case err == nil:
			txStatus = domain.SourceTxStatusConfirmed
			break
		}
	}

	// Store source transaction details in the database
	p := UpsertDocumentParams{
		VaaId:    params.VaaId,
		ChainId:  params.ChainId,
		TxDetail: txDetail,
		TxStatus: txStatus,
	}
	return repository.UpsertDocument(ctx, &p)
}