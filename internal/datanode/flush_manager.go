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
	"fmt"
	"path"
	"strconv"
	"sync"

	"github.com/milvus-io/milvus/internal/kv"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/etcdpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/retry"
	"go.uber.org/zap"
)

// flushManager defines a flush manager signature
type flushManager interface {
	// notify flush manager insert buffer data
	flushBufferData(data *BufferData, segmentID UniqueID, flushed bool, dropped bool, pos *internalpb.MsgPosition) error
	// notify flush manager del buffer data
	flushDelData(data *DelDataBuf, segmentID UniqueID, pos *internalpb.MsgPosition) error
	// injectFlush injects compaction or other blocking task before flush sync
	injectFlush(injection taskInjection, segments ...UniqueID)
	// close handles resource clean up
	close()
}

// segmentFlushPack contains result to save into meta
type segmentFlushPack struct {
	segmentID  UniqueID
	insertLogs map[UniqueID]string
	statsLogs  map[UniqueID]string
	deltaLogs  []*DelDataBuf
	pos        *internalpb.MsgPosition
	flushed    bool
	dropped    bool
	err        error // task execution error, if not nil, notify func should stop datanode
}

// notifyMetaFunc notify meta to persistent flush result
type notifyMetaFunc func(*segmentFlushPack)

// taskPostFunc clean up function after single flush task done
type taskPostFunc func(pack *segmentFlushPack, postInjection postInjectionFunc)

// postInjectionFunc post injection pack process logic
type postInjectionFunc func(pack *segmentFlushPack)

// make sure implementation
var _ flushManager = (*rendezvousFlushManager)(nil)

type orderFlushQueue struct {
	sync.Once
	segmentID UniqueID
	injectCh  chan taskInjection

	// MsgID => flushTask
	working    sync.Map
	notifyFunc notifyMetaFunc

	tailMut sync.Mutex
	tailCh  chan struct{}

	injectMut     sync.Mutex
	runningTasks  int32
	injectHandler *injectHandler
	postInjection postInjectionFunc
}

// newOrderFlushQueue creates a orderFlushQueue
func newOrderFlushQueue(segID UniqueID, f notifyMetaFunc) *orderFlushQueue {
	q := &orderFlushQueue{
		segmentID:  segID,
		notifyFunc: f,
		injectCh:   make(chan taskInjection, 100),
	}
	return q
}

// init orderFlushQueue use once protect init, init tailCh
func (q *orderFlushQueue) init() {
	q.Once.Do(func() {
		q.injectHandler = newInjectHandler(q)
		// new queue acts like tailing task is done
		q.tailCh = make(chan struct{})
		close(q.tailCh)
	})
}

func (q *orderFlushQueue) getFlushTaskRunner(pos *internalpb.MsgPosition) *flushTaskRunner {
	actual, loaded := q.working.LoadOrStore(string(pos.MsgID), newFlushTaskRunner(q.segmentID, q.injectCh))
	t := actual.(*flushTaskRunner)
	if !loaded {

		q.injectMut.Lock()
		q.runningTasks++
		if q.injectHandler != nil {
			q.injectHandler.close()
			q.injectHandler = nil
		}
		q.injectMut.Unlock()

		q.tailMut.Lock()
		t.init(q.notifyFunc, q.postTask, q.tailCh)
		q.tailCh = t.finishSignal
		q.tailMut.Unlock()
	}
	return t
}

func (q *orderFlushQueue) postTask(pack *segmentFlushPack, postInjection postInjectionFunc) {
	q.working.Delete(string(pack.pos.MsgID))
	q.injectMut.Lock()
	q.runningTasks--
	if q.runningTasks == 0 {
		q.injectHandler = newInjectHandler(q)
	}
	if postInjection != nil {
		q.postInjection = postInjection
	}

	if q.postInjection != nil {
		q.postInjection(pack)
	}
	q.injectMut.Unlock()
}

// enqueueInsertBuffer put insert buffer data into queue
func (q *orderFlushQueue) enqueueInsertFlush(task flushInsertTask, binlogs, statslogs map[UniqueID]string, flushed bool, dropped bool, pos *internalpb.MsgPosition) {
	q.getFlushTaskRunner(pos).runFlushInsert(task, binlogs, statslogs, flushed, dropped, pos)
}

// enqueueDelBuffer put delete buffer data into queue
func (q *orderFlushQueue) enqueueDelFlush(task flushDeleteTask, deltaLogs *DelDataBuf, pos *internalpb.MsgPosition) {
	q.getFlushTaskRunner(pos).runFlushDel(task, deltaLogs)
}

// inject performs injection for current task queue
// send into injectCh in there is running task
// or perform injection logic here if there is no injection
func (q *orderFlushQueue) inject(inject taskInjection) {
	q.injectCh <- inject
}

type injectHandler struct {
	once sync.Once
	wg   sync.WaitGroup
	done chan struct{}
}

func newInjectHandler(q *orderFlushQueue) *injectHandler {
	h := &injectHandler{
		done: make(chan struct{}),
	}
	h.wg.Add(1)
	go h.handleInjection(q)
	return h
}

func (h *injectHandler) handleInjection(q *orderFlushQueue) {
	defer h.wg.Done()
	for {
		select {
		case inject := <-q.injectCh:
			q.tailMut.Lock() //Maybe double check
			injectDone := make(chan struct{})
			q.tailCh = injectDone
			q.tailMut.Unlock()
			inject.injected <- struct{}{}
			<-inject.injectOver
			close(injectDone)
		case <-h.done:
			return
		}
	}
}

func (h *injectHandler) close() {
	h.once.Do(func() {
		close(h.done)
		h.wg.Wait()
	})
}

// rendezvousFlushManager makes sure insert & del buf all flushed
type rendezvousFlushManager struct {
	allocatorInterface
	kv.BaseKV
	Replica

	// segment id => flush queue
	dispatcher sync.Map
	notifyFunc notifyMetaFunc
}

// getFlushQueue
func (m *rendezvousFlushManager) getFlushQueue(segmentID UniqueID) *orderFlushQueue {
	newQueue := newOrderFlushQueue(segmentID, m.notifyFunc)
	actual, _ := m.dispatcher.LoadOrStore(segmentID, newQueue)
	// all operation on dispatcher is private, assertion ok guaranteed
	queue := actual.(*orderFlushQueue)
	queue.init()
	return queue
}

// notify flush manager insert buffer data
func (m *rendezvousFlushManager) flushBufferData(data *BufferData, segmentID UniqueID, flushed bool,
	dropped bool, pos *internalpb.MsgPosition) error {

	// empty flush
	if data == nil || data.buffer == nil {
		m.getFlushQueue(segmentID).enqueueInsertFlush(&flushBufferInsertTask{},
			map[UniqueID]string{}, map[UniqueID]string{}, flushed, dropped, pos)
		return nil
	}

	collID, partID, meta, err := m.getSegmentMeta(segmentID, pos)
	if err != nil {
		return err
	}

	// encode data and convert output data
	inCodec := storage.NewInsertCodec(meta)

	binLogs, statsBinlogs, err := inCodec.Serialize(partID, segmentID, data.buffer)
	if err != nil {
		return err
	}

	start, _, err := m.allocIDBatch(uint32(len(binLogs)))
	if err != nil {
		return err
	}

	field2Insert := make(map[UniqueID]string, len(binLogs))
	kvs := make(map[string]string, len(binLogs))
	paths := make([]string, 0, len(binLogs))
	field2Logidx := make(map[UniqueID]UniqueID, len(binLogs))
	for idx, blob := range binLogs {
		fieldID, err := strconv.ParseInt(blob.GetKey(), 10, 64)
		if err != nil {
			log.Error("Flush failed ... cannot parse string to fieldID ..", zap.Error(err))
			return err
		}

		logidx := start + int64(idx)

		// no error raise if alloc=false
		k, _ := m.genKey(false, collID, partID, segmentID, fieldID, logidx)

		key := path.Join(Params.InsertBinlogRootPath, k)
		paths = append(paths, key)
		kvs[key] = string(blob.Value[:])
		field2Insert[fieldID] = key
		field2Logidx[fieldID] = logidx
	}

	field2Stats := make(map[UniqueID]string)
	// write stats binlog
	for _, blob := range statsBinlogs {
		fieldID, err := strconv.ParseInt(blob.GetKey(), 10, 64)
		if err != nil {
			log.Error("Flush failed ... cannot parse string to fieldID ..", zap.Error(err))
			return err
		}

		logidx := field2Logidx[fieldID]

		// no error raise if alloc=false
		k, _ := m.genKey(false, collID, partID, segmentID, fieldID, logidx)

		key := path.Join(Params.StatsBinlogRootPath, k)
		kvs[key] = string(blob.Value[:])
		field2Stats[fieldID] = key
	}

	m.updateSegmentCheckPoint(segmentID)
	m.getFlushQueue(segmentID).enqueueInsertFlush(&flushBufferInsertTask{
		BaseKV: m.BaseKV,
		data:   kvs,
	}, field2Insert, field2Stats, flushed, dropped, pos)
	return nil
}

// notify flush manager del buffer data
func (m *rendezvousFlushManager) flushDelData(data *DelDataBuf, segmentID UniqueID,
	pos *internalpb.MsgPosition) error {

	// del signal with empty data
	if data == nil || data.delData == nil {
		m.getFlushQueue(segmentID).enqueueDelFlush(&flushBufferDeleteTask{}, nil, pos)
		return nil
	}

	collID, partID, err := m.getCollectionAndPartitionID(segmentID)
	if err != nil {
		return err
	}

	delCodec := storage.NewDeleteCodec()

	blob, err := delCodec.Serialize(collID, partID, segmentID, data.delData)
	if err != nil {
		return err
	}

	logID, err := m.allocID()
	if err != nil {
		log.Error("failed to alloc ID", zap.Error(err))
		return err
	}

	blobKey, _ := m.genKey(false, collID, partID, segmentID, logID)
	blobPath := path.Join(Params.DeleteBinlogRootPath, blobKey)
	kvs := map[string]string{blobPath: string(blob.Value[:])}
	data.fileSize = int64(len(blob.Value))
	data.filePath = blobPath
	log.Debug("delete blob path", zap.String("path", blobPath))

	m.getFlushQueue(segmentID).enqueueDelFlush(&flushBufferDeleteTask{
		BaseKV: m.BaseKV,
		data:   kvs,
	}, data, pos)
	return nil
}

// injectFlush inject process before task finishes
func (m *rendezvousFlushManager) injectFlush(injection taskInjection, segments ...UniqueID) {
	for _, segmentID := range segments {
		m.getFlushQueue(segmentID).inject(injection)
	}
}

// fetch meta info for segment
func (m *rendezvousFlushManager) getSegmentMeta(segmentID UniqueID, pos *internalpb.MsgPosition) (UniqueID, UniqueID, *etcdpb.CollectionMeta, error) {
	if !m.hasSegment(segmentID, true) {
		return -1, -1, nil, fmt.Errorf("No such segment %d in the replica", segmentID)
	}

	// fetch meta information of segment
	collID, partID, err := m.getCollectionAndPartitionID(segmentID)
	if err != nil {
		return -1, -1, nil, err
	}
	sch, err := m.getCollectionSchema(collID, pos.GetTimestamp())
	if err != nil {
		return -1, -1, nil, err
	}

	meta := &etcdpb.CollectionMeta{
		ID:     collID,
		Schema: sch,
	}
	return collID, partID, meta, nil
}

// close cleans up all the left members
func (m *rendezvousFlushManager) close() {
	m.dispatcher.Range(func(k, v interface{}) bool {
		//assertion ok
		queue := v.(*orderFlushQueue)
		queue.injectMut.Lock()
		if queue.injectHandler != nil {
			queue.injectHandler.close()
		}
		queue.injectMut.Unlock()
		return true
	})
}

type flushBufferInsertTask struct {
	kv.BaseKV
	data map[string]string
}

// flushInsertData implements flushInsertTask
func (t *flushBufferInsertTask) flushInsertData() error {
	if t.BaseKV != nil && len(t.data) > 0 {
		return t.MultiSave(t.data)
	}
	return nil
}

type flushBufferDeleteTask struct {
	kv.BaseKV
	data map[string]string
}

// flushDeleteData implements flushDeleteTask
func (t *flushBufferDeleteTask) flushDeleteData() error {
	if len(t.data) > 0 && t.BaseKV != nil {
		return t.MultiSave(t.data)
	}
	return nil
}

// NewRendezvousFlushManager create rendezvousFlushManager with provided allocator and kv
func NewRendezvousFlushManager(allocator allocatorInterface, kv kv.BaseKV, replica Replica, f notifyMetaFunc) *rendezvousFlushManager {
	return &rendezvousFlushManager{
		allocatorInterface: allocator,
		BaseKV:             kv,
		notifyFunc:         f,
		Replica:            replica,
	}
}

func flushNotifyFunc(dsService *dataSyncService, opts ...retry.Option) notifyMetaFunc {
	return func(pack *segmentFlushPack) {
		if pack.err != nil {
			log.Warn("flush pack with error, data node quit now")
			// TODO silverxia change to graceful stop datanode
			panic(pack.err)
		}
		fieldInsert := []*datapb.FieldBinlog{}
		fieldStats := []*datapb.FieldBinlog{}
		deltaInfos := []*datapb.DeltaLogInfo{}
		checkPoints := []*datapb.CheckPoint{}
		for k, v := range pack.insertLogs {
			fieldInsert = append(fieldInsert, &datapb.FieldBinlog{FieldID: k, Binlogs: []string{v}})
		}
		for k, v := range pack.statsLogs {
			fieldStats = append(fieldStats, &datapb.FieldBinlog{FieldID: k, Binlogs: []string{v}})
		}
		for _, delData := range pack.deltaLogs {
			deltaInfos = append(deltaInfos, &datapb.DeltaLogInfo{RecordEntries: uint64(delData.size), TimestampFrom: delData.tsFrom, TimestampTo: delData.tsTo, DeltaLogPath: delData.filePath, DeltaLogSize: delData.fileSize})
		}

		// only current segment checkpoint info,
		updates, _ := dsService.replica.getSegmentStatisticsUpdates(pack.segmentID)
		checkPoints = append(checkPoints, &datapb.CheckPoint{
			SegmentID: pack.segmentID,
			NumOfRows: updates.GetNumRows(),
			Position:  pack.pos,
		})

		log.Debug("SaveBinlogPath",
			zap.Int64("SegmentID", pack.segmentID),
			zap.Int64("CollectionID", dsService.collectionID),
			zap.Int("Length of Field2BinlogPaths", len(fieldInsert)),
			zap.Int("Length of Field2Stats", len(fieldStats)),
			zap.Int("Length of Field2Deltalogs", len(deltaInfos)),
		)

		req := &datapb.SaveBinlogPathsRequest{
			Base: &commonpb.MsgBase{
				MsgType:   0, //TODO msg type
				MsgID:     0, //TODO msg id
				Timestamp: 0, //TODO time stamp
				SourceID:  Params.NodeID,
			},
			SegmentID:           pack.segmentID,
			CollectionID:        dsService.collectionID,
			Field2BinlogPaths:   fieldInsert,
			Field2StatslogPaths: fieldStats,
			Deltalogs:           deltaInfos,

			CheckPoints: checkPoints,

			StartPositions: dsService.replica.listNewSegmentsStartPositions(),
			Flushed:        pack.flushed,
			Dropped:        pack.dropped,
		}
		err := retry.Do(context.Background(), func() error {
			rsp, err := dsService.dataCoord.SaveBinlogPaths(context.Background(), req)
			// should be network issue, return error and retry
			if err != nil {
				return fmt.Errorf(err.Error())
			}

			// TODO should retry only when datacoord status is unhealthy
			if rsp.ErrorCode != commonpb.ErrorCode_Success {
				return fmt.Errorf("data service save bin log path failed, reason = %s", rsp.Reason)
			}
			return nil
		}, opts...)
		if err != nil {
			log.Warn("failed to SaveBinlogPaths", zap.Error(err))
			// TODO change to graceful stop
			panic(err)
		}

		if pack.flushed || pack.dropped {
			dsService.replica.segmentFlushed(pack.segmentID)
		}
		dsService.flushingSegCache.Remove(req.GetSegmentID())
	}
}
