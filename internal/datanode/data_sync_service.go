// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datanode

import (
	"context"
	"errors"

	"github.com/milvus-io/milvus/internal/kv"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/msgstream"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/flowgraph"

	"go.uber.org/zap"
)

// dataSyncService controls a flowgraph for a specific collection
type dataSyncService struct {
	ctx          context.Context
	cancelFn     context.CancelFunc
	fg           *flowgraph.TimeTickedFlowGraph // internal flowgraph processes insert/delta messages
	flushCh      chan flushMsg                  // chan to notify flush
	replica      Replica                        // segment replica stores meta
	idAllocator  allocatorInterface             // id/timestamp allocator
	msFactory    msgstream.Factory
	collectionID UniqueID        // collection id of vchan for which this data sync service serves
	dataCoord    types.DataCoord // DataCoord instance to interact with
	clearSignal  chan<- UniqueID // signal channel to notify flowgraph close for collection/partition drop msg consumed

	flushingSegCache *Cache       // a guarding cache stores currently flushing segment ids
	flushManager     flushManager // flush manager handles flush process
	blobKV           kv.BaseKV
}

func newDataSyncService(ctx context.Context,
	flushCh chan flushMsg,
	replica Replica,
	alloc allocatorInterface,
	factory msgstream.Factory,
	vchan *datapb.VchannelInfo,
	clearSignal chan<- UniqueID,
	dataCoord types.DataCoord,
	flushingSegCache *Cache,
	blobKV kv.BaseKV,

) (*dataSyncService, error) {

	if replica == nil {
		return nil, errors.New("Nil input")
	}

	ctx1, cancel := context.WithCancel(ctx)

	service := &dataSyncService{
		ctx:              ctx1,
		cancelFn:         cancel,
		fg:               nil,
		flushCh:          flushCh,
		replica:          replica,
		idAllocator:      alloc,
		msFactory:        factory,
		collectionID:     vchan.GetCollectionID(),
		dataCoord:        dataCoord,
		clearSignal:      clearSignal,
		flushingSegCache: flushingSegCache,
		blobKV:           blobKV,
	}

	if err := service.initNodes(vchan); err != nil {
		return nil, err
	}
	return service, nil
}

type parallelConfig struct {
	maxQueueLength int32
	maxParallelism int32
}

type nodeConfig struct {
	msFactory    msgstream.Factory // msgStream factory
	collectionID UniqueID
	vChannelName string
	replica      Replica // Segment replica
	allocator    allocatorInterface

	// defaults
	parallelConfig
}

func newParallelConfig() parallelConfig {
	return parallelConfig{Params.FlowGraphMaxQueueLength, Params.FlowGraphMaxParallelism}
}

// start starts the flowgraph in datasyncservice
func (dsService *dataSyncService) start() {
	if dsService.fg != nil {
		log.Debug("Data Sync Service starting flowgraph")
		dsService.fg.Start()
	} else {
		log.Debug("Data Sync Service flowgraph nil")
	}
}

func (dsService *dataSyncService) close() {
	if dsService.fg != nil {
		log.Debug("Data Sync Service closing flowgraph")
		dsService.fg.Close()
	}

	dsService.cancelFn()
	dsService.flushManager.close()
}

// initNodes inits a TimetickedFlowGraph
func (dsService *dataSyncService) initNodes(vchanInfo *datapb.VchannelInfo) error {
	dsService.fg = flowgraph.NewTimeTickedFlowGraph(dsService.ctx)

	m := map[string]interface{}{
		"PulsarAddress":  Params.PulsarAddress,
		"ReceiveBufSize": 1024,
		"PulsarBufSize":  1024,
	}

	err := dsService.msFactory.SetParams(m)
	if err != nil {
		return err
	}

	// initialize flush manager for DataSync Service
	dsService.flushManager = NewRendezvousFlushManager(dsService.idAllocator, dsService.blobKV, dsService.replica, flushNotifyFunc(dsService))

	// recover segment checkpoints
	for _, us := range vchanInfo.GetUnflushedSegments() {
		if us.CollectionID != dsService.collectionID ||
			us.GetInsertChannel() != vchanInfo.ChannelName {
			log.Warn("Collection ID or ChannelName not compact",
				zap.Int64("Wanted ID", dsService.collectionID),
				zap.Int64("Actual ID", us.CollectionID),
				zap.String("Wanted Channel Name", vchanInfo.ChannelName),
				zap.String("Actual Channel Name", us.GetInsertChannel()),
			)
			continue
		}

		log.Info("Recover Segment NumOfRows form checkpoints",
			zap.String("InsertChannel", us.GetInsertChannel()),
			zap.Int64("SegmentID", us.GetID()),
			zap.Int64("NumOfRows", us.GetNumOfRows()),
		)

		if err := dsService.replica.addNormalSegment(us.GetID(), us.CollectionID, us.PartitionID, us.GetInsertChannel(), us.GetNumOfRows(), us.Statslogs, &segmentCheckPoint{us.GetNumOfRows(), *us.GetDmlPosition()}); err != nil {
			return err
		}
	}

	for _, fs := range vchanInfo.GetFlushedSegments() {
		if fs.CollectionID != dsService.collectionID ||
			fs.GetInsertChannel() != vchanInfo.ChannelName {
			log.Warn("Collection ID or ChannelName not compact",
				zap.Int64("Wanted ID", dsService.collectionID),
				zap.Int64("Actual ID", fs.CollectionID),
				zap.String("Wanted Channel Name", vchanInfo.ChannelName),
				zap.String("Actual Channel Name", fs.GetInsertChannel()),
			)
			continue
		}

		log.Info("Recover Segment NumOfRows form checkpoints",
			zap.String("InsertChannel", fs.GetInsertChannel()),
			zap.Int64("SegmentID", fs.GetID()),
			zap.Int64("NumOfRows", fs.GetNumOfRows()),
		)
		if err := dsService.replica.addFlushedSegment(fs.GetID(), fs.CollectionID,
			fs.PartitionID, fs.GetInsertChannel(), fs.GetNumOfRows(), fs.Statslogs); err != nil {
			return err
		}
	}

	c := &nodeConfig{
		msFactory:    dsService.msFactory,
		collectionID: vchanInfo.GetCollectionID(),
		vChannelName: vchanInfo.GetChannelName(),
		replica:      dsService.replica,
		allocator:    dsService.idAllocator,

		parallelConfig: newParallelConfig(),
	}

	var dmStreamNode Node
	dmStreamNode, err = newDmInputNode(dsService.ctx, vchanInfo.GetSeekPosition(), c)
	if err != nil {
		return err
	}

	var ddNode Node = newDDNode(dsService.ctx, dsService.collectionID, vchanInfo, dsService.msFactory)
	var insertBufferNode Node
	insertBufferNode, err = newInsertBufferNode(
		dsService.ctx,
		dsService.flushCh,
		dsService.flushManager,
		dsService.flushingSegCache,
		c,
	)
	if err != nil {
		return err
	}

	var deleteNode Node
	deleteNode, err = newDeleteNode(dsService.ctx, dsService.flushManager, dsService.clearSignal, c)
	if err != nil {
		return err
	}

	dsService.fg.AddNode(dmStreamNode)
	dsService.fg.AddNode(ddNode)
	dsService.fg.AddNode(insertBufferNode)
	dsService.fg.AddNode(deleteNode)

	// ddStreamNode
	err = dsService.fg.SetEdges(dmStreamNode.Name(),
		[]string{},
		[]string{ddNode.Name()},
	)
	if err != nil {
		log.Error("set edges failed in node", zap.String("name", dmStreamNode.Name()), zap.Error(err))
		return err
	}

	// ddNode
	err = dsService.fg.SetEdges(ddNode.Name(),
		[]string{dmStreamNode.Name()},
		[]string{insertBufferNode.Name()},
	)
	if err != nil {
		log.Error("set edges failed in node", zap.String("name", ddNode.Name()), zap.Error(err))
		return err
	}

	// insertBufferNode
	err = dsService.fg.SetEdges(insertBufferNode.Name(),
		[]string{ddNode.Name()},
		[]string{deleteNode.Name()},
	)
	if err != nil {
		log.Error("set edges failed in node", zap.String("name", insertBufferNode.Name()), zap.Error(err))
		return err
	}

	//deleteNode
	err = dsService.fg.SetEdges(deleteNode.Name(),
		[]string{insertBufferNode.Name()},
		[]string{},
	)
	if err != nil {
		log.Error("set edges failed in node", zap.String("name", deleteNode.Name()), zap.Error(err))
		return err
	}
	return nil
}
