// Copyright 2016 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/pd/pkg/core"
	"github.com/tikv/pd/pkg/core/constant"
)

const (
	// OperatorExpireTime is the duration that when an operator is not started
	// after it, the operator will be considered expired.
	OperatorExpireTime = 3 * time.Second
	cancelReason       = "cancel-reason"
)

// CancelReasonType is the type of cancel reason.
type CancelReasonType string

var (
	// RegionNotFound is the cancel reason when the region is not found.
	RegionNotFound CancelReasonType = "region not found"
	// EpochNotMatch is the cancel reason when the region epoch is not match.
	EpochNotMatch CancelReasonType = "epoch not match"
	// AlreadyExist is the cancel reason when the operator is running.
	AlreadyExist CancelReasonType = "already exist"
	// AdminStop is the cancel reason when the operator is stopped by admin.
	AdminStop CancelReasonType = "admin stop"
	// NotInRunningState is the cancel reason when the operator is not in running state.
	NotInRunningState CancelReasonType = "not in running state"
	// Timeout is the cancel reason when the operator is timeout.
	Timeout CancelReasonType = "timeout"
	// Expired is the cancel reason when the operator is expired.
	Expired CancelReasonType = "expired"
	// NotInCreateStatus is the cancel reason when the operator is not in create status.
	NotInCreateStatus CancelReasonType = "not in create status"
	// StaleStatus is the cancel reason when the operator is in a stale status.
	StaleStatus CancelReasonType = "stale status"
	// ExceedStoreLimit is the cancel reason when the operator exceeds the store limit.
	ExceedStoreLimit CancelReasonType = "exceed store limit"
	// ExceedWaitLimit is the cancel reason when the operator exceeds the waiting queue limit.
	ExceedWaitLimit CancelReasonType = "exceed wait limit"
	// RelatedMergeRegion is the cancel reason when the operator is cancelled by related merge region.
	RelatedMergeRegion CancelReasonType = "related merge region"
	// Unknown is the cancel reason when the operator is cancelled by an unknown reason.
	Unknown CancelReasonType = "unknown"
)

// Operator contains execution steps generated by scheduler.
// NOTE: This type is exported by HTTP API. Please pay more attention when modifying it.
type Operator struct {
	desc             string
	brief            string
	regionID         uint64
	regionEpoch      *metapb.RegionEpoch
	kind             OpKind
	steps            []OpStep
	stepsTime        []int64 // step finish time
	currentStep      int32
	status           OpStatusTracker
	level            constant.PriorityLevel
	Counters         []prometheus.Counter
	FinishedCounters []prometheus.Counter
	additionalInfos  opAdditionalInfo
	ApproximateSize  int64
	timeout          time.Duration
	influence        *OpInfluence
}

// NewOperator creates a new operator.
func NewOperator(desc, brief string, regionID uint64, regionEpoch *metapb.RegionEpoch, kind OpKind, approximateSize int64, steps ...OpStep) *Operator {
	level := constant.Medium
	if kind&OpAdmin != 0 {
		level = constant.Urgent
	}
	maxDuration := float64(0)
	for _, v := range steps {
		maxDuration += v.Timeout(approximateSize).Seconds()
	}
	return &Operator{
		desc:        desc,
		brief:       brief,
		regionID:    regionID,
		regionEpoch: regionEpoch,
		kind:        kind,
		steps:       steps,
		stepsTime:   make([]int64, len(steps)),
		status:      NewOpStatusTracker(),
		level:       level,
		additionalInfos: opAdditionalInfo{
			value: make(map[string]string),
		},
		ApproximateSize: approximateSize,
		timeout:         time.Duration(maxDuration) * time.Second,
	}
}

// Sync some attribute with the given timeout.
func (o *Operator) Sync(other *Operator) {
	o.timeout = other.timeout
	o.SetAdditionalInfo(string(RelatedMergeRegion), strconv.FormatUint(other.RegionID(), 10))
	other.SetAdditionalInfo(string(RelatedMergeRegion), strconv.FormatUint(o.RegionID(), 10))
}

func (o *Operator) String() string {
	stepStrs := make([]string, len(o.steps))
	for i := range o.steps {
		stepStrs[i] = fmt.Sprintf("%d:{%s}", i, o.steps[i].String())
	}
	s := fmt.Sprintf("%s {%s} (kind:%s, region:%v(%v, %v), createAt:%s, startAt:%s, currentStep:%v, size:%d, steps:[%s], timeout:[%s])",
		o.desc, o.brief, o.kind, o.regionID, o.regionEpoch.GetVersion(), o.regionEpoch.GetConfVer(), o.GetCreateTime(),
		o.GetStartTime(), atomic.LoadInt32(&o.currentStep), o.ApproximateSize, strings.Join(stepStrs, ", "), o.timeout.String())
	if o.CheckSuccess() {
		s += " finished"
	}
	if o.CheckTimeout() {
		s += " timeout"
	}
	return s
}

// Brief returns the operator's short brief.
func (o *Operator) Brief() string {
	return o.brief
}

// MarshalJSON serializes custom types to JSON.
func (o *Operator) MarshalJSON() ([]byte, error) {
	return []byte(`"` + o.String() + `"`), nil
}

// OpObject is used to return Operator as a json object for API.
type OpObject struct {
	Desc        string              `json:"desc"`
	Brief       string              `json:"brief"`
	RegionID    uint64              `json:"region_id"`
	RegionEpoch *metapb.RegionEpoch `json:"region_epoch"`
	Kind        OpKind              `json:"kind"`
	Timeout     string              `json:"timeout"`
	Status      OpStatus            `json:"status"`
}

// ToJSONObject serializes Operator as JSON object.
func (o *Operator) ToJSONObject() *OpObject {
	var status OpStatus
	if o.CheckSuccess() {
		status = SUCCESS
	} else if o.CheckTimeout() {
		status = TIMEOUT
	} else {
		status = o.Status()
	}

	return &OpObject{
		Desc:        o.desc,
		Brief:       o.brief,
		RegionID:    o.regionID,
		RegionEpoch: o.regionEpoch,
		Kind:        o.kind,
		Timeout:     o.timeout.String(),
		Status:      status,
	}
}

// Desc returns the operator's short description.
func (o *Operator) Desc() string {
	return o.desc
}

// SetDesc sets the description for the operator.
func (o *Operator) SetDesc(desc string) {
	o.desc = desc
}

// AttachKind attaches an operator kind for the operator.
func (o *Operator) AttachKind(kind OpKind) {
	o.kind |= kind
}

// RegionID returns the region that operator is targeted.
func (o *Operator) RegionID() uint64 {
	return o.regionID
}

// RegionEpoch returns the region's epoch that is attached to the operator.
func (o *Operator) RegionEpoch() *metapb.RegionEpoch {
	return o.regionEpoch
}

// Kind returns operator's kind.
func (o *Operator) Kind() OpKind {
	return o.kind
}

// SchedulerKind return the highest OpKind even if the operator has many OpKind
// fix #3778
func (o *Operator) SchedulerKind() OpKind {
	// LowBit ref: https://en.wikipedia.org/wiki/Find_first_set
	// 6(110) ==> 2(10)
	// 5(101) ==> 1(01)
	// 4(100) ==> 4(100)
	return o.kind & (-o.kind)
}

// Status returns operator status.
func (o *Operator) Status() OpStatus {
	return o.status.Status()
}

// SetStatusReachTime sets the reach time of the operator, only for test purpose.
func (o *Operator) SetStatusReachTime(st OpStatus, t time.Time) {
	o.status.setTime(st, t)
}

// CheckAndGetStatus returns operator status after `CheckExpired` and `CheckTimeout`.
func (o *Operator) CheckAndGetStatus() OpStatus {
	switch {
	case o.CheckExpired():
		return EXPIRED
	case o.CheckTimeout():
		return TIMEOUT
	default:
		return o.Status()
	}
}

// GetReachTimeOf returns the time when operator reaches the given status.
func (o *Operator) GetReachTimeOf(st OpStatus) time.Time {
	return o.status.ReachTimeOf(st)
}

// GetCreateTime gets the create time of operator.
func (o *Operator) GetCreateTime() time.Time {
	return o.status.ReachTimeOf(CREATED)
}

// ElapsedTime returns duration since it was created.
func (o *Operator) ElapsedTime() time.Duration {
	return time.Since(o.GetCreateTime())
}

// Start sets the operator to STARTED status, returns whether succeeded.
func (o *Operator) Start() bool {
	return o.status.To(STARTED)
}

// HasStarted returns whether operator has started.
func (o *Operator) HasStarted() bool {
	return !o.GetStartTime().IsZero()
}

// GetStartTime gets the start time of operator.
func (o *Operator) GetStartTime() time.Time {
	return o.status.ReachTimeOf(STARTED)
}

// RunningTime returns duration since it started.
func (o *Operator) RunningTime() time.Duration {
	if o.HasStarted() {
		return time.Since(o.GetStartTime())
	}
	return 0
}

// IsEnd checks if the operator is at and end status.
func (o *Operator) IsEnd() bool {
	return o.status.IsEnd()
}

// CheckSuccess checks if all steps are finished, and update the status.
func (o *Operator) CheckSuccess() bool {
	if atomic.LoadInt32(&o.currentStep) >= int32(len(o.steps)) {
		return o.status.To(SUCCESS) || o.Status() == SUCCESS
	}
	return false
}

// Cancel marks the operator canceled.
func (o *Operator) Cancel(reason ...CancelReasonType) bool {
	o.additionalInfos.Lock()
	defer o.additionalInfos.Unlock()
	if _, ok := o.additionalInfos.value[cancelReason]; !ok && len(reason) != 0 {
		o.additionalInfos.value[cancelReason] = string(reason[0])
	}
	return o.status.To(CANCELED)
}

// Replace marks the operator replaced.
func (o *Operator) Replace() bool {
	return o.status.To(REPLACED)
}

// CheckExpired checks if the operator is expired, and update the status.
func (o *Operator) CheckExpired() bool {
	return o.status.CheckExpired(OperatorExpireTime)
}

// CheckTimeout returns true if the operator is timeout, and update the status.
func (o *Operator) CheckTimeout() bool {
	if o.CheckSuccess() {
		return false
	}
	return o.status.CheckTimeout(o.timeout)
}

// Len returns the operator's steps count.
func (o *Operator) Len() int {
	return len(o.steps)
}

// Step returns the i-th step.
func (o *Operator) Step(i int) OpStep {
	if i >= 0 && i < len(o.steps) {
		return o.steps[i]
	}
	return nil
}

// ContainNonWitnessStep returns true if it contains the target OpStep
func (o *Operator) ContainNonWitnessStep() bool {
	for _, step := range o.steps {
		switch step.(type) {
		case BecomeNonWitness:
			return true
		default:
		}
	}
	return false
}

// getCurrentTimeAndStep returns the start time of the i-th step.
// opStep is nil if the i-th step is not found.
func (o *Operator) getCurrentTimeAndStep() (startTime time.Time, opStep OpStep) {
	startTime = o.GetStartTime()
	currentStep := atomic.LoadInt32(&o.currentStep)
	if int(currentStep) < len(o.steps) {
		opStep = o.steps[currentStep]
		// we should use the finished time of the previous step if the first step is finished.
		// the start time of the first step is the start time of the operator.
		if currentStep > 0 {
			startTime = time.Unix(0, atomic.LoadInt64(&(o.stepsTime[currentStep-1])))
		}
	}
	return
}

// Check checks if current step is finished, returns next step to take action.
// If operator is at an end status, check returns nil.
// It's safe to be called by multiple goroutine concurrently.
func (o *Operator) Check(region *core.RegionInfo) OpStep {
	if o.IsEnd() {
		return nil
	}
	// CheckTimeout will call CheckSuccess first
	defer func() { _ = o.CheckTimeout() }()
	for step := atomic.LoadInt32(&o.currentStep); int(step) < len(o.steps); step++ {
		if o.steps[int(step)].IsFinish(region) {
			current := time.Now()
			if atomic.CompareAndSwapInt64(&(o.stepsTime[step]), 0, current.UnixNano()) {
				startTime, _ := o.getCurrentTimeAndStep()
				operatorStepDuration.WithLabelValues(reflect.TypeOf(o.steps[int(step)]).Name()).
					Observe(current.Sub(startTime).Seconds())
			}
			atomic.StoreInt32(&o.currentStep, step+1)
		} else {
			return o.steps[int(step)]
		}
	}
	return nil
}

// ConfVerChanged returns the number of confver has consumed by steps
func (o *Operator) ConfVerChanged(region *core.RegionInfo) (total uint64) {
	current := atomic.LoadInt32(&o.currentStep)
	if current == int32(len(o.steps)) {
		current--
	}
	// including current step, it may has taken effects in this heartbeat
	for _, step := range o.steps[0 : current+1] {
		total += step.ConfVerChanged(region)
	}
	return total
}

// SetPriorityLevel sets the priority level for operator.
func (o *Operator) SetPriorityLevel(level constant.PriorityLevel) {
	o.level = level
}

// GetPriorityLevel gets the priority level.
func (o *Operator) GetPriorityLevel() constant.PriorityLevel {
	return o.level
}

// UnfinishedInfluence calculates the store difference which unfinished operator steps make.
func (o *Operator) UnfinishedInfluence(opInfluence OpInfluence, region *core.RegionInfo) {
	for step := atomic.LoadInt32(&o.currentStep); int(step) < len(o.steps); step++ {
		if !o.steps[int(step)].IsFinish(region) {
			o.steps[int(step)].Influence(opInfluence, region)
		}
	}
}

// TotalInfluence calculates the store difference which whole operator steps make.
func (o *Operator) TotalInfluence(opInfluence OpInfluence, region *core.RegionInfo) {
	// skip if region is nil and not cache influence.
	if region == nil && o.influence == nil {
		return
	}
	if o.influence == nil {
		o.influence = NewOpInfluence()
		for step := range o.steps {
			o.steps[step].Influence(*o.influence, region)
		}
	}
	opInfluence.Add(o.influence)
}

// OpHistory is used to log and visualize completed operators.
type OpHistory struct {
	FinishTime time.Time
	From, To   uint64
	Kind       constant.ResourceKind
}

// History transfers the operator's steps to operator histories.
func (o *Operator) History() []OpHistory {
	now := time.Now()
	var histories []OpHistory
	var addPeerStores, removePeerStores []uint64
	for _, step := range o.steps {
		switch s := step.(type) {
		case TransferLeader:
			histories = append(histories, OpHistory{
				FinishTime: now,
				From:       s.FromStore,
				To:         s.ToStore,
				Kind:       constant.LeaderKind,
			})
		case AddPeer:
			addPeerStores = append(addPeerStores, s.ToStore)
		case AddLearner:
			addPeerStores = append(addPeerStores, s.ToStore)
		case RemovePeer:
			removePeerStores = append(removePeerStores, s.FromStore)
		}
	}
	for i := range addPeerStores {
		if i < len(removePeerStores) {
			histories = append(histories, OpHistory{
				FinishTime: now,
				From:       removePeerStores[i],
				To:         addPeerStores[i],
				Kind:       constant.RegionKind,
			})
		}
	}
	return histories
}

// OpRecord is used to log and visualize completed operators.
// NOTE: This type is exported by HTTP API. Please pay more attention when modifying it.
type OpRecord struct {
	*Operator
	FinishTime time.Time
	duration   time.Duration
}

func (o *OpRecord) String() string {
	return fmt.Sprintf("%s (finishAt:%v, duration:%v)", o.Operator.String(), o.FinishTime, o.duration)
}

// MarshalJSON returns the status of operator as a JSON string
func (o *OpRecord) MarshalJSON() ([]byte, error) {
	return []byte(`"` + o.String() + `"`), nil
}

// Record transfers the operator to OpRecord.
func (o *Operator) Record(finishTime time.Time) *OpRecord {
	step := atomic.LoadInt32(&o.currentStep)
	record := &OpRecord{
		Operator:   o,
		FinishTime: finishTime,
	}
	start := o.GetStartTime()
	if o.Status() != SUCCESS && 0 < step && int(step-1) < len(o.stepsTime) {
		start = time.Unix(0, o.stepsTime[int(step-1)])
	}
	record.duration = finishTime.Sub(start)
	return record
}

// IsLeaveJointStateOperator returns true if the desc is OpDescLeaveJointState.
func (o *Operator) IsLeaveJointStateOperator() bool {
	return strings.EqualFold(o.desc, OpDescLeaveJointState)
}

// these values are used for unit test.
const (
	// mock region default region size is 96MB.
	mockRegionSize = 96
	mockDesc       = "test"
	mockBrief      = "test"
)

// NewTestOperator creates a test operator, only used for unit test.
func NewTestOperator(regionID uint64, regionEpoch *metapb.RegionEpoch, kind OpKind, steps ...OpStep) *Operator {
	// OpSteps can not be empty for test.
	if len(steps) == 0 {
		steps = []OpStep{ChangePeerV2Leave{}}
	}
	return NewOperator(mockDesc, mockBrief, regionID, regionEpoch, kind, mockRegionSize, steps...)
}