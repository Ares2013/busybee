package storage

import (
	"fmt"
	"log"
	"math"
	"time"

	"github.com/deepfabric/beehive/pb/raftcmdpb"
	bhstorage "github.com/deepfabric/beehive/storage"
	bhutil "github.com/deepfabric/beehive/util"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/util/protoc"
)

const (
	countToClean     = uint64(4096)
	maxConsumerAlive = int64(7 * 24 * 60 * 60) // 7 day
)

type queueBatch struct {
	loaded                 bool
	maxOffset              uint64
	removedOffset          uint64
	queueKey               []byte
	consumerStartKey       []byte
	consumerEndKey         []byte
	maxAndCleanOffsetValue []byte
	maxAndCleanOffsetKey   []byte
	pairs                  [][]byte
	buf                    *goetty.ByteBuf
}

func newQueueBatch() batchType {
	return &queueBatch{
		maxAndCleanOffsetValue: make([]byte, 16, 16),
		buf:                    goetty.NewByteBuf(256),
	}
}

func (qb *queueBatch) support() []rpcpb.Type {
	return []rpcpb.Type{rpcpb.QueueAdd}
}

func (qb *queueBatch) addReq(req *raftcmdpb.Request, resp *raftcmdpb.Response, b *batch, buf *goetty.ByteBuf) {
	switch rpcpb.Type(req.CustemType) {
	case rpcpb.QueueAdd:
		msg := rpcpb.AcquireQueueAddRequest()
		protoc.MustUnmarshal(msg, req.Cmd)
		msg.Key = req.Key

		qb.add(msg, b)

		value := rpcpb.AcquireUint64Response()
		value.Value = qb.maxOffset
		resp.Value = protoc.MustMarshal(value)

		rpcpb.ReleaseQueueAddRequest(msg)
		rpcpb.ReleaseUint64Response(value)
	default:
		log.Fatalf("BUG: not supoprt rpctype: %d", rpcpb.Type(req.CustemType))
	}
}

func (qb *queueBatch) add(req *rpcpb.QueueAddRequest, b *batch) {
	if !qb.loaded {
		qb.queueKey = copyKey(req.Key, qb.buf)
		qb.consumerStartKey = consumerStartKey(req.Key, qb.buf)
		qb.consumerEndKey = consumerEndKey(req.Key, qb.buf)
		qb.maxAndCleanOffsetKey = maxAndCleanOffsetKey(req.Key, qb.buf)
		value, err := b.bs.getStore(b.shard).Get(qb.maxAndCleanOffsetKey)
		if err != nil {
			log.Fatalf("load max queue offset failed with %+v", err)
		}

		if len(value) > 0 {
			copy(qb.maxAndCleanOffsetValue, value)
			qb.maxOffset = goetty.Byte2UInt64(qb.maxAndCleanOffsetValue)
			qb.removedOffset = goetty.Byte2UInt64(qb.maxAndCleanOffsetValue[8:])
		}

		qb.loaded = true
	}

	for _, item := range req.Items {
		qb.maxOffset++
		qb.pairs = append(qb.pairs, itemKey(req.Key, qb.maxOffset, qb.buf), item)
	}
}

func (qb *queueBatch) exec(s bhstorage.DataStorage, wb bhutil.WriteBatch, b *batch) error {
	if len(qb.pairs)%2 != 0 {
		return fmt.Errorf("queue batch pairs len must pow of 2, but %d", len(qb.pairs))
	}

	if qb.maxOffset > 0 {
		// clean [last clean offset, minimum committed offset in all consumers]
		if qb.maxOffset-qb.removedOffset > countToClean {
			now := time.Now().Unix()
			low := uint64(math.MaxUint64)
			err := s.Scan(qb.consumerStartKey, qb.consumerEndKey, func(key, value []byte) (bool, error) {
				v := goetty.Byte2UInt64(value)
				ts := goetty.Byte2Int64(value[8:])

				if (now-ts < maxConsumerAlive) &&
					v < low {
					low = v
				}

				return true, nil
			}, false)
			if err != nil {
				log.Fatalf("exec queue add batch failed with %+v", err)
			}

			if low > qb.removedOffset {
				from := itemKey(qb.queueKey, qb.removedOffset, qb.buf)
				to := itemKey(qb.queueKey, low+1, qb.buf)
				err = s.RangeDelete(from, to)
				log.Fatalf("exec queue add batch failed with %+v", err)
				qb.removedOffset = low
			}
		}

		goetty.Uint64ToBytesTo(qb.maxOffset, qb.maxAndCleanOffsetValue)
		goetty.Uint64ToBytesTo(qb.removedOffset, qb.maxAndCleanOffsetValue[8:])
		wb.Set(qb.maxAndCleanOffsetKey, qb.maxAndCleanOffsetValue)
	}

	for i := 0; i < len(qb.pairs)/2; i++ {
		wb.Set(qb.pairs[2*i], qb.pairs[2*i+1])
	}

	return nil
}

func (qb *queueBatch) reset() {
	qb.loaded = false
	qb.maxOffset = 0
	qb.removedOffset = 0
	qb.queueKey = qb.queueKey[:0]
	qb.consumerStartKey = qb.consumerStartKey[:0]
	qb.consumerEndKey = qb.consumerEndKey[:0]
	qb.maxAndCleanOffsetKey = qb.maxAndCleanOffsetKey[:0]
	qb.maxAndCleanOffsetValue = qb.maxAndCleanOffsetValue[:0]
	qb.pairs = qb.pairs[:0]
	qb.buf.Clear()
}
