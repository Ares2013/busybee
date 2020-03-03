package core

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	hbmetapb "github.com/deepfabric/beehive/pb/metapb"
	"github.com/deepfabric/beehive/util"
	"github.com/deepfabric/busybee/pkg/crm"
	"github.com/deepfabric/busybee/pkg/crowd"
	"github.com/deepfabric/busybee/pkg/metric"
	"github.com/deepfabric/busybee/pkg/notify"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/deepfabric/busybee/pkg/storage"
	bbutil "github.com/deepfabric/busybee/pkg/util"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/log"
	"github.com/fagongzi/util/protoc"
	"github.com/fagongzi/util/task"
	"github.com/fagongzi/util/uuid"
	"github.com/robfig/cron/v3"
)

var (
	emptyBMData = bytes.NewBuffer(nil)
	initBM      = roaring.NewBitmap()
	logger      log.Logger
)

func init() {
	initBM.WriteTo(emptyBMData)
	logger = log.NewLoggerWithPrefix("[engine]")
}

// Engine the engine maintains all state information
type Engine interface {
	// Start start the engine
	Start() error
	// Stop stop the engine
	Stop() error
	// StartInstance start instance, an instance may contain a lot of people,
	// so an instance will be divided into many shards, each shard handles some
	// people's events.
	StartInstance(workflow metapb.Workflow, loader metapb.BMLoader, crowd []byte, workers uint64) error
	// LastInstance returns last instance
	LastInstance(id uint64) (*metapb.WorkflowInstance, error)
	// HistoryInstance returens a workflow instance snapshot
	HistoryInstance(uint64, uint64) (*metapb.WorkflowInstanceSnapshot, error)
	// UpdateCrowd update instance crowd
	UpdateCrowd(id uint64, loader metapb.BMLoader, crowdMeta []byte) error
	// UpdateInstance update running workflow
	UpdateWorkflow(workflow metapb.Workflow) error
	// StopInstance stop instance
	StopInstance(id uint64) error
	// InstanceCountState returns instance count state
	InstanceCountState(id uint64) (metapb.InstanceCountState, error)
	// InstanceStepState returns instance step state
	InstanceStepState(id uint64, name string) (metapb.StepState, error)
	// TenantInit init tenant, and create the tenant input and output queue
	TenantInit(id, partitions uint64) error
	// Notifier returns notifier
	Notifier() notify.Notifier
	// Storage returns storage
	Storage() storage.Storage
	// Service returns a crm service
	Service() crm.Service
	// AddCronJob add cron job
	AddCronJob(string, func()) (cron.EntryID, error)
	// StopCronJob stop the cron job
	StopCronJob(cron.EntryID)
}

// NewEngine returns a engine
func NewEngine(store storage.Storage, notifier notify.Notifier, opts ...Option) (Engine, error) {
	eng := &engine{
		opts:                   &options{},
		store:                  store,
		notifier:               notifier,
		eventC:                 store.WatchEvent(),
		retryNewInstanceC:      make(chan *metapb.WorkflowInstance, 16),
		retryStoppingInstanceC: make(chan *metapb.WorkflowInstance, 16),
		retryCompleteInstanceC: make(chan uint64, 1024),
		stopInstanceC:          make(chan uint64, 1024),
		runner:                 task.NewRunner(),
		cronRunner:             cron.New(cron.WithSeconds()),
		service:                crm.NewService(store),
		loaders:                make(map[metapb.BMLoader]crowd.Loader),
	}

	for _, opt := range opts {
		opt(eng.opts)
	}

	eng.opts.adjust()
	return eng, nil
}

type engine struct {
	opts       *options
	store      storage.Storage
	service    crm.Service
	notifier   notify.Notifier
	runner     *task.Runner
	cronRunner *cron.Cron

	workers                sync.Map // key -> *worker
	eventC                 chan storage.Event
	retryNewInstanceC      chan *metapb.WorkflowInstance
	retryStoppingInstanceC chan *metapb.WorkflowInstance
	retryCompleteInstanceC chan uint64
	stopInstanceC          chan uint64

	loaders map[metapb.BMLoader]crowd.Loader
}

func (eng *engine) Start() error {
	eng.initBMLoaders()
	eng.initCron()
	eng.initEvent()

	err := eng.store.Start()
	if err != nil {
		return err
	}

	return nil
}

func (eng *engine) initCron() {
	eng.cronRunner.Start()
}

func (eng *engine) initEvent() {
	eng.runner.RunCancelableTask(eng.handleEvent)
}

func (eng *engine) Stop() error {
	eng.cronRunner.Stop()
	return eng.runner.Stop()
}

func (eng *engine) TenantInit(id, partitions uint64) error {
	err := eng.store.Set(storage.QueueMetadataKey(id, metapb.TenantInputGroup),
		goetty.Uint64ToBytes(partitions))
	if err != nil {
		return err
	}

	err = eng.store.Set(storage.QueueMetadataKey(id, metapb.TenantOutputGroup),
		goetty.Uint64ToBytes(1))
	if err != nil {
		return err
	}

	var shards []hbmetapb.Shard
	for i := uint64(0); i < partitions; i++ {
		shards = append(shards, hbmetapb.Shard{
			Group: uint64(metapb.TenantInputGroup),
			Start: storage.PartitionKey(id, i),
			End:   storage.PartitionKey(id, i+1),
		})
	}
	shards = append(shards, hbmetapb.Shard{
		Group: uint64(metapb.TenantOutputGroup),
		Start: storage.PartitionKey(id, 0),
		End:   storage.PartitionKey(id, 1),
	})

	return eng.store.RaftStore().AddShards(shards...)
}

func (eng *engine) StartInstance(workflow metapb.Workflow, loader metapb.BMLoader, crowdMeta []byte, workers uint64) error {
	if err := checkExcution(workflow); err != nil {
		return err
	}

	logger.Infof("workflow-%d start load bitmap crowd",
		workflow.ID)
	bm, err := eng.loadBM(loader, crowdMeta)
	if err != nil {
		logger.Errorf("start workflow-%d failed with %+v",
			workflow.ID,
			err)
		return err
	}

	old, err := eng.loadInstance(workflow.ID)
	if err != nil {
		return err
	}
	if old != nil {
		if old.State != metapb.Stopped {
			logger.Warningf("workflow-%d last instance is not stopped",
				workflow.ID)
			return nil
		}

		oldBM, err := eng.loadBM(old.Loader, old.LoaderMeta)
		if err != nil {
			logger.Errorf("start workflow-%d failed with %+v",
				workflow.ID,
				err)
			return err
		}

		bm.AndNot(oldBM)
		logger.Infof("start workflow-%d with new instance, crow changed to %d, workers %d",
			workflow.ID,
			bm.GetCardinality(),
			workers)
	} else {
		logger.Infof("start workflow-%d with first instance, crow %d, workers %d",
			workflow.ID,
			bm.GetCardinality(),
			workers)
	}

	id, err := eng.Storage().RaftStore().Prophet().GetRPC().AllocID()
	if err != nil {
		logger.Errorf("start workflow-%d failed with %+v",
			workflow.ID,
			err)
		return err
	}

	buf := goetty.NewByteBuf(32)
	defer buf.Release()
	key := storage.WorkflowCrowdShardKey(workflow.ID, id, 0, buf)

	loader, loaderMeta, err := eng.putBM(bm, key, 0)
	if err != nil {
		return err
	}

	instance := metapb.WorkflowInstance{
		Snapshot:   workflow,
		InstanceID: id,
		Loader:     loader,
		LoaderMeta: loaderMeta,
		TotalCrowd: bm.GetCardinality(),
		Workers:    workers,
	}

	req := rpcpb.AcquireStartingInstanceRequest()
	req.Instance = instance
	_, err = eng.store.ExecCommand(req)
	if err != nil {
		metric.IncStorageFailed()
		logger.Errorf("start workflow-%d failed with %+v",
			workflow.ID,
			err)
	}

	return err
}

func (eng *engine) LastInstance(id uint64) (*metapb.WorkflowInstance, error) {
	return eng.loadInstance(id)
}

func (eng *engine) HistoryInstance(wid uint64, instanceID uint64) (*metapb.WorkflowInstanceSnapshot, error) {
	buf := goetty.NewByteBuf(17)
	defer buf.Release()

	key := storage.WorkflowHistoryInstanceKey(wid, instanceID, buf)
	value, err := eng.store.Get(key)
	if err != nil {
		return nil, err
	}

	if len(value) == 0 {
		return nil, nil
	}

	v := &metapb.WorkflowInstanceSnapshot{}
	protoc.MustUnmarshal(v, value)
	return v, nil
}

func (eng *engine) StopInstance(id uint64) error {
	value, err := eng.store.Get(storage.WorkflowCurrentInstanceKey(id))
	if err != nil {
		return err
	}
	if len(value) == 0 {
		return nil
	}

	_, err = eng.store.ExecCommand(&rpcpb.StopInstanceRequest{
		WorkflowID: id,
	})
	return err
}

func (eng *engine) UpdateWorkflow(workflow metapb.Workflow) error {
	if err := checkExcution(workflow); err != nil {
		return err
	}

	instance, err := eng.loadRunningInstance(workflow.ID)
	if err != nil {
		return err
	}

	instance.Snapshot = workflow
	err = eng.doUpdateWorkflow(workflow)
	if err != nil {
		return err
	}

	return eng.store.PutToQueue(instance.Snapshot.TenantID, 0,
		metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
			Type: metapb.UpdateWorkflowType,
			UpdateWorkflow: &metapb.UpdateWorkflowEvent{
				Workflow: workflow,
			},
		}))
}

func (eng *engine) UpdateCrowd(id uint64, loader metapb.BMLoader, crowdMeta []byte) error {
	instance, err := eng.loadRunningInstance(id)
	if err != nil {
		return err
	}

	newBM, err := eng.loadBM(loader, crowdMeta)
	if err != nil {
		return err
	}

	workers, err := eng.getInstanceWorkers(instance)
	if err != nil {
		return err
	}
	var old []*roaring.Bitmap
	for _, worker := range workers {
		bm := bbutil.AcquireBitmap()
		for _, state := range worker.States {
			v, err := eng.loadBM(state.Loader, state.LoaderMeta)
			if err != nil {
				log.Fatalf("BUG: state crowd must success, using raw")
			}

			bm.Or(v)
		}
		old = append(old, bm)
	}
	bbutil.BMAlloc(newBM, old...)

	var events [][]byte
	for idx, bm := range old {
		events = append(events, protoc.MustMarshal(&metapb.Event{
			Type: metapb.UpdateCrowdType,
			UpdateCrowd: &metapb.UpdateCrowdEvent{
				WorkflowID: id,
				Index:      uint32(idx),
				Crowd:      bbutil.MustMarshalBM(bm),
			},
		}))
	}

	return eng.store.PutToQueue(instance.Snapshot.TenantID, 0,
		metapb.TenantInputGroup, events...)
}

func (eng *engine) InstanceCountState(id uint64) (metapb.InstanceCountState, error) {
	instance, err := eng.loadRunningInstance(id)
	if err != nil {
		return metapb.InstanceCountState{}, err
	}

	m := make(map[string]*metapb.CountState)
	state := metapb.InstanceCountState{}
	state.Total = instance.TotalCrowd
	state.Snapshot = instance.Snapshot
	for _, step := range instance.Snapshot.Steps {
		m[step.Name] = &metapb.CountState{
			Step:  step.Name,
			Count: 0,
		}
	}

	workers, err := eng.getInstanceWorkers(instance)
	if err != nil {
		return metapb.InstanceCountState{}, err
	}

	for _, stepState := range workers {
		for _, ss := range stepState.States {
			if _, ok := m[ss.Step.Name]; ok {
				m[ss.Step.Name].Count += ss.TotalCrowd
			}
		}
	}

	for _, v := range m {
		state.States = append(state.States, *v)
	}

	return state, nil
}

func (eng *engine) InstanceStepState(id uint64, name string) (metapb.StepState, error) {
	instance, err := eng.loadRunningInstance(id)
	if err != nil {
		return metapb.StepState{}, err
	}

	var target metapb.Step
	valueBM := bbutil.AcquireBitmap()
	shards, err := eng.getInstanceWorkers(instance)
	if err != nil {
		return metapb.StepState{}, err
	}

	for _, stepState := range shards {
		for _, ss := range stepState.States {
			if ss.Step.Name == name {
				target = ss.Step
				v, err := eng.loadBM(ss.Loader, ss.LoaderMeta)
				if err != nil {
					return metapb.StepState{}, err
				}
				valueBM = bbutil.BMOr(valueBM, v)
			}
		}
	}

	buf := goetty.NewByteBuf(32)
	defer buf.Release()

	key := storage.TempKey(uuid.NewV4().Bytes(), buf)
	loader, loaderMeta, err := eng.putBM(valueBM, key, eng.opts.tempKeyTTL)
	if err != nil {
		return metapb.StepState{}, err
	}

	return metapb.StepState{
		Step:       target,
		Loader:     loader,
		LoaderMeta: loaderMeta,
	}, nil
}

func (eng *engine) Notifier() notify.Notifier {
	return eng.notifier
}

func (eng *engine) Storage() storage.Storage {
	return eng.store
}

func (eng *engine) Service() crm.Service {
	return eng.service
}

func (eng *engine) AddCronJob(cronExpr string, fn func()) (cron.EntryID, error) {
	return eng.cronRunner.AddFunc(cronExpr, fn)
}

func (eng *engine) StopCronJob(id cron.EntryID) {
	eng.cronRunner.Remove(id)
}

func (eng *engine) doUpdateWorkflow(value metapb.Workflow) error {
	req := &rpcpb.UpdateWorkflowRequest{}
	req.Workflow = value
	_, err := eng.store.ExecCommand(req)
	return err
}

func (eng *engine) loadInstance(id uint64) (*metapb.WorkflowInstance, error) {
	value, err := eng.store.Get(storage.WorkflowCurrentInstanceKey(id))
	if err != nil {
		metric.IncStorageFailed()
		return nil, err
	}

	if len(value) == 0 {
		return nil, nil
	}

	instance := &metapb.WorkflowInstance{}
	protoc.MustUnmarshal(instance, value)
	return instance, nil
}

func (eng *engine) loadRunningInstance(id uint64) (*metapb.WorkflowInstance, error) {
	instance, err := eng.loadInstance(id)
	if err != nil {
		return nil, err
	}

	if instance == nil || instance.State != metapb.Running {
		return nil, fmt.Errorf("workflow-%d is not running",
			id)
	}

	return instance, nil
}

func (eng *engine) getInstanceWorkers(instance *metapb.WorkflowInstance) ([]metapb.WorkflowInstanceWorkerState, error) {
	from := storage.InstanceShardKey(instance.Snapshot.ID, 0)
	end := storage.InstanceShardKey(instance.Snapshot.ID, uint32(instance.Workers))

	var shards []metapb.WorkflowInstanceWorkerState
	for {
		values, err := eng.store.Scan(from, end, instance.Workers)
		if err != nil {
			return nil, err
		}

		if len(values) == 0 {
			break
		}

		for _, value := range values {
			shard := metapb.WorkflowInstanceWorkerState{}
			protoc.MustUnmarshal(&shard, value)
			shards = append(shards, shard)
		}

		from = storage.InstanceShardKey(instance.Snapshot.ID, shards[len(shards)-1].Index+1)
	}

	return shards, nil
}

func (eng *engine) buildSnapshot(instance *metapb.WorkflowInstance, buf *goetty.ByteBuf) (*metapb.WorkflowInstanceSnapshot, error) {
	shards, err := eng.getInstanceWorkers(instance)
	if err != nil {
		return nil, err
	}

	snapshot := &metapb.WorkflowInstanceSnapshot{
		Snapshot:  *instance,
		Timestamp: time.Now().Unix(),
	}
	snapshot.Snapshot.State = metapb.Stopped
	snapshot.Snapshot.StoppedAt = time.Now().Unix()

	value := make(map[string]*roaring.Bitmap)
	for _, shard := range shards {
		for _, state := range shard.States {
			v, err := eng.loadBM(state.Loader, state.LoaderMeta)
			if err != nil {
				return nil, err
			}

			if bm, ok := value[state.Step.Name]; ok {
				bm.Or(v)
			} else {
				value[state.Step.Name] = v
			}
		}
	}

	for _, step := range instance.Snapshot.Steps {
		key := storage.TempKey(uuid.NewV4().Bytes(), buf)

		loader, loadMeta, err := eng.putBM(value[step.Name], key, eng.opts.snapshotTTL)
		if err != nil {
			return nil, err
		}

		snapshot.States = append(snapshot.States, metapb.StepState{
			Step:       step,
			Loader:     loader,
			LoaderMeta: loadMeta,
		})
	}

	return snapshot, nil
}

func (eng *engine) handleEvent(ctx context.Context) {
	buf := goetty.NewByteBuf(32)
	defer buf.Release()

	for {
		buf.Clear()

		select {
		case <-ctx.Done():
			logger.Infof("handler instance task stopped")
			return
		case event, ok := <-eng.eventC:
			if ok {
				eng.doEvent(event, buf)
			}
		case instance, ok := <-eng.retryNewInstanceC:
			if ok {
				eng.doStartInstanceEvent(instance)
			}
		case instance, ok := <-eng.retryStoppingInstanceC:
			if ok {
				eng.doStoppingInstanceEvent(instance, buf)
			}
		case id, ok := <-eng.retryCompleteInstanceC:
			if ok {
				eng.doCreateInstanceStateShardComplete(id)
			}
		case id, ok := <-eng.stopInstanceC:
			if ok {
				eng.doStopInstance(id)
			}
		}
	}
}

func (eng *engine) doEvent(event storage.Event, buf *goetty.ByteBuf) {
	switch event.EventType {
	case storage.StartingInstanceEvent:
		eng.doStartInstanceEvent(event.Data.(*metapb.WorkflowInstance))
	case storage.RunningInstanceEvent:
		eng.doStartedInstanceEvent(event.Data.(*metapb.WorkflowInstance))
	case storage.StoppingInstanceEvent:
		eng.doStoppingInstanceEvent(event.Data.(*metapb.WorkflowInstance), buf)
	case storage.StoppedInstanceEvent:
		eng.doStoppedInstanceEvent(event.Data.(uint64))
	case storage.RunningInstanceWorkerEvent:
		eng.doStartInstanceStateEvent(event.Data.(metapb.WorkflowInstanceWorkerState))
	case storage.RemoveInstanceWorkerEvent:
		eng.doInstanceStateRemovedEvent(event.Data.(metapb.WorkflowInstanceWorkerState))
	}
}

func (eng *engine) doStartInstanceStateEvent(state metapb.WorkflowInstanceWorkerState) {
	logger.Infof("create workflow-%d worker %d",
		state.WorkflowID,
		state.Index)

	key := workerKey(state)
	if _, ok := eng.workers.Load(key); ok {
		logger.Fatalf("BUG: start a exists state worker")
	}

	now := time.Now().Unix()
	if state.StopAt != 0 && now >= state.StopAt {
		return
	}

	for {
		w, err := newStateWorker(key, state, eng)
		if err != nil {
			logger.Errorf("create worker %s failed with %+v",
				key,
				err)
			continue
		}

		eng.workers.Store(w.key, w)
		w.run()

		if state.StopAt != 0 {
			after := time.Second * time.Duration(state.StopAt-now)
			util.DefaultTimeoutWheel().Schedule(after, eng.stopWorker, w)
		}
		break
	}

}

func (eng *engine) stopWorker(arg interface{}) {
	w := arg.(*stateWorker)
	eng.workers.Delete(w.key)
	w.stop()
}

func (eng *engine) doInstanceStateRemovedEvent(state metapb.WorkflowInstanceWorkerState) {
	logger.Infof("stop workflow-%d shard %d, moved to the other node",
		state.WorkflowID,
		state.Index)

	key := workerKey(state)
	if w, ok := eng.workers.Load(key); ok {
		eng.workers.Delete(key)
		w.(*stateWorker).stop()
	}
}

func (eng *engine) doStartInstanceEvent(instance *metapb.WorkflowInstance) {
	logger.Infof("starting workflow-%d",
		instance.Snapshot.ID)

	var stopAt int64
	if instance.Snapshot.Duration > 0 {
		stopAt = time.Now().Add(time.Second * time.Duration(instance.Snapshot.Duration)).Unix()
	}

	bm, err := eng.loadBM(instance.Loader, instance.LoaderMeta)
	if err != nil {
		logger.Errorf("start workflow-%d failed with %+v, retry later",
			instance.Snapshot.ID, err)
		util.DefaultTimeoutWheel().Schedule(eng.opts.retryInterval,
			eng.addToRetryCompleteInstance, instance.Snapshot.ID)
		return
	}

	bms := bbutil.BMSplit(bm, instance.Workers)
	for index, bm := range bms {
		state := metapb.WorkflowInstanceWorkerState{}
		state.TenantID = instance.Snapshot.TenantID
		state.WorkflowID = instance.Snapshot.ID
		state.Index = uint32(index)
		state.StopAt = stopAt

		for _, step := range instance.Snapshot.Steps {
			state.States = append(state.States, metapb.StepState{
				Step:       step,
				TotalCrowd: 0,
				Loader:     metapb.RawLoader,
				LoaderMeta: emptyBMData.Bytes(),
			})
		}

		state.States[0].TotalCrowd = bm.GetCardinality()
		state.States[0].LoaderMeta = bbutil.MustMarshalBM(bm)
		if !eng.doCreateInstanceState(instance, state) {
			return
		}
	}

	eng.doCreateInstanceStateShardComplete(instance.Snapshot.ID)
}

func (eng *engine) doStartedInstanceEvent(instance *metapb.WorkflowInstance) {
	if instance.Snapshot.Duration == 0 {
		logger.Infof("workflow-%d started",
			instance.Snapshot.ID)
		return
	}

	now := time.Now().Unix()
	after := instance.Snapshot.Duration - (now - instance.StartedAt)
	if after <= 0 {
		eng.addToInstanceStop(instance.Snapshot.ID)
		return
	}

	util.DefaultTimeoutWheel().Schedule(time.Second*time.Duration(after), eng.addToInstanceStop, instance.Snapshot.ID)
	logger.Infof("workflow-%d started, duration %d seconds",
		instance.Snapshot.ID,
		after)
}

func (eng *engine) doStoppingInstanceEvent(instance *metapb.WorkflowInstance, buf *goetty.ByteBuf) {
	logger.Infof("stopping workflow-%d and %d workers",
		instance.Snapshot.ID,
		instance.Workers)

	err := eng.doSaveSnapshot(instance, buf)
	if err != nil {
		eng.doRetryStoppingInstance(instance, err)
		return
	}

	logger.Infof("workflow-%d history added with instance id %d",
		instance.Snapshot.ID,
		instance.InstanceID)

	n := uint32(instance.Workers)
	for index := uint32(0); index < n; index++ {
		req := rpcpb.AcquireRemoveInstanceStateShardRequest()
		req.WorkflowID = instance.Snapshot.ID
		req.Index = index
		_, err := eng.store.ExecCommand(req)
		if err != nil {
			eng.doRetryStoppingInstance(instance, err)
			return
		}
	}

	eng.doInstanceStoppingComplete(instance)
}

func (eng *engine) doInstanceStoppingComplete(instance *metapb.WorkflowInstance) {
	_, err := eng.store.ExecCommand(&rpcpb.StoppedInstanceRequest{
		WorkflowID: instance.Snapshot.ID,
	})
	if err != nil {
		eng.doRetryStoppingInstance(instance, err)
		return
	}

	logger.Infof("workflow-%d stopped", instance.Snapshot.ID)

	go eng.doRemoveShardBitmaps(instance)
}

func (eng *engine) doRetryStoppingInstance(instance *metapb.WorkflowInstance, err error) {
	metric.IncStorageFailed()
	logger.Errorf("stopping workflow-%d failed with %+v, retry later",
		instance.Snapshot.ID,
		err)
	util.DefaultTimeoutWheel().Schedule(eng.opts.retryInterval,
		eng.addToRetryStoppingInstance,
		instance)
}

func (eng *engine) doSaveSnapshot(instance *metapb.WorkflowInstance, buf *goetty.ByteBuf) error {
	key := storage.WorkflowHistoryInstanceKey(instance.Snapshot.ID, instance.InstanceID, buf)
	value, err := eng.store.Get(key)
	if err != nil {
		return err
	}

	if len(value) > 0 {
		return nil
	}

	snapshot, err := eng.buildSnapshot(instance, buf)
	if err != nil {
		return err
	}

	return eng.store.SetWithTTL(key, protoc.MustMarshal(snapshot), int64(eng.opts.snapshotTTL))
}

func (eng *engine) doStoppedInstanceEvent(id uint64) {
	var removed []interface{}
	eng.workers.Range(func(key, value interface{}) bool {
		w := value.(*stateWorker)
		if w.state.WorkflowID == id {
			removed = append(removed, key)
		}
		return true
	})

	for _, key := range removed {
		if w, ok := eng.workers.Load(key); ok {
			eng.stopWorker(w)
		}
	}

	logger.Infof("workflow-%d stopped",
		id)
}

func (eng *engine) doCreateInstanceState(instance *metapb.WorkflowInstance, state metapb.WorkflowInstanceWorkerState) bool {
	_, err := eng.store.ExecCommand(&rpcpb.CreateInstanceStateShardRequest{
		State: state,
	})
	if err != nil {
		metric.IncStorageFailed()
		logger.Errorf("create workflow-%d state %s failed with %+v, retry later",
			instance.Snapshot.ID,
			workerKey(state),
			err)
		util.DefaultTimeoutWheel().Schedule(eng.opts.retryInterval, eng.addToRetryNewInstance, instance)
		return false
	}

	return true
}

func (eng *engine) doCreateInstanceStateShardComplete(id uint64) {
	_, err := eng.store.ExecCommand(&rpcpb.StartedInstanceRequest{
		WorkflowID: id,
	})
	if err != nil {
		metric.IncStorageFailed()
		logger.Errorf("start workflow-%d state failed with %+v, retry later",
			id, err)
		util.DefaultTimeoutWheel().Schedule(eng.opts.retryInterval, eng.addToRetryCompleteInstance, id)
		return
	}
}

func (eng *engine) doStopInstance(id uint64) {
	_, err := eng.store.ExecCommand(&rpcpb.StopInstanceRequest{
		WorkflowID: id,
	})
	if err != nil {
		metric.IncStorageFailed()
		logger.Errorf("stopping workflow-%d failed with %+v, retry later",
			id, err)
		util.DefaultTimeoutWheel().Schedule(eng.opts.retryInterval, eng.addToInstanceStop, id)
		return
	}
}

func (eng *engine) doRemoveShardBitmaps(instance *metapb.WorkflowInstance) {
	switch instance.Loader {
	case metapb.KVLoader:
		return
	case metapb.RawLoader:
		return
	case metapb.KVShardLoader:
		meta := &metapb.ShardBitmapLoadMeta{}
		protoc.MustUnmarshal(meta, instance.LoaderMeta)
		logger.Infof("start remove workflow-%d/%d shard bitmaps",
			instance.Snapshot.ID,
			instance.InstanceID)

		buf := goetty.NewByteBuf(32)
		defer buf.Release()

		for i := uint32(0); i < meta.Shards; i++ {
			buf.Clear()
			err := eng.store.Delete(storage.ShardBitmapKey(meta.Key, i, buf))
			if err != nil {
				logger.Errorf("remove workflow shard bitmap workflow-%d/%d(%d) failed",
					instance.Snapshot.ID,
					instance.InstanceID,
					i)
				continue
			}

			logger.Errorf("remove workflow shard bitmap workflow-%d/%d(%d) completed",
				instance.Snapshot.ID,
				instance.InstanceID,
				i)
		}
	}
}

func (eng *engine) addToRetryNewInstance(arg interface{}) {
	eng.retryNewInstanceC <- arg.(*metapb.WorkflowInstance)
}

func (eng *engine) addToRetryStoppingInstance(arg interface{}) {
	eng.retryStoppingInstanceC <- arg.(*metapb.WorkflowInstance)
}

func (eng *engine) addToRetryCompleteInstance(arg interface{}) {
	eng.retryCompleteInstanceC <- arg.(uint64)
}

func (eng *engine) addToInstanceStop(id interface{}) {
	eng.stopInstanceC <- id.(uint64)
}

func workerKey(state metapb.WorkflowInstanceWorkerState) string {
	return fmt.Sprintf("%d/%d",
		state.WorkflowID,
		state.Index)
}
