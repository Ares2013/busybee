package crowd

import (
	"fmt"

	"github.com/RoaringBitmap/roaring"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/deepfabric/busybee/pkg/storage"
	"github.com/deepfabric/busybee/pkg/util"
	"github.com/fagongzi/util/protoc"
)

const (
	limit uint64 = 80000
)

type kvLoader struct {
	store storage.Storage
}

// NewKVLoader returns a KV bitmap loader
func NewKVLoader(store storage.Storage) Loader {
	return &kvLoader{
		store: store,
	}
}

func (l *kvLoader) Get(key []byte) (*roaring.Bitmap, error) {
	bm := util.AcquireBitmap()
	resp := rpcpb.AcquireUint32SliceResponse()
	start := uint32(0)
	total := uint64(0)
	for {
		req := rpcpb.AcquireBMRangeRequest()
		req.Key = key
		req.Start = start
		req.Limit = limit
		value, err := l.store.ExecCommand(req)
		if err != nil {
			return nil, err
		}

		resp.Reset()
		protoc.MustUnmarshal(resp, value)

		if len(resp.Values) == 0 {
			break
		}

		total += uint64(len(resp.Values))
		bm.AddMany(resp.Values)
		start = bm.Maximum() + 1
	}

	rpcpb.ReleaseUint32SliceResponse(resp)

	if total < kb {
		logger.Infof("load %d crowd from KV with key<%s>, %d bytes",
			bm.GetCardinality(),
			string(key),
			total)
	} else if total < mb {
		logger.Infof("load %d crowd from KV with key<%s>, %d KB",
			bm.GetCardinality(),
			string(key),
			total/kb)
	} else {
		logger.Infof("load %d crowd from KV with key<%s>, %d MB",
			bm.GetCardinality(),
			string(key),
			total/mb)
	}

	return bm, nil
}

func (l *kvLoader) Set(key []byte, data []byte) (uint64, uint32, error) {
	return 0, 0, fmt.Errorf("KV loader not support Set")
}
