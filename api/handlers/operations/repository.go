package operations

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wormhole-foundation/wormhole/sdk/vaa"

	"github.com/wormhole-foundation/wormhole-explorer/api/internal/errors"
	"github.com/wormhole-foundation/wormhole-explorer/api/internal/pagination"
	"github.com/wormhole-foundation/wormhole-explorer/common/utils"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

// Repository definition
type Repository struct {
	db          *mongo.Database
	logger      *zap.Logger
	collections struct {
		vaas               *mongo.Collection
		parsedVaa          *mongo.Collection
		globalTransactions *mongo.Collection
	}
}

// NewRepository create a new Repository.
func NewRepository(db *mongo.Database, logger *zap.Logger) *Repository {
	return &Repository{db: db,
		logger: logger.With(zap.String("module", "OperationRepository")),
		collections: struct {
			vaas               *mongo.Collection
			parsedVaa          *mongo.Collection
			globalTransactions *mongo.Collection
		}{
			vaas:               db.Collection("vaas"),
			parsedVaa:          db.Collection("parsedVaa"),
			globalTransactions: db.Collection("globalTransactions"),
		},
	}
}

// FindById returns the operations for the given chainID/emitter/seq.
func (r *Repository) FindById(ctx context.Context, id string) (*OperationDto, error) {

	var pipeline mongo.Pipeline

	// filter vaas by id
	pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.D{{Key: "_id", Value: id}}}})

	// lookup vaas
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "vaas"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "vaas"}}}})

	// lookup globalTransactions
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "globalTransactions"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "globalTransactions"}}}})

	// lookup transferPrices
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "transferPrices"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "transferPrices"}}}})

	// lookup parsedVaa
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "parsedVaa"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "parsedVaa"}}}})

	// add fields
	pipeline = append(pipeline, bson.D{{Key: "$addFields", Value: bson.D{
		{Key: "payload", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$parsedVaa.parsedPayload", 0}}}},
		{Key: "vaa", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$vaas", 0}}}},
		{Key: "standardizedProperties", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$parsedVaa.standardizedProperties", 0}}}},
		{Key: "symbol", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.symbol", 0}}}},
		{Key: "usdAmount", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.usdAmount", 0}}}},
		{Key: "tokenAmount", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.tokenAmount", 0}}}},
	}}})

	// unset
	pipeline = append(pipeline, bson.D{{Key: "$unset", Value: bson.A{"transferPrices", "parsedVaa"}}})

	// Execute the aggregation pipeline
	cur, err := r.collections.globalTransactions.Aggregate(ctx, pipeline)
	if err != nil {
		r.logger.Error("failed execute aggregation pipeline", zap.Error(err))
		return nil, err
	}

	// Read results from cursor
	var operations []*OperationDto
	err = cur.All(ctx, &operations)
	if err != nil {
		r.logger.Error("failed to decode cursor", zap.Error(err))
		return nil, err
	}

	// Check if there is only one operation
	if len(operations) > 1 {
		r.logger.Error("invalid number of operations", zap.Int("count", len(operations)))
		return nil, fmt.Errorf("invalid number of operations")
	}

	if len(operations) == 0 {
		return nil, errors.ErrNotFound
	}

	return operations[0], nil
}

type mongoID struct {
	Id string `bson:"_id"`
}

type OperationQuery struct {
	Pagination     pagination.Pagination
	TxHash         string
	Address        string
	SourceChainIDs []vaa.ChainID
	TargetChainIDs []vaa.ChainID
	AppIDs         []string
	ExclusiveAppId bool
	PayloadType    []int
	From           *time.Time
	To             *time.Time
}

func buildQueryOperationsByChain(sourceChainIDs, targetChainIDs []vaa.ChainID) bson.D {

	var allMatch bson.A

	if len(sourceChainIDs) > 0 {
		matchFromChain := bson.M{"rawStandardizedProperties.fromChain": bson.M{"$in": sourceChainIDs}}
		matchEmitterChain := bson.M{"emitterChain": bson.M{"$in": sourceChainIDs}}
		matchSourceChain := bson.M{"$or": bson.A{matchFromChain, matchEmitterChain}}
		allMatch = append(allMatch, matchSourceChain)
	}

	if len(targetChainIDs) > 0 {
		matchTargetChain := bson.M{"rawStandardizedProperties.toChain": bson.M{"$in": targetChainIDs}}
		allMatch = append(allMatch, matchTargetChain)
	}

	if (len(sourceChainIDs) == 1 && len(targetChainIDs) == 1) && (sourceChainIDs[0] == targetChainIDs[0]) {
		return bson.D{{Key: "$match", Value: bson.M{"$or": allMatch}}}
	}

	return bson.D{{Key: "$match", Value: bson.M{"$and": allMatch}}}
}

func buildQueryOperationsByAppID(appIDs []string, exclusive bool) bson.D {
	if !exclusive {
		return bson.D{{Key: "$match", Value: bson.M{"rawStandardizedProperties.appIds": bson.M{"$in": appIDs}}}}
	}
	matchAppID := bson.A{}
	for _, appID := range appIDs {
		cond := bson.M{"$and": bson.A{
			bson.M{"rawStandardizedProperties.appIds": bson.M{"$eq": appID}},
			bson.M{"rawStandardizedProperties.appIds": bson.M{"$size": 1}},
		}}
		matchAppID = append(matchAppID, cond)
	}
	return bson.D{{Key: "$match", Value: bson.M{"$or": matchAppID}}}
}

// findOperationsIdByAddress returns all operations filtered by address.
func findOperationsIdByAddress(ctx context.Context, db *mongo.Database, address string, pagination *pagination.Pagination) ([]string, error) {
	addressHex := strings.ToLower(address)
	if !utils.StartsWith0x(address) {
		addressHex = "0x" + strings.ToLower(addressHex)
	}

	matchGlobalTransactions := bson.D{{Key: "$match", Value: bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "originTx.from", Value: bson.M{"$eq": addressHex}}},
		bson.D{{Key: "originTx.from", Value: bson.M{"$eq": address}}},
		bson.D{{Key: "originTx.attribute.value.originAddress", Value: bson.M{"$eq": addressHex}}},
		bson.D{{Key: "originTx.attribute.value.originAddress", Value: bson.M{"$eq": address}}},
	}}}}}

	matchParsedVaa := bson.D{{Key: "$match", Value: bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "standardizedProperties.toAddress", Value: bson.M{"$eq": addressHex}}},
		bson.D{{Key: "standardizedProperties.toAddress", Value: bson.M{"$eq": address}}},
	}}}}}

	globalTransactionFilter := bson.D{{Key: "$unionWith", Value: bson.D{{Key: "coll", Value: "globalTransactions"}, {Key: "pipeline", Value: bson.A{matchGlobalTransactions}}}}}
	parserFilter := bson.D{{Key: "$unionWith", Value: bson.D{{Key: "coll", Value: "parsedVaa"}, {Key: "pipeline", Value: bson.A{matchParsedVaa}}}}}
	group := bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$_id"}}}}
	pipeline := []bson.D{globalTransactionFilter, parserFilter, group}

	cur, err := db.Collection("_operationsTemporal").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	var documents []mongoID
	err = cur.All(ctx, &documents)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, doc := range documents {
		ids = append(ids, doc.Id)
	}
	return ids, nil
}

// matchOperationByTxHash returns a mongo pipeline to match operations by txHash.
func (r *Repository) matchOperationByTxHash(ctx context.Context, txHash string) primitive.D {
	// build txHash field to search in mongo
	txHashHex := strings.ToLower(txHash)
	if !utils.StartsWith0x(txHash) {
		txHashHex = "0x" + strings.ToLower(txHashHex)
	}

	// build destination txHash field to search in mongo
	var qLowerWith0X, qHigherWith0X string
	qLower := strings.ToLower(txHash)
	qHigher := strings.ToUpper(txHash)
	if !utils.StartsWith0x(txHash) {
		qLowerWith0X = "0x" + strings.ToLower(qLower)
		qHigherWith0X = "0x" + strings.ToUpper(qHigher)
	}

	return bson.D{{Key: "$match", Value: bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "originTx.nativeTxHash", Value: bson.M{"$eq": txHashHex}}},
		bson.D{{Key: "originTx.nativeTxHash", Value: bson.M{"$eq": txHash}}},
		bson.D{{Key: "originTx.nativeTxHash", Value: bson.M{"$eq": qLower}}},
		bson.D{{Key: "originTx.nativeTxHash", Value: bson.M{"$eq": qHigher}}},
		bson.D{{Key: "originTx.attribute.value.originTxHash", Value: bson.M{"$eq": txHashHex}}},
		bson.D{{Key: "originTx.attribute.value.originTxHash", Value: bson.M{"$eq": txHash}}},
		bson.D{{Key: "originTx.attribute.value.originTxHash", Value: bson.M{"$eq": qLower}}},
		bson.D{{Key: "originTx.attribute.value.originTxHash", Value: bson.M{"$eq": qHigher}}},
		bson.D{{Key: "destinationTx.txHash", Value: bson.M{"$eq": txHash}}},
		bson.D{{Key: "destinationTx.txHash", Value: bson.M{"$eq": qLower}}},
		bson.D{{Key: "destinationTx.txHash", Value: bson.M{"$eq": qHigher}}},
		bson.D{{Key: "destinationTx.txHash", Value: bson.M{"$eq": qLowerWith0X}}},
		bson.D{{Key: "destinationTx.txHash", Value: bson.M{"$eq": qHigherWith0X}}},
	}}}}}
}

func (r *Repository) FindFromParsedVaa(ctx context.Context, query OperationQuery) ([]*OperationDto, error) {

	pipeline := BuildPipelineSearchFromParsedVaa(query)

	cur, err := r.collections.parsedVaa.Aggregate(ctx, pipeline)
	if err != nil {
		r.logger.Error("failed execute aggregation pipeline", zap.Error(err))
		return nil, err
	}

	// Read results from cursor
	var operations []*OperationDto
	err = cur.All(ctx, &operations)
	if err != nil {
		r.logger.Error("failed to decode cursor", zap.Error(err))
		return nil, err
	}

	return operations, nil
}

func BuildPipelineSearchFromParsedVaa(query OperationQuery) mongo.Pipeline {

	var pipeline mongo.Pipeline

	if len(query.PayloadType) > 0 {
		payloadTypeFilter := bson.D{{Key: "$match", Value: bson.M{"parsedPayload.payloadType": bson.M{"$in": query.PayloadType}}}}
		pipeline = append(pipeline, payloadTypeFilter)
	}

	if len(query.SourceChainIDs) > 0 || len(query.TargetChainIDs) > 0 {
		matchBySourceTargetChain := buildQueryOperationsByChain(query.SourceChainIDs, query.TargetChainIDs)
		pipeline = append(pipeline, matchBySourceTargetChain)
	}

	if len(query.AppIDs) > 0 {
		matchByAppId := buildQueryOperationsByAppID(query.AppIDs, query.ExclusiveAppId)
		pipeline = append(pipeline, matchByAppId)
	}

	if query.From != nil {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.M{"timestamp": bson.M{"$gte": query.From}}}})
	}

	if query.To != nil {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.M{"timestamp": bson.M{"$lte": query.To}}}})
	}

	pipeline = append(pipeline, bson.D{{Key: "$sort", Value: bson.D{
		bson.E{Key: "timestamp", Value: query.Pagination.GetSortInt()},
		bson.E{Key: "_id", Value: -1},
	}}})

	// Skip initial results
	pipeline = append(pipeline, bson.D{{Key: "$skip", Value: query.Pagination.Skip}})

	// Limit size of results
	pipeline = append(pipeline, bson.D{{Key: "$limit", Value: query.Pagination.Limit}})

	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "vaas"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "vaas"}}}})

	// lookup transferPrices
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "transferPrices"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "transferPrices"}}}})

	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "globalTransactions"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "globalTransactions"}}}})

	// add fields
	pipeline = append(pipeline, bson.D{{Key: "$addFields", Value: bson.D{
		{Key: "payload", Value: "$parsedPayload"},
		{Key: "vaa", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$vaas", 0}}}},
		{Key: "symbol", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.symbol", 0}}}},
		{Key: "usdAmount", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.usdAmount", 0}}}},
		{Key: "tokenAmount", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.tokenAmount", 0}}}},
		{Key: "originTx", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$globalTransactions.originTx", 0}}}},
		{Key: "destinationTx", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$globalTransactions.destinationTx", 0}}}},
	}}})

	// unset
	pipeline = append(pipeline, bson.D{{Key: "$unset", Value: bson.A{"transferPrices"}}})
	return pipeline
}

// FindAll returns all operations filtered by q.
func (r *Repository) FindAll(ctx context.Context, query OperationQuery) ([]*OperationDto, error) {

	var pipeline mongo.Pipeline

	// filter operations by address or txHash
	if query.Address != "" {
		// find all ids that match by address
		ids, err := findOperationsIdByAddress(ctx, r.db, query.Address, &query.Pagination)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return []*OperationDto{}, nil
		}
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}}})
	} else if query.TxHash != "" {
		// match operation by txHash (source tx and destination tx)
		matchByTxHash := r.matchOperationByTxHash(ctx, query.TxHash)
		pipeline = append(pipeline, matchByTxHash)
	}

	if query.From != nil {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.M{"originTx.timestamp": bson.M{"$gte": query.From}}}})
	}

	if query.To != nil {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.M{"originTx.timestamp": bson.M{"$lte": query.To}}}})
	}

	// sort
	pipeline = append(pipeline, bson.D{{Key: "$sort", Value: bson.D{
		bson.E{Key: "originTx.timestamp", Value: query.Pagination.GetSortInt()},
		bson.E{Key: "_id", Value: -1},
	}}})

	// Skip initial results
	pipeline = append(pipeline, bson.D{{Key: "$skip", Value: query.Pagination.Skip}})

	// Limit size of results
	pipeline = append(pipeline, bson.D{{Key: "$limit", Value: query.Pagination.Limit}})

	// lookup vaas
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "vaas"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "vaas"}}}})

	// lookup globalTransactions
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "globalTransactions"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "globalTransactions"}}}})

	// lookup transferPrices
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "transferPrices"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "transferPrices"}}}})

	// lookup parsedVaa
	pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "parsedVaa"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "parsedVaa"}}}})

	// add fields
	pipeline = append(pipeline, bson.D{{Key: "$addFields", Value: bson.D{
		{Key: "payload", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$parsedVaa.parsedPayload", 0}}}},
		{Key: "vaa", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$vaas", 0}}}},
		{Key: "standardizedProperties", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$parsedVaa.standardizedProperties", 0}}}},
		{Key: "symbol", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.symbol", 0}}}},
		{Key: "usdAmount", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.usdAmount", 0}}}},
		{Key: "tokenAmount", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$transferPrices.tokenAmount", 0}}}},
	}}})

	// unset
	pipeline = append(pipeline, bson.D{{Key: "$unset", Value: bson.A{"transferPrices", "parsedVaa"}}})

	// Execute the aggregation pipeline
	cur, err := r.collections.globalTransactions.Aggregate(ctx, pipeline)
	if err != nil {
		r.logger.Error("failed execute aggregation pipeline", zap.Error(err))
		return nil, err
	}

	// Read results from cursor
	var operations []*OperationDto
	err = cur.All(ctx, &operations)
	if err != nil {
		r.logger.Error("failed to decode cursor", zap.Error(err))
		return nil, err
	}

	return operations, nil
}
