// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package querycoord

import (
	"context"
	"fmt"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"

	"github.com/milvus-io/milvus/internal/allocator"
	etcdkv "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/util/tsoutil"
)

func TestReloadFromKV(t *testing.T) {
	refreshParams()
	baseCtx, cancel := context.WithCancel(context.Background())
	kv, err := etcdkv.NewEtcdKV(Params.EtcdEndpoints, Params.MetaRootPath)
	assert.Nil(t, err)
	meta, err := newMeta(baseCtx, kv, nil, nil)
	assert.Nil(t, err)

	segmentInfo := &querypb.SegmentInfo{
		SegmentID:    defaultSegmentID,
		CollectionID: defaultCollectionID,
		PartitionID:  defaultPartitionID,
		SegmentState: querypb.SegmentState_sealed,
	}
	key := fmt.Sprintf("%s/%d/%d/%d", handoffSegmentPrefix, defaultCollectionID, defaultPartitionID, defaultSegmentID)
	value, err := proto.Marshal(segmentInfo)
	assert.Nil(t, err)
	err = kv.Save(key, string(value))
	assert.Nil(t, err)

	t.Run("Test_CollectionNotExist", func(t *testing.T) {
		indexChecker, err := newIndexChecker(baseCtx, kv, meta, nil, nil, nil, nil, nil)
		assert.Nil(t, err)
		assert.Equal(t, 0, len(indexChecker.handoffReqChan))
	})

	err = kv.Save(key, string(value))
	assert.Nil(t, err)

	meta.addCollection(defaultCollectionID, genCollectionSchema(defaultCollectionID, false))
	meta.setLoadType(defaultCollectionID, querypb.LoadType_LoadPartition)

	t.Run("Test_PartitionNotExist", func(t *testing.T) {
		indexChecker, err := newIndexChecker(baseCtx, kv, meta, nil, nil, nil, nil, nil)
		assert.Nil(t, err)
		assert.Equal(t, 0, len(indexChecker.handoffReqChan))
	})

	err = kv.Save(key, string(value))
	assert.Nil(t, err)
	meta.setLoadType(defaultCollectionID, querypb.LoadType_loadCollection)

	t.Run("Test_CollectionExist", func(t *testing.T) {
		indexChecker, err := newIndexChecker(baseCtx, kv, meta, nil, nil, nil, nil, nil)
		assert.Nil(t, err)
		for {
			if len(indexChecker.handoffReqChan) > 0 {
				break
			}
		}
	})

	cancel()
}

func TestCheckIndexLoop(t *testing.T) {
	refreshParams()
	ctx, cancel := context.WithCancel(context.Background())
	kv, err := etcdkv.NewEtcdKV(Params.EtcdEndpoints, Params.MetaRootPath)
	assert.Nil(t, err)
	meta, err := newMeta(ctx, kv, nil, nil)
	assert.Nil(t, err)

	rootCoord := newRootCoordMock()
	assert.Nil(t, err)
	indexCoord := newIndexCoordMock()
	indexCoord.returnIndexFile = true

	segmentInfo := &querypb.SegmentInfo{
		SegmentID:    defaultSegmentID,
		CollectionID: defaultCollectionID,
		PartitionID:  defaultPartitionID,
		SegmentState: querypb.SegmentState_sealed,
	}
	key := fmt.Sprintf("%s/%d/%d/%d", handoffSegmentPrefix, defaultCollectionID, defaultPartitionID, defaultSegmentID)
	value, err := proto.Marshal(segmentInfo)
	assert.Nil(t, err)

	t.Run("Test_ReqInValid", func(t *testing.T) {
		childCtx, childCancel := context.WithCancel(context.Background())
		indexChecker, err := newIndexChecker(childCtx, kv, meta, nil, nil, rootCoord, indexCoord, nil)
		assert.Nil(t, err)

		err = kv.Save(key, string(value))
		assert.Nil(t, err)
		indexChecker.enqueueHandoffReq(segmentInfo)
		indexChecker.wg.Add(1)
		go indexChecker.checkIndexLoop()
		for {
			_, err := kv.Load(key)
			if err != nil {
				break
			}
		}
		assert.Equal(t, 0, len(indexChecker.indexedSegmentsChan))
		childCancel()
		indexChecker.wg.Wait()
	})
	meta.addCollection(defaultCollectionID, genCollectionSchema(defaultCollectionID, false))
	meta.setLoadType(defaultCollectionID, querypb.LoadType_loadCollection)
	t.Run("Test_GetIndexInfo", func(t *testing.T) {
		childCtx, childCancel := context.WithCancel(context.Background())
		indexChecker, err := newIndexChecker(childCtx, kv, meta, nil, nil, rootCoord, indexCoord, nil)
		assert.Nil(t, err)

		indexChecker.enqueueHandoffReq(segmentInfo)
		indexChecker.wg.Add(1)
		go indexChecker.checkIndexLoop()
		for {
			if len(indexChecker.indexedSegmentsChan) > 0 {
				break
			}
		}
		childCancel()
		indexChecker.wg.Wait()
	})

	cancel()
}

func TestProcessHandoffAfterIndexDone(t *testing.T) {
	refreshParams()
	ctx, cancel := context.WithCancel(context.Background())
	kv, err := etcdkv.NewEtcdKV(Params.EtcdEndpoints, Params.MetaRootPath)
	assert.Nil(t, err)
	meta, err := newMeta(ctx, kv, nil, nil)
	assert.Nil(t, err)
	taskScheduler := &TaskScheduler{
		ctx:              ctx,
		cancel:           cancel,
		client:           kv,
		triggerTaskQueue: NewTaskQueue(),
	}
	idAllocatorKV, err := tsoutil.NewTSOKVBase(Params.EtcdEndpoints, Params.KvRootPath, "queryCoordTaskID")
	assert.Nil(t, err)
	idAllocator := allocator.NewGlobalIDAllocator("idTimestamp", idAllocatorKV)
	err = idAllocator.Initialize()
	assert.Nil(t, err)
	taskScheduler.taskIDAllocator = func() (UniqueID, error) {
		return idAllocator.AllocOne()
	}
	indexChecker, err := newIndexChecker(ctx, kv, meta, nil, taskScheduler, nil, nil, nil)
	assert.Nil(t, err)
	indexChecker.wg.Add(1)
	go indexChecker.processHandoffAfterIndexDone()

	segmentInfo := &querypb.SegmentInfo{
		SegmentID:    defaultSegmentID,
		CollectionID: defaultCollectionID,
		PartitionID:  defaultPartitionID,
		SegmentState: querypb.SegmentState_sealed,
	}
	key := fmt.Sprintf("%s/%d/%d/%d", handoffSegmentPrefix, defaultCollectionID, defaultPartitionID, defaultSegmentID)
	value, err := proto.Marshal(segmentInfo)
	assert.Nil(t, err)
	err = kv.Save(key, string(value))
	assert.Nil(t, err)
	indexChecker.enqueueIndexedSegment(segmentInfo)
	for {
		_, err := kv.Load(key)
		if err != nil {
			break
		}
	}
	assert.Equal(t, false, taskScheduler.triggerTaskQueue.taskEmpty())
	cancel()
	indexChecker.wg.Wait()
}
