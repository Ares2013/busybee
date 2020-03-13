package core

import (
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/deepfabric/busybee/pkg/metric"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/util"
	bbutil "github.com/deepfabric/busybee/pkg/util"
	"github.com/fagongzi/goetty"
)

var (
	pool = sync.Pool{
		New: func() interface{} {
			return util.AcquireBitmap()
		},
	}
)

func acquireBM() *roaring.Bitmap {
	return pool.Get().(*roaring.Bitmap)
}

func releaseBM(value *roaring.Bitmap) {
	value.Clear()
	pool.Put(value)
}

type transaction struct {
	w *stateWorker

	totalCrowds *roaring.Bitmap
	stepCrowds  []*roaring.Bitmap

	err          error
	cbs          []*stepCB
	changes      []changedCtx
	crowdChanged bool
}

func newTransaction() *transaction {
	return &transaction{}
}

func (tran *transaction) start(w *stateWorker) {
	tran.w = w
	tran.totalCrowds = acquireBM()
	tran.totalCrowds.Or(w.totalCrowds)

	for _, crowd := range w.stepCrowds {
		v := acquireBM()
		v.Or(crowd)
		tran.stepCrowds = append(tran.stepCrowds, v)
	}
}

func (tran *transaction) doStepTimerEvent(item item) {
	idx := item.value.(int)
	logger.Infof("worker %s step timer %s", idx)
	if tran.err != nil {
		return
	}

	step := tran.w.state.States[idx]
	if step.Step.Execution.Type != metapb.Timer {
		return
	}

	ctx := newExprCtx(metapb.UserEvent{
		TenantID:   tran.w.state.TenantID,
		WorkflowID: tran.w.state.WorkflowID,
		InstanceID: tran.w.state.InstanceID,
	}, tran.w, idx)
	err := tran.w.steps[step.Step.Name].Execute(ctx, tran, who{})
	if err != nil {
		metric.IncWorkflowWorkerFailed()
		logger.Errorf("worker %s trigger timer failed with %+v",
			tran.w.key,
			err)
		tran.err = err
		return
	}

	// if crash or stopped while not store, the changed will not committed, it will retry later on other node
	tran.w.retryDo("store last trigger", tran, func(tran *transaction) error {
		return tran.w.eng.Storage().Set(timeStepLastTriggerKey(tran.w.state.WorkflowID,
			tran.w.state.InstanceID, step.Step.Name, tran.w.buf), goetty.Int64ToBytes(time.Now().Unix()))
	})
}

func (tran *transaction) doStepUserEvents(item item) {
	if item.cb != nil {
		tran.cbs = append(tran.cbs, item.cb)
	}
	if tran.err != nil {
		return
	}

	events := item.value.([]metapb.UserEvent)
	for _, event := range events {
		logger.Debugf("worker %s step event %+v", tran.w.key, event)

		for idx, crowd := range tran.stepCrowds {
			if crowd.Contains(event.UserID) {
				ctx := newExprCtx(event, tran.w, idx)
				err := tran.w.steps[tran.w.state.States[idx].Step.Name].Execute(ctx, tran, who{event.UserID, nil})
				if err != nil {
					metric.IncWorkflowWorkerFailed()
					logger.Errorf("worker %s step event %+v failed with %+v",
						tran.w.key,
						event,
						err)
					tran.err = err
					return
				}

				break
			}
		}
	}
}

func (tran *transaction) doUpdateCrowd(item item) {
	if item.cb != nil {
		tran.cbs = append(tran.cbs, item.cb)
	}

	if tran.err != nil {
		return
	}

	crowd := item.value.([]byte)
	newTotal := acquireBM()
	defer releaseBM(newTotal)
	bbutil.MustParseBMTo(crowd, newTotal)

	newAdded := acquireBM()
	defer releaseBM(newAdded)
	newAdded.Or(newTotal)
	newAdded.AndNot(tran.totalCrowds)

	tran.totalCrowds.Clear()
	tran.totalCrowds.Or(newTotal)

	for idx, sc := range tran.stepCrowds {
		if idx == 0 {
			sc.Or(newAdded)
		}
		sc.And(newTotal)
	}

	tran.crowdChanged = true
}

// this function will called by every step exectuion, if the target crowd or user
// removed to other step.
func (tran *transaction) stepChanged(ctx changedCtx) {
	// the users is the timer step to filter a crowd on the all workflow crowd,
	// so it's contains other shards crowds.
	if ctx.who.users != nil {
		// filter other shard state crowds
		ctx.who.users.And(tran.totalCrowds)
	}

	for idx := range tran.w.state.States {
		changed := false
		if tran.w.state.States[idx].Step.Name == ctx.from {
			changed = tran.removeFromStep(idx, ctx.who)
		} else if tran.w.state.States[idx].Step.Name == ctx.to {
			changed = tran.moveToStep(idx, ctx.who)
			ctx.ttl = tran.w.state.States[idx].Step.TTL
			if changed {
				tran.addChanged(ctx)
				tran.maybeTriggerDirectSteps(idx, ctx)
			}
		}
	}
}

func (tran *transaction) maybeTriggerDirectSteps(idx int, ctx changedCtx) {
	if !tran.w.isDirectStep(ctx.to) {
		return
	}

	from := ctx.to
	to := tran.w.directSteps[from]
	for {
		tran.addChanged(changedCtx{from, to, ctx.who, 0})

		if !tran.w.isDirectStep(to) {
			break
		}
		from = to
		to = tran.w.directSteps[from]
	}

	tran.removeFromStep(idx, ctx.who)
	for idx := range tran.w.state.States {
		if tran.w.state.States[idx].Step.Name == to {
			tran.moveToStep(idx, ctx.who)
			return
		}
	}
}

func (tran *transaction) removeFromStep(idx int, target who) bool {
	changed := false
	if nil != target.users {
		afterChanged := bbutil.BMAndnot(tran.stepCrowds[idx], target.users)
		if tran.stepCrowds[idx].GetCardinality() == afterChanged.GetCardinality() {
			changed = false
		} else {
			tran.stepCrowds[idx] = afterChanged
		}
	} else {
		tran.stepCrowds[idx].Remove(target.user)
	}
	return changed
}

func (tran *transaction) moveToStep(idx int, target who) bool {
	changed := true
	if nil != target.users {
		afterChanged := bbutil.BMOr(tran.stepCrowds[idx], target.users)
		if tran.stepCrowds[idx].GetCardinality() == afterChanged.GetCardinality() {
			changed = false
		} else {
			tran.stepCrowds[idx] = afterChanged
		}
	} else {
		tran.stepCrowds[idx].Add(target.user)
	}

	return changed
}

func (tran *transaction) addChanged(changed changedCtx) {
	for idx := range tran.changes {
		if tran.changes[idx].from == changed.from &&
			tran.changes[idx].to == changed.to {
			tran.changes[idx].add(changed)
			return
		}
	}

	ctx := changedCtx{changed.from, changed.to, who{0, acquireBM()}, changed.ttl}
	ctx.add(changed)
	tran.changes = append(tran.changes, ctx)
	tran.crowdChanged = true
}

func (tran *transaction) reset() {
	if tran.totalCrowds != nil {
		releaseBM(tran.totalCrowds)
	}

	if len(tran.changes) > 0 {
		for idx := range tran.stepCrowds {
			releaseBM(tran.stepCrowds[idx])
			tran.stepCrowds[idx] = nil
		}
		tran.stepCrowds = tran.stepCrowds[:0]
	}

	for idx := range tran.changes {
		releaseBM(tran.changes[idx].who.users)
		tran.changes[idx].who.users = nil
	}

	tran.changes = tran.changes[:0]
	tran.cbs = tran.cbs[:0]

	tran.err = nil
	tran.crowdChanged = false
}