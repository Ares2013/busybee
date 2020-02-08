package storage

import (
	"github.com/deepfabric/beehive/pb"
	"github.com/deepfabric/beehive/pb/raftcmdpb"
	bhstorage "github.com/deepfabric/beehive/storage"
	bhutil "github.com/deepfabric/beehive/util"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/log"
)

const (
	opAdd = iota
	opRemove
	opClear
	opDel
	opSet
)

type batchType interface {
	support() []rpcpb.Type
	addReq(*raftcmdpb.Request, *raftcmdpb.Response, *batch, *goetty.ByteBuf)
	exec(bhstorage.DataStorage, bhutil.WriteBatch, *batch) error
	reset()
}

type batch struct {
	shard        uint64
	writtenBytes uint64
	changedBytes int64
	bs           *beeStorage

	types []batchType
	fn    map[rpcpb.Type]batchType
}

func newBatch(bs *beeStorage, types ...batchType) *batch {
	b := &batch{
		bs:    bs,
		types: types,
		fn:    make(map[rpcpb.Type]batchType),
	}

	for _, tp := range types {
		for _, t := range tp.support() {
			b.fn[t] = tp
		}
	}

	return b
}

func (b *batch) Add(shard uint64, req *raftcmdpb.Request, buf *goetty.ByteBuf) (bool, *raftcmdpb.Response, error) {
	if b.shard != 0 && b.shard != shard {
		log.Fatalf("BUG: diffent shard opts in a batch, %d, %d",
			b.shard,
			shard)
	}

	b.shard = shard
	resp := pb.AcquireResponse()

	if tp, ok := b.fn[rpcpb.Type(req.CustemType)]; ok {
		tp.addReq(req, resp, b, buf)
		return true, resp, nil
	}

	return false, nil, nil
}

func (b *batch) get(key []byte) ([]byte, error) {
	return b.bs.getStore(b.shard).Get(key)
}

func (b *batch) Execute() (uint64, int64, error) {
	s := b.bs.getStore(b.shard)
	wb := s.NewWriteBatch()

	for _, tp := range b.types {
		err := tp.exec(s, wb, b)
		if err != nil {
			return 0, 0, err
		}
	}

	err := s.Write(wb, false)
	if err != nil {
		return 0, 0, err
	}

	return b.writtenBytes, b.changedBytes, err
}

func (b *batch) Reset() {
	b.shard = 0
	b.writtenBytes = 0
	b.changedBytes = 0

	for _, tp := range b.types {
		tp.reset()
	}
}
