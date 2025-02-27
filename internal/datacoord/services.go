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

package datacoord

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/milvus-io/milvus/internal/util/trace"

	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
	"github.com/milvus-io/milvus/internal/util/metricsinfo"
	"go.uber.org/zap"
)

// checks whether server in Healthy State
func (s *Server) isClosed() bool {
	return atomic.LoadInt64(&s.isServing) != ServerStateHealthy
}

// GetTimeTickChannel legacy API, returns time tick channel name
func (s *Server) GetTimeTickChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Value: Params.TimeTickChannelName,
	}, nil
}

// GetStatisticsChannel legacy API, returns statistics channel name
func (s *Server) GetStatisticsChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Value: Params.StatisticsChannelName,
	}, nil
}

// Flush notify segment to flush
// this api only guarantees all the segments requested is sealed
// these segments will be flushed only after the Flush policy is fulfilled
func (s *Server) Flush(ctx context.Context, req *datapb.FlushRequest) (*datapb.FlushResponse, error) {
	log.Debug("receive flush request", zap.Int64("dbID", req.GetDbID()), zap.Int64("collectionID", req.GetCollectionID()))
	sp, ctx := trace.StartSpanFromContextWithOperationName(ctx, "DataCoord-Flush")
	defer sp.Finish()
	resp := &datapb.FlushResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    "",
		},
		DbID:         0,
		CollectionID: 0,
		SegmentIDs:   []int64{},
	}
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}
	sealedSegments, err := s.segmentManager.SealAllSegments(ctx, req.CollectionID)
	if err != nil {
		resp.Status.Reason = fmt.Sprintf("failed to flush %d, %s", req.CollectionID, err)
		return resp, nil
	}
	log.Debug("flush response with segments",
		zap.Int64("collectionID", req.GetCollectionID()),
		zap.Any("segments", sealedSegments))
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.DbID = req.GetDbID()
	resp.CollectionID = req.GetCollectionID()
	resp.SegmentIDs = sealedSegments
	return resp, nil
}

// AssignSegmentID applies for segment ids and make allocation for records
func (s *Server) AssignSegmentID(ctx context.Context, req *datapb.AssignSegmentIDRequest) (*datapb.AssignSegmentIDResponse, error) {
	if s.isClosed() {
		return &datapb.AssignSegmentIDResponse{
			Status: &commonpb.Status{
				Reason:    serverNotServingErrMsg,
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
			},
		}, nil
	}

	assigns := make([]*datapb.SegmentIDAssignment, 0, len(req.SegmentIDRequests))

	for _, r := range req.SegmentIDRequests {
		log.Debug("handle assign segment request",
			zap.Int64("collectionID", r.GetCollectionID()),
			zap.Int64("partitionID", r.GetPartitionID()),
			zap.String("channelName", r.GetChannelName()),
			zap.Uint32("count", r.GetCount()))

		if coll := s.GetCollection(ctx, r.CollectionID); coll == nil {
			continue
		}

		s.cluster.Watch(r.ChannelName, r.CollectionID)

		allocations, err := s.segmentManager.AllocSegment(ctx,
			r.CollectionID, r.PartitionID, r.ChannelName, int64(r.Count))
		if err != nil {
			log.Warn("failed to alloc segment", zap.Any("request", r), zap.Error(err))
			continue
		}

		log.Debug("Assign segment success", zap.Any("assignments", allocations))

		for _, allocation := range allocations {
			result := &datapb.SegmentIDAssignment{
				SegID:        allocation.SegmentID,
				ChannelName:  r.ChannelName,
				Count:        uint32(allocation.NumOfRows),
				CollectionID: r.CollectionID,
				PartitionID:  r.PartitionID,
				ExpireTime:   allocation.ExpireTime,
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_Success,
					Reason:    "",
				},
			}
			assigns = append(assigns, result)
		}
	}
	return &datapb.AssignSegmentIDResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		SegIDAssignments: assigns,
	}, nil
}

// GetSegmentStates returns segments state
func (s *Server) GetSegmentStates(ctx context.Context, req *datapb.GetSegmentStatesRequest) (*datapb.GetSegmentStatesResponse, error) {
	resp := &datapb.GetSegmentStatesResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}

	for _, segmentID := range req.SegmentIDs {
		state := &datapb.SegmentStateInfo{
			Status:    &commonpb.Status{},
			SegmentID: segmentID,
		}
		segmentInfo := s.meta.GetSegment(segmentID)
		if segmentInfo == nil {
			state.Status.ErrorCode = commonpb.ErrorCode_UnexpectedError
			state.Status.Reason = fmt.Sprintf("failed to get segment %d", segmentID)
		} else {
			state.Status.ErrorCode = commonpb.ErrorCode_Success
			state.State = segmentInfo.GetState()
			state.StartPosition = segmentInfo.GetStartPosition()
		}
		resp.States = append(resp.States, state)
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success

	return resp, nil
}

// GetInsertBinlogPaths returns binlog paths info for requested segments
func (s *Server) GetInsertBinlogPaths(ctx context.Context, req *datapb.GetInsertBinlogPathsRequest) (*datapb.GetInsertBinlogPathsResponse, error) {
	resp := &datapb.GetInsertBinlogPathsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}
	segment := s.meta.GetSegment(req.GetSegmentID())
	if segment == nil {
		resp.Status.Reason = "segment not found"
		return resp, nil
	}
	binlogs := segment.GetBinlogs()
	fids := make([]UniqueID, 0, len(binlogs))
	paths := make([]*internalpb.StringList, 0, len(binlogs))
	for _, field := range binlogs {
		fids = append(fids, field.GetFieldID())
		paths = append(paths, &internalpb.StringList{Values: field.GetBinlogs()})
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.FieldIDs = fids
	resp.Paths = paths
	return resp, nil
}

// GetCollectionStatistics returns statistics for collection
// for now only row count is returned
func (s *Server) GetCollectionStatistics(ctx context.Context, req *datapb.GetCollectionStatisticsRequest) (*datapb.GetCollectionStatisticsResponse, error) {
	resp := &datapb.GetCollectionStatisticsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}
	nums := s.meta.GetNumRowsOfCollection(req.CollectionID)
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.Stats = append(resp.Stats, &commonpb.KeyValuePair{Key: "row_count", Value: strconv.FormatInt(nums, 10)})
	return resp, nil
}

// GetPartitionStatistics return statistics for parition
// for now only row count is returned
func (s *Server) GetPartitionStatistics(ctx context.Context, req *datapb.GetPartitionStatisticsRequest) (*datapb.GetPartitionStatisticsResponse, error) {
	resp := &datapb.GetPartitionStatisticsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}
	nums := s.meta.GetNumRowsOfPartition(req.CollectionID, req.PartitionID)
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.Stats = append(resp.Stats, &commonpb.KeyValuePair{Key: "row_count", Value: strconv.FormatInt(nums, 10)})
	return resp, nil
}

// GetSegmentInfoChannel legacy API, returns segment info statistics channel
func (s *Server) GetSegmentInfoChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Value: Params.SegmentInfoChannelName,
	}, nil
}

// GetSegmentInfo returns segment info requested, status, row count, etc included
func (s *Server) GetSegmentInfo(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
	resp := &datapb.GetSegmentInfoResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}
	infos := make([]*datapb.SegmentInfo, 0, len(req.SegmentIDs))
	for _, id := range req.SegmentIDs {
		info := s.meta.GetSegment(id)
		if info == nil {
			resp.Status.Reason = fmt.Sprintf("failed to get segment %d", id)
			return resp, nil
		}
		infos = append(infos, info.SegmentInfo)
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.Infos = infos
	return resp, nil
}

// SaveBinlogPaths update segment related binlog path
// works for Checkpoints and Flush
func (s *Server) SaveBinlogPaths(ctx context.Context, req *datapb.SaveBinlogPathsRequest) (*commonpb.Status, error) {
	resp := &commonpb.Status{ErrorCode: commonpb.ErrorCode_UnexpectedError}

	if s.isClosed() {
		resp.Reason = serverNotServingErrMsg
		return resp, nil
	}

	log.Debug("receive SaveBinlogPaths request",
		zap.Int64("collectionID", req.GetCollectionID()),
		zap.Int64("segmentID", req.GetSegmentID()),
		zap.Bool("isFlush", req.GetFlushed()),
		zap.Bool("isDropped", req.GetDropped()),
		zap.Any("checkpoints", req.GetCheckPoints()))

	// validate
	nodeID := req.GetBase().GetSourceID()
	segmentID := req.GetSegmentID()
	segment := s.meta.GetSegment(segmentID)

	if segment == nil {
		FailResponse(resp, fmt.Sprintf("failed to get segment %d", segmentID))
		log.Error("failed to get segment", zap.Int64("segmentID", segmentID))
		return resp, nil
	}

	channel := segment.GetInsertChannel()
	if !s.channelManager.Match(nodeID, channel) {
		FailResponse(resp, fmt.Sprintf("channel %s is not watched on node %d", channel, nodeID))
		log.Warn("node is not matched with channel", zap.String("channel", channel), zap.Int64("nodeID", nodeID))
		return resp, nil
	}

	if req.GetDropped() {
		s.segmentManager.DropSegment(ctx, segment.GetID())
	}

	// set segment to SegmentState_Flushing and save binlogs and checkpoints
	err := s.meta.UpdateFlushSegmentsInfo(
		req.GetSegmentID(),
		req.GetFlushed(),
		req.GetDropped(),
		req.GetField2BinlogPaths(),
		req.GetField2StatslogPaths(),
		req.GetDeltalogs(),
		req.GetCheckPoints(),
		req.GetStartPositions())
	if err != nil {
		log.Error("save binlog and checkpoints failed",
			zap.Int64("segmentID", req.GetSegmentID()),
			zap.Error(err))
		resp.Reason = err.Error()
		return resp, nil
	}

	log.Debug("flush segment with meta", zap.Int64("id", req.SegmentID),
		zap.Any("meta", req.GetField2BinlogPaths()))

	if req.GetDropped() && s.checkShouldDropChannel(channel) {
		log.Debug("remove channel", zap.String("channel", channel))
		err = s.channelManager.RemoveChannel(channel)
		if err != nil {
			log.Warn("failed to remove channel", zap.String("channel", channel), zap.Error(err))
		}
		s.segmentManager.DropSegmentsOfChannel(ctx, channel)
	}

	if req.GetFlushed() {
		s.segmentManager.DropSegment(ctx, req.SegmentID)
		s.flushCh <- req.SegmentID

		if Params.EnableCompaction {
			cctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			defer cancel()

			tt, err := getTimetravelReverseTime(cctx, s.allocator)
			if err == nil {
				err = s.compactionTrigger.triggerSingleCompaction(segment.GetCollectionID(),
					segment.GetPartitionID(), segmentID, segment.GetInsertChannel(), tt)
				if err != nil {
					log.Warn("failed to trigger single compaction", zap.Int64("segmentID", segmentID))
				}
			}
		}
	}
	resp.ErrorCode = commonpb.ErrorCode_Success
	return resp, nil
}

func (s *Server) checkShouldDropChannel(channel string) bool {
	segments := s.meta.GetSegmentsByChannel(channel)
	for _, segment := range segments {
		if segment.GetStartPosition() != nil && // fitler empty segment
			// FIXME: we filter compaction generated segments
			// because datanode may not know the segment due to the network lag or
			// datacoord crash when handling CompleteCompaction.
			len(segment.CompactionFrom) == 0 &&
			segment.GetState() != commonpb.SegmentState_Dropped {
			return false
		}
	}
	return true
}

// GetComponentStates returns DataCoord's current state
func (s *Server) GetComponentStates(ctx context.Context) (*internalpb.ComponentStates, error) {
	resp := &internalpb.ComponentStates{
		State: &internalpb.ComponentInfo{
			NodeID:    Params.NodeID,
			Role:      "datacoord",
			StateCode: 0,
		},
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
			Reason:    "",
		},
	}
	state := atomic.LoadInt64(&s.isServing)
	switch state {
	case ServerStateInitializing:
		resp.State.StateCode = internalpb.StateCode_Initializing
	case ServerStateHealthy:
		resp.State.StateCode = internalpb.StateCode_Healthy
	default:
		resp.State.StateCode = internalpb.StateCode_Abnormal
	}
	return resp, nil
}

// GetRecoveryInfo get recovery info for segment
func (s *Server) GetRecoveryInfo(ctx context.Context, req *datapb.GetRecoveryInfoRequest) (*datapb.GetRecoveryInfoResponse, error) {
	collectionID := req.GetCollectionID()
	partitionID := req.GetPartitionID()
	log.Info("receive get recovery info request",
		zap.Int64("collectionID", collectionID),
		zap.Int64("partitionID", partitionID))
	resp := &datapb.GetRecoveryInfoResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}
	segmentIDs := s.meta.GetSegmentsIDOfPartition(collectionID, partitionID)
	segment2Binlogs := make(map[UniqueID][]*datapb.FieldBinlog)
	segment2StatsBinlogs := make(map[UniqueID][]*datapb.FieldBinlog)
	segment2DeltaBinlogs := make(map[UniqueID][]*datapb.DeltaLogInfo)
	segmentsNumOfRows := make(map[UniqueID]int64)

	flushedIDs := make(map[int64]struct{})
	for _, id := range segmentIDs {
		segment := s.meta.GetSegment(id)
		if segment == nil {
			errMsg := fmt.Sprintf("failed to get segment %d", id)
			log.Error(errMsg)
			resp.Status.Reason = errMsg
			return resp, nil
		}
		if segment.State != commonpb.SegmentState_Flushed && segment.State != commonpb.SegmentState_Flushing {
			continue
		}
		_, ok := flushedIDs[id]
		if !ok {
			flushedIDs[id] = struct{}{}
		}

		binlogs := segment.GetBinlogs()
		field2Binlog := make(map[UniqueID][]string)
		for _, field := range binlogs {
			field2Binlog[field.GetFieldID()] = append(field2Binlog[field.GetFieldID()], field.GetBinlogs()...)
		}

		for f, paths := range field2Binlog {
			fieldBinlogs := &datapb.FieldBinlog{
				FieldID: f,
				Binlogs: paths,
			}
			segment2Binlogs[id] = append(segment2Binlogs[id], fieldBinlogs)
		}

		segmentsNumOfRows[id] = segment.NumOfRows

		statsBinlogs := segment.GetStatslogs()
		field2StatsBinlog := make(map[UniqueID][]string)
		for _, field := range statsBinlogs {
			field2StatsBinlog[field.GetFieldID()] = append(field2StatsBinlog[field.GetFieldID()], field.GetBinlogs()...)
		}

		for f, paths := range field2StatsBinlog {
			fieldBinlogs := &datapb.FieldBinlog{
				FieldID: f,
				Binlogs: paths,
			}
			segment2StatsBinlogs[id] = append(segment2StatsBinlogs[id], fieldBinlogs)
		}

		segment2DeltaBinlogs[id] = append(segment2DeltaBinlogs[id], segment.GetDeltalogs()...)
	}

	binlogs := make([]*datapb.SegmentBinlogs, 0, len(segment2Binlogs))
	for segmentID := range flushedIDs {
		sbl := &datapb.SegmentBinlogs{
			SegmentID:    segmentID,
			NumOfRows:    segmentsNumOfRows[segmentID],
			FieldBinlogs: segment2Binlogs[segmentID],
			Statslogs:    segment2StatsBinlogs[segmentID],
			Deltalogs:    segment2DeltaBinlogs[segmentID],
		}
		binlogs = append(binlogs, sbl)
	}

	dresp, err := s.rootCoordClient.DescribeCollection(s.ctx, &milvuspb.DescribeCollectionRequest{
		Base: &commonpb.MsgBase{
			MsgType:  commonpb.MsgType_DescribeCollection,
			SourceID: Params.NodeID,
		},
		CollectionID: collectionID,
	})
	if err = VerifyResponse(dresp, err); err != nil {
		log.Error("get collection info from master failed",
			zap.Int64("collectionID", collectionID),
			zap.Error(err))

		resp.Status.Reason = err.Error()
		return resp, nil
	}

	channels := dresp.GetVirtualChannelNames()
	channelInfos := make([]*datapb.VchannelInfo, 0, len(channels))
	for _, c := range channels {
		channelInfo := s.GetVChanPositions(c, collectionID, partitionID)
		channelInfos = append(channelInfos, channelInfo)
		log.Debug("datacoord append channelInfo in GetRecoveryInfo",
			zap.Any("collectionID", collectionID),
			zap.Any("channelInfo", channelInfo),
		)
	}

	resp.Binlogs = binlogs
	resp.Channels = channelInfos
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	return resp, nil
}

// GetFlushedSegments returns all segment matches provided criterion and in State Flushed
// If requested partition id < 0, ignores the partition id filter
func (s *Server) GetFlushedSegments(ctx context.Context, req *datapb.GetFlushedSegmentsRequest) (*datapb.GetFlushedSegmentsResponse, error) {
	resp := &datapb.GetFlushedSegmentsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}
	collectionID := req.GetCollectionID()
	partitionID := req.GetPartitionID()
	log.Debug("GetFlushedSegment",
		zap.Int64("collectionID", collectionID),
		zap.Int64("partitionID", partitionID))
	if s.isClosed() {
		resp.Status.Reason = serverNotServingErrMsg
		return resp, nil
	}
	var segmentIDs []UniqueID
	if partitionID < 0 {
		segmentIDs = s.meta.GetSegmentsIDOfCollection(collectionID)
	} else {
		segmentIDs = s.meta.GetSegmentsIDOfPartition(collectionID, partitionID)
	}
	ret := make([]UniqueID, 0, len(segmentIDs))
	for _, id := range segmentIDs {
		s := s.meta.GetSegment(id)
		if s != nil && s.GetState() != commonpb.SegmentState_Flushed {
			continue
		}
		// if this segment == nil, we assume this segment has been compacted and flushed
		ret = append(ret, id)
	}
	resp.Segments = ret
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	return resp, nil
}

// GetMetrics returns DataCoord metrics info
// it may include SystemMetrics, Topology metrics, etc.
func (s *Server) GetMetrics(ctx context.Context, req *milvuspb.GetMetricsRequest) (*milvuspb.GetMetricsResponse, error) {
	log.Debug("DataCoord.GetMetrics",
		zap.Int64("node_id", Params.NodeID),
		zap.String("req", req.Request))

	if s.isClosed() {
		log.Warn("DataCoord.GetMetrics failed",
			zap.Int64("node_id", Params.NodeID),
			zap.String("req", req.Request),
			zap.Error(errDataCoordIsUnhealthy(Params.NodeID)))

		return &milvuspb.GetMetricsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msgDataCoordIsUnhealthy(Params.NodeID),
			},
			Response: "",
		}, nil
	}

	metricType, err := metricsinfo.ParseMetricType(req.Request)
	if err != nil {
		log.Warn("DataCoord.GetMetrics failed to parse metric type",
			zap.Int64("node_id", Params.NodeID),
			zap.String("req", req.Request),
			zap.Error(err))

		return &milvuspb.GetMetricsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    err.Error(),
			},
			Response: "",
		}, nil
	}

	log.Debug("DataCoord.GetMetrics",
		zap.String("metric_type", metricType))

	if metricType == metricsinfo.SystemInfoMetrics {
		ret, err := s.metricsCacheManager.GetSystemInfoMetrics()
		if err == nil && ret != nil {
			return ret, nil
		}
		log.Debug("failed to get system info metrics from cache, recompute instead",
			zap.Error(err))

		metrics, err := s.getSystemInfoMetrics(ctx, req)

		log.Debug("DataCoord.GetMetrics",
			zap.Int64("node_id", Params.NodeID),
			zap.String("req", req.Request),
			zap.String("metric_type", metricType),
			zap.Any("metrics", metrics), // TODO(dragondriver): necessary? may be very large
			zap.Error(err))

		s.metricsCacheManager.UpdateSystemInfoMetrics(metrics)

		return metrics, err
	}

	log.Debug("DataCoord.GetMetrics failed, request metric type is not implemented yet",
		zap.Int64("node_id", Params.NodeID),
		zap.String("req", req.Request),
		zap.String("metric_type", metricType))

	return &milvuspb.GetMetricsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    metricsinfo.MsgUnimplementedMetric,
		},
		Response: "",
	}, nil
}

// CompleteCompaction completes a compaction with the result
func (s *Server) CompleteCompaction(ctx context.Context, req *datapb.CompactionResult) (*commonpb.Status, error) {
	log.Debug("receive complete compaction request", zap.Int64("planID", req.PlanID), zap.Int64("segmentID", req.GetSegmentID()))

	resp := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_UnexpectedError,
	}

	if s.isClosed() {
		log.Warn("failed to complete compaction", zap.Int64("planID", req.PlanID),
			zap.Error(errDataCoordIsUnhealthy(Params.NodeID)))

		resp.Reason = msgDataCoordIsUnhealthy(Params.NodeID)
		return resp, nil
	}

	if !Params.EnableCompaction {
		resp.Reason = "compaction disabled"
		return resp, nil
	}

	if err := s.compactionHandler.completeCompaction(req); err != nil {
		log.Error("failed to complete compaction", zap.Int64("planID", req.PlanID), zap.Error(err))
		resp.Reason = err.Error()
		return resp, nil
	}

	log.Debug("success to complete compaction", zap.Int64("planID", req.PlanID))
	resp.ErrorCode = commonpb.ErrorCode_Success
	return resp, nil
}

// ManualCompaction triggers a compaction for a collection
func (s *Server) ManualCompaction(ctx context.Context, req *milvuspb.ManualCompactionRequest) (*milvuspb.ManualCompactionResponse, error) {
	log.Debug("receive manual compaction", zap.Int64("collectionID", req.GetCollectionID()))

	resp := &milvuspb.ManualCompactionResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}

	if s.isClosed() {
		log.Warn("failed to execute manual compaction", zap.Int64("collectionID", req.GetCollectionID()),
			zap.Error(errDataCoordIsUnhealthy(Params.NodeID)))
		resp.Status.Reason = msgDataCoordIsUnhealthy(Params.NodeID)
		return resp, nil
	}

	if !Params.EnableCompaction {
		resp.Status.Reason = "compaction disabled"
		return resp, nil
	}

	id, err := s.compactionTrigger.forceTriggerCompaction(req.CollectionID, &timetravel{req.Timetravel})
	if err != nil {
		log.Error("failed to trigger manual compaction", zap.Int64("collectionID", req.GetCollectionID()), zap.Error(err))
		resp.Status.Reason = err.Error()
		return resp, nil
	}

	log.Debug("success to trigger manual compaction", zap.Int64("collectionID", req.GetCollectionID()), zap.Int64("compactionID", id))
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.CompactionID = id
	return resp, nil
}

// GetCompactionState gets the state of a compaction
func (s *Server) GetCompactionState(ctx context.Context, req *milvuspb.GetCompactionStateRequest) (*milvuspb.GetCompactionStateResponse, error) {
	log.Debug("receive get compaction state request", zap.Int64("compactionID", req.GetCompactionID()))
	resp := &milvuspb.GetCompactionStateResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}

	if s.isClosed() {
		log.Warn("failed to get compaction state", zap.Int64("compactionID", req.GetCompactionID()),
			zap.Error(errDataCoordIsUnhealthy(Params.NodeID)))
		resp.Status.Reason = msgDataCoordIsUnhealthy(Params.NodeID)
		return resp, nil
	}

	if !Params.EnableCompaction {
		resp.Status.Reason = "compaction disabled"
		return resp, nil
	}

	tasks := s.compactionHandler.getCompactionTasksBySignalID(req.GetCompactionID())
	state, executingCnt, completedCnt, timeoutCnt := getCompactionState(tasks)

	resp.State = state
	resp.ExecutingPlanNo = int64(executingCnt)
	resp.CompletedPlanNo = int64(completedCnt)
	resp.TimeoutPlanNo = int64(timeoutCnt)
	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	log.Debug("success to get compaction state", zap.Any("state", state), zap.Int("executing", executingCnt),
		zap.Int("completed", completedCnt), zap.Int("timeout", timeoutCnt))
	return resp, nil
}

func (s *Server) GetCompactionStateWithPlans(ctx context.Context, req *milvuspb.GetCompactionPlansRequest) (*milvuspb.GetCompactionPlansResponse, error) {
	log.Debug("received GetCompactionStateWithPlans request", zap.Int64("compactionID", req.GetCompactionID()))

	resp := &milvuspb.GetCompactionPlansResponse{
		Status: &commonpb.Status{ErrorCode: commonpb.ErrorCode_UnexpectedError},
	}

	if s.isClosed() {
		log.Warn("failed to get compaction state with plans", zap.Int64("compactionID", req.GetCompactionID()), zap.Error(errDataCoordIsUnhealthy(Params.NodeID)))
		resp.Status.Reason = msgDataCoordIsUnhealthy(Params.NodeID)
		return resp, nil
	}

	if !Params.EnableCompaction {
		resp.Status.Reason = "compaction disabled"
		return resp, nil
	}

	tasks := s.compactionHandler.getCompactionTasksBySignalID(req.GetCompactionID())
	for _, task := range tasks {
		resp.MergeInfos = append(resp.MergeInfos, getCompactionMergeInfo(task))
	}

	state, _, _, _ := getCompactionState(tasks)

	resp.Status.ErrorCode = commonpb.ErrorCode_Success
	resp.State = state
	log.Debug("success to get state with plans", zap.Any("state", state), zap.Any("merge infos", resp.MergeInfos))
	return resp, nil
}

func getCompactionMergeInfo(task *compactionTask) *milvuspb.CompactionMergeInfo {
	segments := task.plan.GetSegmentBinlogs()
	var sources []int64
	for _, s := range segments {
		sources = append(sources, s.GetSegmentID())
	}

	var target int64 = -1
	if task.result != nil {
		target = task.result.GetSegmentID()
	}

	return &milvuspb.CompactionMergeInfo{
		Sources: sources,
		Target:  target,
	}
}

func getCompactionState(tasks []*compactionTask) (state commonpb.CompactionState, executingCnt, completedCnt, timeoutCnt int) {
	for _, t := range tasks {
		switch t.state {
		case executing:
			executingCnt++
		case completed:
			completedCnt++
		case timeout:
			timeoutCnt++
		}
	}
	if executingCnt != 0 {
		state = commonpb.CompactionState_Executing
	} else {
		state = commonpb.CompactionState_Completed
	}
	return
}

func (s *Server) WatchChannels(ctx context.Context, req *datapb.WatchChannelsRequest) (*datapb.WatchChannelsResponse, error) {
	log.Debug("receive watch channels request", zap.Any("channels", req.GetChannelNames()))
	resp := &datapb.WatchChannelsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
		},
	}

	if s.isClosed() {
		log.Warn("failed to  watch channels request", zap.Any("channels", req.GetChannelNames()),
			zap.Error(errDataCoordIsUnhealthy(Params.NodeID)))
		resp.Status.Reason = msgDataCoordIsUnhealthy(Params.NodeID)
		return resp, nil
	}
	for _, channelName := range req.GetChannelNames() {
		ch := &channel{
			Name:         channelName,
			CollectionID: req.GetCollectionID(),
		}
		err := s.channelManager.Watch(ch)
		if err != nil {
			log.Warn("fail to watch channelName", zap.String("channelName", channelName), zap.Error(err))
			resp.Status.Reason = err.Error()
			return resp, nil
		}
	}
	resp.Status.ErrorCode = commonpb.ErrorCode_Success

	return resp, nil
}
