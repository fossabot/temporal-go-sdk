// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package internal

// All code in this file is private to the package.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gogo/protobuf/types"
	"github.com/opentracing/opentracing-go"
	"github.com/pborman/uuid"
	"github.com/uber-go/tally"
	commonpb "go.temporal.io/temporal-proto/common/v1"
	enumspb "go.temporal.io/temporal-proto/enums/v1"
	historypb "go.temporal.io/temporal-proto/history/v1"
	tasklistpb "go.temporal.io/temporal-proto/tasklist/v1"
	"go.temporal.io/temporal-proto/workflowservice/v1"
	"go.uber.org/zap"

	"go.temporal.io/temporal/internal/common"
	"go.temporal.io/temporal/internal/common/backoff"
	"go.temporal.io/temporal/internal/common/metrics"
	"go.temporal.io/temporal/internal/common/serializer"
)

const (
	pollTaskServiceTimeOut = 150 * time.Second // Server long poll is 2 * Minutes + delta

	stickyDecisionScheduleToStartTimeoutSeconds = 5

	ratioToForceCompleteDecisionTaskComplete = 0.8
)

type (
	// taskPoller interface to poll and process for task
	taskPoller interface {
		// PollTask polls for one new task
		PollTask() (interface{}, error)
		// ProcessTask processes a task
		ProcessTask(interface{}) error
	}

	// basePoller is the base class for all poller implementations
	basePoller struct {
		stopC <-chan struct{}
	}

	// workflowTaskPoller implements polling/processing a workflow task
	workflowTaskPoller struct {
		basePoller
		namespace     string
		taskListName  string
		identity      string
		service       workflowservice.WorkflowServiceClient
		taskHandler   WorkflowTaskHandler
		metricsScope  tally.Scope
		logger        *zap.Logger
		dataConverter DataConverter

		stickyUUID                   string
		disableStickyExecution       bool
		StickyScheduleToStartTimeout time.Duration

		pendingRegularPollCount int
		pendingStickyPollCount  int
		stickyBacklog           int64
		requestLock             sync.Mutex
	}

	// activityTaskPoller implements polling/processing a workflow task
	activityTaskPoller struct {
		basePoller
		namespace           string
		taskListName        string
		identity            string
		service             workflowservice.WorkflowServiceClient
		taskHandler         ActivityTaskHandler
		metricsScope        *metrics.TaggedScope
		logger              *zap.Logger
		activitiesPerSecond float64
	}

	historyIteratorImpl struct {
		iteratorFunc  func(nextPageToken []byte) (*historypb.History, []byte, error)
		execution     *commonpb.WorkflowExecution
		nextPageToken []byte
		namespace     string
		service       workflowservice.WorkflowServiceClient
		metricsScope  tally.Scope
		maxEventID    int64
	}

	localActivityTaskPoller struct {
		basePoller
		handler      *localActivityTaskHandler
		metricsScope tally.Scope
		logger       *zap.Logger
		laTunnel     *localActivityTunnel
	}

	localActivityTaskHandler struct {
		userContext        context.Context
		metricsScope       *metrics.TaggedScope
		logger             *zap.Logger
		dataConverter      DataConverter
		contextPropagators []ContextPropagator
		tracer             opentracing.Tracer
	}

	localActivityResult struct {
		result  *commonpb.Payloads
		err     error
		task    *localActivityTask
		backoff time.Duration
	}

	localActivityTunnel struct {
		taskCh   chan *localActivityTask
		resultCh chan interface{}
		stopCh   <-chan struct{}
	}
)

func newLocalActivityTunnel(stopCh <-chan struct{}) *localActivityTunnel {
	return &localActivityTunnel{
		taskCh:   make(chan *localActivityTask, 1000),
		resultCh: make(chan interface{}),
		stopCh:   stopCh,
	}
}

func (lat *localActivityTunnel) getTask() *localActivityTask {
	select {
	case task := <-lat.taskCh:
		return task
	case <-lat.stopCh:
		return nil
	}
}

func (lat *localActivityTunnel) sendTask(task *localActivityTask) bool {
	select {
	case lat.taskCh <- task:
		return true
	case <-lat.stopCh:
		return false
	}
}

func isClientSideError(err error) bool {
	// If an activity execution exceeds deadline.
	return err == context.DeadlineExceeded
}

// stopping returns true if worker is stopping right now
func (bp *basePoller) stopping() bool {
	select {
	case <-bp.stopC:
		return true
	default:
		return false
	}
}

// doPoll runs the given pollFunc in a separate go routine. Returns when either of the conditions are met:
// - poll succeeds, poll fails or worker is stopping
func (bp *basePoller) doPoll(pollFunc func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	if bp.stopping() {
		return nil, errStop
	}

	var err error
	var result interface{}

	doneC := make(chan struct{})
	ctx, cancel := newChannelContext(context.Background(), chanTimeout(pollTaskServiceTimeOut))

	go func() {
		result, err = pollFunc(ctx)
		cancel()
		close(doneC)
	}()

	select {
	case <-doneC:
		return result, err
	case <-bp.stopC:
		cancel()
		return nil, errStop
	}
}

// newWorkflowTaskPoller creates a new workflow task poller which must have a one to one relationship to workflow worker
func newWorkflowTaskPoller(taskHandler WorkflowTaskHandler, service workflowservice.WorkflowServiceClient, params workerExecutionParameters) *workflowTaskPoller {
	return &workflowTaskPoller{
		basePoller:                   basePoller{stopC: params.WorkerStopChannel},
		service:                      service,
		namespace:                    params.Namespace,
		taskListName:                 params.TaskList,
		identity:                     params.Identity,
		taskHandler:                  taskHandler,
		metricsScope:                 params.MetricsScope,
		logger:                       params.Logger,
		dataConverter:                params.DataConverter,
		stickyUUID:                   uuid.New(),
		disableStickyExecution:       params.DisableStickyExecution,
		StickyScheduleToStartTimeout: params.StickyScheduleToStartTimeout,
	}
}

// PollTask polls a new task
func (wtp *workflowTaskPoller) PollTask() (interface{}, error) {
	// Get the task.
	workflowTask, err := wtp.doPoll(wtp.poll)
	if err != nil {
		return nil, err
	}

	return workflowTask, nil
}

// ProcessTask processes a task which could be workflow task or local activity result
func (wtp *workflowTaskPoller) ProcessTask(task interface{}) error {
	if wtp.stopping() {
		return errStop
	}

	switch task := task.(type) {
	case *workflowTask:
		return wtp.processWorkflowTask(task)
	case *resetStickinessTask:
		return wtp.processResetStickinessTask(task)
	default:
		panic("unknown task type.")
	}
}

func (wtp *workflowTaskPoller) processWorkflowTask(task *workflowTask) error {
	if task.task == nil {
		// We didn't have task, poll might have timeout.
		traceLog(func() {
			wtp.logger.Debug("Workflow task unavailable")
		})
		return nil
	}

	doneCh := make(chan struct{})
	laResultCh := make(chan *localActivityResult)
	// close doneCh so local activity worker won't get blocked forever when trying to send back result to laResultCh.
	defer close(doneCh)

	for {
		var response *workflowservice.RespondDecisionTaskCompletedResponse
		startTime := time.Now()
		task.doneCh = doneCh
		task.laResultCh = laResultCh
		completedRequest, err := wtp.taskHandler.ProcessWorkflowTask(
			task,
			func(response interface{}, startTime time.Time) (*workflowTask, error) {
				wtp.logger.Debug("Force RespondDecisionTaskCompleted.", zap.Int64("TaskStartedEventID", task.task.GetStartedEventId()))
				wtp.metricsScope.Counter(metrics.DecisionTaskForceCompleted).Inc(1)
				heartbeatResponse, err := wtp.RespondTaskCompletedWithMetrics(response, nil, task.task, startTime)
				if err != nil {
					return nil, err
				}
				if heartbeatResponse == nil || heartbeatResponse.DecisionTask == nil {
					return nil, nil
				}
				task := wtp.toWorkflowTask(heartbeatResponse.DecisionTask)
				task.doneCh = doneCh
				task.laResultCh = laResultCh
				return task, nil
			},
		)
		if completedRequest == nil && err == nil {
			return nil
		}
		if _, ok := err.(decisionHeartbeatError); ok {
			return err
		}
		response, err = wtp.RespondTaskCompletedWithMetrics(completedRequest, err, task.task, startTime)
		if err != nil {
			return err
		}

		if response == nil || response.DecisionTask == nil {
			return nil
		}

		// we are getting new decision task, so reset the workflowTask and continue process the new one
		task = wtp.toWorkflowTask(response.DecisionTask)
	}
}

func (wtp *workflowTaskPoller) processResetStickinessTask(rst *resetStickinessTask) error {
	tchCtx, cancel := newChannelContext(context.Background())
	defer cancel()
	wtp.metricsScope.Counter(metrics.StickyCacheEvict).Inc(1)
	if _, err := wtp.service.ResetStickyTaskList(tchCtx, rst.task); err != nil {
		wtp.logger.Warn("ResetStickyTaskList failed",
			zap.String(tagWorkflowID, rst.task.Execution.GetWorkflowId()),
			zap.String(tagRunID, rst.task.Execution.GetRunId()),
			zap.Error(err))
		return err
	}

	return nil
}

func (wtp *workflowTaskPoller) RespondTaskCompletedWithMetrics(completedRequest interface{}, taskErr error, task *workflowservice.PollForDecisionTaskResponse, startTime time.Time) (response *workflowservice.RespondDecisionTaskCompletedResponse, err error) {

	if taskErr != nil {
		wtp.metricsScope.Counter(metrics.DecisionExecutionFailedCounter).Inc(1)
		wtp.logger.Warn("Failed to process decision task.",
			zap.String(tagWorkflowType, task.WorkflowType.GetName()),
			zap.String(tagWorkflowID, task.WorkflowExecution.GetWorkflowId()),
			zap.String(tagRunID, task.WorkflowExecution.GetRunId()),
			zap.Error(taskErr))
		// convert err to DecisionTaskFailed
		completedRequest = errorToFailDecisionTask(task.TaskToken, taskErr, wtp.identity, wtp.dataConverter)
	} else {
		wtp.metricsScope.Counter(metrics.DecisionTaskCompletedCounter).Inc(1)
	}

	wtp.metricsScope.Timer(metrics.DecisionExecutionLatency).Record(time.Since(startTime))

	responseStartTime := time.Now()
	if response, err = wtp.RespondTaskCompleted(completedRequest, task); err != nil {
		wtp.metricsScope.Counter(metrics.DecisionResponseFailedCounter).Inc(1)
		return
	}
	wtp.metricsScope.Timer(metrics.DecisionResponseLatency).Record(time.Since(responseStartTime))

	return
}

func (wtp *workflowTaskPoller) RespondTaskCompleted(completedRequest interface{}, task *workflowservice.PollForDecisionTaskResponse) (response *workflowservice.RespondDecisionTaskCompletedResponse, err error) {
	ctx := context.Background()
	// Respond task completion.
	err = backoff.Retry(ctx,
		func() error {
			tchCtx, cancel := newChannelContext(ctx)
			defer cancel()
			var err1 error
			switch request := completedRequest.(type) {
			case *workflowservice.RespondDecisionTaskFailedRequest:
				// Only fail decision on first attempt, subsequent failure on the same decision task will timeout.
				// This is to avoid spin on the failed decision task. Checking Attempt not nil for older server.
				if task.GetAttempt() == 0 {
					_, err1 = wtp.service.RespondDecisionTaskFailed(tchCtx, request)
					if err1 != nil {
						traceLog(func() {
							wtp.logger.Debug("RespondDecisionTaskFailed failed.", zap.Error(err1))
						})
					}
				}
			case *workflowservice.RespondDecisionTaskCompletedRequest:
				if request.StickyAttributes == nil && !wtp.disableStickyExecution {
					request.StickyAttributes = &tasklistpb.StickyExecutionAttributes{
						WorkerTaskList:                &tasklistpb.TaskList{Name: getWorkerTaskList(wtp.stickyUUID)},
						ScheduleToStartTimeoutSeconds: common.Int32Ceil(wtp.StickyScheduleToStartTimeout.Seconds()),
					}
				} else {
					request.ReturnNewDecisionTask = false
				}
				response, err1 = wtp.service.RespondDecisionTaskCompleted(tchCtx, request)
				if err1 != nil {
					traceLog(func() {
						wtp.logger.Debug("RespondDecisionTaskCompleted failed.", zap.Error(err1))
					})
				}
			case *workflowservice.RespondQueryTaskCompletedRequest:
				_, err1 = wtp.service.RespondQueryTaskCompleted(tchCtx, request)
				if err1 != nil {
					traceLog(func() {
						wtp.logger.Debug("RespondQueryTaskCompleted failed.", zap.Error(err1))
					})
				}
			default:
				// should not happen
				panic("unknown request type from ProcessWorkflowTask()")
			}

			return err1
		}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)

	return
}

func newLocalActivityPoller(params workerExecutionParameters, laTunnel *localActivityTunnel) *localActivityTaskPoller {
	handler := &localActivityTaskHandler{
		userContext:        params.UserContext,
		metricsScope:       metrics.NewTaggedScope(params.MetricsScope),
		logger:             params.Logger,
		dataConverter:      params.DataConverter,
		contextPropagators: params.ContextPropagators,
		tracer:             params.Tracer,
	}
	return &localActivityTaskPoller{
		basePoller:   basePoller{stopC: params.WorkerStopChannel},
		handler:      handler,
		metricsScope: params.MetricsScope,
		logger:       params.Logger,
		laTunnel:     laTunnel,
	}
}

func (latp *localActivityTaskPoller) PollTask() (interface{}, error) {
	return latp.laTunnel.getTask(), nil
}

func (latp *localActivityTaskPoller) ProcessTask(task interface{}) error {
	if latp.stopping() {
		return errStop
	}

	result := latp.handler.executeLocalActivityTask(task.(*localActivityTask))
	// We need to send back the local activity result to unblock workflowTaskPoller.processWorkflowTask() which is
	// synchronously listening on the laResultCh. We also want to make sure we don't block here forever in case
	// processWorkflowTask() already returns and nobody is receiving from laResultCh. We guarantee that doneCh is closed
	// before returning from workflowTaskPoller.processWorkflowTask().
	select {
	case result.task.workflowTask.laResultCh <- result:
		return nil
	case <-result.task.workflowTask.doneCh:
		// processWorkflowTask() already returns, just drop this local activity result.
		return nil
	}
}

func (lath *localActivityTaskHandler) executeLocalActivityTask(task *localActivityTask) (result *localActivityResult) {
	workflowType := task.params.WorkflowInfo.WorkflowType.Name
	activityType := task.params.ActivityType
	metricsScope := getMetricsScopeForLocalActivity(lath.metricsScope, workflowType, activityType)

	metricsScope.Counter(metrics.LocalActivityTotalCounter).Inc(1)

	ae := activityExecutor{name: activityType, fn: task.params.ActivityFn}

	rootCtx := lath.userContext
	if rootCtx == nil {
		rootCtx = context.Background()
	}

	workflowTypeLocal := task.params.WorkflowInfo.WorkflowType

	ctx := context.WithValue(rootCtx, activityEnvContextKey, &activityEnvironment{
		workflowType:      &workflowTypeLocal,
		workflowNamespace: task.params.WorkflowInfo.Namespace,
		taskList:          task.params.WorkflowInfo.TaskListName,
		activityType:      ActivityType{Name: activityType},
		activityID:        fmt.Sprintf("%v", task.activityID),
		workflowExecution: task.params.WorkflowInfo.WorkflowExecution,
		logger:            lath.logger,
		metricsScope:      metricsScope,
		isLocalActivity:   true,
		dataConverter:     lath.dataConverter,
		attempt:           task.attempt,
	})

	// panic handler
	defer func() {
		if p := recover(); p != nil {
			topLine := fmt.Sprintf("local activity for %s [panic]:", activityType)
			st := getStackTraceRaw(topLine, 7, 0)
			lath.logger.Error("LocalActivity panic.",
				zap.String(tagWorkflowID, task.params.WorkflowInfo.WorkflowExecution.ID),
				zap.String(tagRunID, task.params.WorkflowInfo.WorkflowExecution.RunID),
				zap.String(tagActivityType, activityType),
				zap.String("PanicError", fmt.Sprintf("%v", p)),
				zap.String("PanicStack", st))
			metricsScope.Counter(metrics.LocalActivityPanicCounter).Inc(1)
			panicErr := newPanicError(p, st)
			result = &localActivityResult{
				task:   task,
				result: nil,
				err:    panicErr,
			}
		}
		if result.err != nil {
			metricsScope.Counter(metrics.LocalActivityFailedCounter).Inc(1)
		}
	}()

	timeout := task.params.ScheduleToCloseTimeoutSeconds
	if task.params.StartToCloseTimeoutSeconds != 0 && task.params.StartToCloseTimeoutSeconds < timeout {
		timeout = task.params.StartToCloseTimeoutSeconds
	}
	timeoutDuration := time.Duration(timeout) * time.Second
	deadline := time.Now().Add(timeoutDuration)
	if task.attempt > 0 && !task.expireTime.IsZero() && task.expireTime.Before(deadline) {
		// this is attempt and expire time is before SCHEDULE_TO_CLOSE timeout
		deadline = task.expireTime
	}

	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	task.Lock()
	if task.canceled {
		task.Unlock()
		return &localActivityResult{err: ErrCanceled, task: task}
	}
	task.cancelFunc = cancel
	task.Unlock()

	var laResult *commonpb.Payloads
	var err error
	doneCh := make(chan struct{})
	go func(ch chan struct{}) {
		laStartTime := time.Now()
		ctx, span := createOpenTracingActivitySpan(ctx, lath.tracer, time.Now(), task.params.ActivityType, task.params.WorkflowInfo.WorkflowExecution.ID, task.params.WorkflowInfo.WorkflowExecution.RunID)
		defer span.Finish()
		laResult, err = ae.ExecuteWithActualArgs(ctx, task.params.InputArgs)
		executionLatency := time.Since(laStartTime)
		close(ch)
		metricsScope.Timer(metrics.LocalActivityExecutionLatency).Record(executionLatency)
		if executionLatency > timeoutDuration {
			// If local activity takes longer than expected timeout, the context would already be DeadlineExceeded and
			// the result would be discarded. Print a warning in this case.
			lath.logger.Warn("LocalActivity takes too long to complete.",
				zap.String("LocalActivityID", task.activityID),
				zap.String("LocalActivityType", activityType),
				zap.Int32("ScheduleToCloseTimeoutSeconds", task.params.ScheduleToCloseTimeoutSeconds),
				zap.Duration("ActualExecutionDuration", executionLatency))
		}
	}(doneCh)

WaitResult:
	select {
	case <-ctx.Done():
		select {
		case <-doneCh:
			// double check if result is ready.
			break WaitResult
		default:
		}

		// context is done
		if ctx.Err() == context.Canceled {
			metricsScope.Counter(metrics.LocalActivityCanceledCounter).Inc(1)
			return &localActivityResult{err: ErrCanceled, task: task}
		} else if ctx.Err() == context.DeadlineExceeded {
			metricsScope.Counter(metrics.LocalActivityTimeoutCounter).Inc(1)
			return &localActivityResult{err: ErrDeadlineExceeded, task: task}
		} else {
			// should not happen
			return &localActivityResult{err: NewApplicationError("unexpected context done", true, nil), task: task}
		}
	case <-doneCh:
		// local activity completed
	}

	return &localActivityResult{result: laResult, err: err, task: task}
}

func (wtp *workflowTaskPoller) release(kind enumspb.TaskListKind) {
	if wtp.disableStickyExecution {
		return
	}

	wtp.requestLock.Lock()
	if kind == enumspb.TASK_LIST_KIND_STICKY {
		wtp.pendingStickyPollCount--
	} else {
		wtp.pendingRegularPollCount--
	}
	wtp.requestLock.Unlock()
}

func (wtp *workflowTaskPoller) updateBacklog(taskListKind enumspb.TaskListKind, backlogCountHint int64) {
	if taskListKind == enumspb.TASK_LIST_KIND_NORMAL || wtp.disableStickyExecution {
		// we only care about sticky backlog for now.
		return
	}
	wtp.requestLock.Lock()
	wtp.stickyBacklog = backlogCountHint
	wtp.requestLock.Unlock()
}

// getNextPollRequest returns appropriate next poll request based on poller configuration.
// Simple rules:
// 1) if sticky execution is disabled, always poll for regular task list
// 2) otherwise:
//   2.1) if sticky task list has backlog, always prefer to process sticky task first
//   2.2) poll from the task list that has less pending requests (prefer sticky when they are the same).
// TODO: make this more smart to auto adjust based on poll latency
func (wtp *workflowTaskPoller) getNextPollRequest() (request *workflowservice.PollForDecisionTaskRequest) {
	taskListName := wtp.taskListName
	taskListKind := enumspb.TASK_LIST_KIND_NORMAL
	if !wtp.disableStickyExecution {
		wtp.requestLock.Lock()
		if wtp.stickyBacklog > 0 || wtp.pendingStickyPollCount <= wtp.pendingRegularPollCount {
			wtp.pendingStickyPollCount++
			taskListName = getWorkerTaskList(wtp.stickyUUID)
			taskListKind = enumspb.TASK_LIST_KIND_STICKY
		} else {
			wtp.pendingRegularPollCount++
		}
		wtp.requestLock.Unlock()
	}

	taskList := &tasklistpb.TaskList{
		Name: taskListName,
		Kind: taskListKind,
	}
	return &workflowservice.PollForDecisionTaskRequest{
		Namespace:      wtp.namespace,
		TaskList:       taskList,
		Identity:       wtp.identity,
		BinaryChecksum: getBinaryChecksum(),
	}
}

// Poll for a single workflow task from the service
func (wtp *workflowTaskPoller) poll(ctx context.Context) (interface{}, error) {
	startTime := time.Now()
	wtp.metricsScope.Counter(metrics.DecisionPollCounter).Inc(1)

	traceLog(func() {
		wtp.logger.Debug("workflowTaskPoller::Poll")
	})

	request := wtp.getNextPollRequest()
	defer wtp.release(request.TaskList.GetKind())

	response, err := wtp.service.PollForDecisionTask(ctx, request)
	if err != nil {
		if isServiceTransientError(err) {
			wtp.metricsScope.Counter(metrics.DecisionPollTransientFailedCounter).Inc(1)
		} else {
			wtp.metricsScope.Counter(metrics.DecisionPollFailedCounter).Inc(1)
		}
		wtp.updateBacklog(request.TaskList.GetKind(), 0)
		return nil, err
	}

	if response == nil || len(response.TaskToken) == 0 {
		wtp.metricsScope.Counter(metrics.DecisionPollNoTaskCounter).Inc(1)
		wtp.updateBacklog(request.TaskList.GetKind(), 0)
		return &workflowTask{}, nil
	}

	wtp.updateBacklog(request.TaskList.GetKind(), response.GetBacklogCountHint())

	task := wtp.toWorkflowTask(response)
	traceLog(func() {
		var firstEventID int64 = -1
		if response.History != nil && len(response.History.Events) > 0 {
			firstEventID = response.History.Events[0].GetEventId()
		}
		wtp.logger.Debug("workflowTaskPoller::Poll Succeed",
			zap.Int64("StartedEventID", response.GetStartedEventId()),
			zap.Int64("Attempt", response.GetAttempt()),
			zap.Int64("FirstEventID", firstEventID),
			zap.Bool("IsQueryTask", response.Query != nil))
	})

	wtp.metricsScope.Counter(metrics.DecisionPollSucceedCounter).Inc(1)
	wtp.metricsScope.Timer(metrics.DecisionPollLatency).Record(time.Since(startTime))

	scheduledToStartLatency := time.Duration(response.GetStartedTimestamp() - response.GetScheduledTimestamp())
	wtp.metricsScope.Timer(metrics.DecisionScheduledToStartLatency).Record(scheduledToStartLatency)
	return task, nil
}

func (wtp *workflowTaskPoller) toWorkflowTask(response *workflowservice.PollForDecisionTaskResponse) *workflowTask {
	historyIterator := &historyIteratorImpl{
		nextPageToken: response.NextPageToken,
		execution:     response.WorkflowExecution,
		namespace:     wtp.namespace,
		service:       wtp.service,
		metricsScope:  wtp.metricsScope,
		maxEventID:    response.GetStartedEventId(),
	}
	task := &workflowTask{
		task:            response,
		historyIterator: historyIterator,
	}
	return task
}

func (h *historyIteratorImpl) GetNextPage() (*historypb.History, error) {
	if h.iteratorFunc == nil {
		h.iteratorFunc = newGetHistoryPageFunc(
			context.Background(),
			h.service,
			h.namespace,
			h.execution,
			h.maxEventID,
			h.metricsScope)
	}

	history, token, err := h.iteratorFunc(h.nextPageToken)
	if err != nil {
		return nil, err
	}
	h.nextPageToken = token
	return history, nil
}

func (h *historyIteratorImpl) Reset() {
	h.nextPageToken = nil
}

func (h *historyIteratorImpl) HasNextPage() bool {
	return h.nextPageToken != nil
}

func newGetHistoryPageFunc(
	ctx context.Context,
	service workflowservice.WorkflowServiceClient,
	namespace string,
	execution *commonpb.WorkflowExecution,
	atDecisionTaskCompletedEventID int64,
	metricsScope tally.Scope,
) func(nextPageToken []byte) (*historypb.History, []byte, error) {
	return func(nextPageToken []byte) (*historypb.History, []byte, error) {
		metricsScope.Counter(metrics.WorkflowGetHistoryCounter).Inc(1)
		startTime := time.Now()
		var resp *workflowservice.GetWorkflowExecutionHistoryResponse
		err := backoff.Retry(ctx,
			func() error {
				tchCtx, cancel := newChannelContext(ctx)
				defer cancel()

				var err1 error
				resp, err1 = service.GetWorkflowExecutionHistory(tchCtx, &workflowservice.GetWorkflowExecutionHistoryRequest{
					Namespace:     namespace,
					Execution:     execution,
					NextPageToken: nextPageToken,
				})
				return err1
			}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
		if err != nil {
			metricsScope.Counter(metrics.WorkflowGetHistoryFailedCounter).Inc(1)
			return nil, nil, err
		}

		metricsScope.Counter(metrics.WorkflowGetHistorySucceedCounter).Inc(1)
		metricsScope.Timer(metrics.WorkflowGetHistoryLatency).Record(time.Since(startTime))

		var h *historypb.History

		if resp.RawHistory != nil {
			var err1 error
			h, err1 = serializer.DeserializeBlobDataToHistoryEvents(resp.RawHistory, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
			if err1 != nil {
				return nil, nil, nil
			}
		} else {
			h = resp.History
		}

		size := len(h.Events)
		if size > 0 && atDecisionTaskCompletedEventID > 0 &&
			h.Events[size-1].GetEventId() > atDecisionTaskCompletedEventID {
			first := h.Events[0].GetEventId() // eventIds start from 1
			h.Events = h.Events[:atDecisionTaskCompletedEventID-first+1]
			if h.Events[len(h.Events)-1].GetEventType() != enumspb.EVENT_TYPE_DECISION_TASK_COMPLETED {
				return nil, nil, fmt.Errorf("newGetHistoryPageFunc: atDecisionTaskCompletedEventID(%v) "+
					"points to event that is not DecisionTaskCompleted", atDecisionTaskCompletedEventID)
			}
			return h, nil, nil
		}
		return h, resp.NextPageToken, nil
	}
}

func newActivityTaskPoller(taskHandler ActivityTaskHandler, service workflowservice.WorkflowServiceClient, params workerExecutionParameters) *activityTaskPoller {
	return &activityTaskPoller{
		basePoller:          basePoller{stopC: params.WorkerStopChannel},
		taskHandler:         taskHandler,
		service:             service,
		namespace:           params.Namespace,
		taskListName:        params.TaskList,
		identity:            params.Identity,
		logger:              params.Logger,
		metricsScope:        metrics.NewTaggedScope(params.MetricsScope),
		activitiesPerSecond: params.TaskListActivitiesPerSecond,
	}
}

// Poll for a single activity task from the service
func (atp *activityTaskPoller) poll(ctx context.Context) (interface{}, error) {
	startTime := time.Now()

	atp.metricsScope.Counter(metrics.ActivityPollCounter).Inc(1)

	traceLog(func() {
		atp.logger.Debug("activityTaskPoller::Poll")
	})
	request := &workflowservice.PollForActivityTaskRequest{
		Namespace:        atp.namespace,
		TaskList:         &tasklistpb.TaskList{Name: atp.taskListName},
		Identity:         atp.identity,
		TaskListMetadata: &tasklistpb.TaskListMetadata{MaxTasksPerSecond: &types.DoubleValue{Value: atp.activitiesPerSecond}},
	}

	response, err := atp.service.PollForActivityTask(ctx, request)
	if err != nil {
		if isServiceTransientError(err) {
			atp.metricsScope.Counter(metrics.ActivityPollTransientFailedCounter).Inc(1)
		} else {
			atp.metricsScope.Counter(metrics.ActivityPollFailedCounter).Inc(1)
		}
		return nil, err
	}
	if response == nil || len(response.TaskToken) == 0 {
		atp.metricsScope.Counter(metrics.ActivityPollNoTaskCounter).Inc(1)
		return &activityTask{}, nil
	}

	atp.metricsScope.Counter(metrics.ActivityPollSucceedCounter).Inc(1)
	atp.metricsScope.Timer(metrics.ActivityPollLatency).Record(time.Since(startTime))

	scheduledToStartLatency := time.Duration(response.GetStartedTimestamp() - response.GetScheduledTimestampOfThisAttempt())
	atp.metricsScope.Timer(metrics.ActivityScheduledToStartLatency).Record(scheduledToStartLatency)

	return &activityTask{task: response, pollStartTime: startTime}, nil
}

// PollTask polls a new task
func (atp *activityTaskPoller) PollTask() (interface{}, error) {
	// Get the task.
	activityTask, err := atp.doPoll(atp.poll)
	if err != nil {
		return nil, err
	}
	return activityTask, nil
}

// ProcessTask processes a new task
func (atp *activityTaskPoller) ProcessTask(task interface{}) error {
	if atp.stopping() {
		return errStop
	}

	activityTask := task.(*activityTask)
	if activityTask.task == nil {
		// We didn't have task, poll might have timeout.
		traceLog(func() {
			atp.logger.Debug("Activity task unavailable")
		})
		return nil
	}

	workflowType := activityTask.task.WorkflowType.GetName()
	activityType := activityTask.task.ActivityType.GetName()
	metricsScope := getMetricsScopeForActivity(atp.metricsScope, workflowType, activityType)

	executionStartTime := time.Now()
	// Process the activity task.
	request, err := atp.taskHandler.Execute(atp.taskListName, activityTask.task)
	if err != nil {
		metricsScope.Counter(metrics.ActivityExecutionFailedCounter).Inc(1)
		return err
	}
	metricsScope.Timer(metrics.ActivityExecutionLatency).Record(time.Since(executionStartTime))

	if request == ErrActivityResultPending {
		return nil
	}

	// if worker is stopping, don't bother reporting activity completion
	if atp.stopping() {
		return errStop
	}

	responseStartTime := time.Now()
	reportErr := reportActivityComplete(context.Background(), atp.service, request, metricsScope)
	if reportErr != nil {
		metricsScope.Counter(metrics.ActivityResponseFailedCounter).Inc(1)
		traceLog(func() {
			atp.logger.Debug("reportActivityComplete failed", zap.Error(reportErr))
		})
		return reportErr
	}

	metricsScope.Timer(metrics.ActivityResponseLatency).Record(time.Since(responseStartTime))
	metricsScope.Timer(metrics.ActivityEndToEndLatency).Record(time.Since(activityTask.pollStartTime))
	return nil
}

func reportActivityComplete(ctx context.Context, service workflowservice.WorkflowServiceClient, request interface{}, metricsScope tally.Scope) error {
	if request == nil {
		// nothing to report
		return nil
	}

	var reportErr error
	switch request := request.(type) {
	case *workflowservice.RespondActivityTaskCanceledRequest:
		reportErr = backoff.Retry(ctx,
			func() error {
				tchCtx, cancel := newChannelContext(ctx)
				defer cancel()

				_, err := service.RespondActivityTaskCanceled(tchCtx, request)
				return err
			}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
	case *workflowservice.RespondActivityTaskFailedRequest:
		reportErr = backoff.Retry(ctx,
			func() error {
				tchCtx, cancel := newChannelContext(ctx)
				defer cancel()

				_, err := service.RespondActivityTaskFailed(tchCtx, request)
				return err
			}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
	case *workflowservice.RespondActivityTaskCompletedRequest:
		reportErr = backoff.Retry(ctx,
			func() error {
				tchCtx, cancel := newChannelContext(ctx)
				defer cancel()

				_, err := service.RespondActivityTaskCompleted(tchCtx, request)
				return err
			}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
	}
	if reportErr == nil {
		switch request.(type) {
		case *workflowservice.RespondActivityTaskCanceledRequest:
			metricsScope.Counter(metrics.ActivityTaskCanceledCounter).Inc(1)
		case *workflowservice.RespondActivityTaskFailedRequest:
			metricsScope.Counter(metrics.ActivityTaskFailedCounter).Inc(1)
		case *workflowservice.RespondActivityTaskCompletedRequest:
			metricsScope.Counter(metrics.ActivityTaskCompletedCounter).Inc(1)
		}
	}

	return reportErr
}

func reportActivityCompleteByID(ctx context.Context, service workflowservice.WorkflowServiceClient, request interface{}, metricsScope tally.Scope) error {
	if request == nil {
		// nothing to report
		return nil
	}

	var reportErr error
	switch request := request.(type) {
	case *workflowservice.RespondActivityTaskCanceledByIdRequest:
		reportErr = backoff.Retry(ctx,
			func() error {
				tchCtx, cancel := newChannelContext(ctx)
				defer cancel()

				_, err := service.RespondActivityTaskCanceledById(tchCtx, request)
				return err
			}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
	case *workflowservice.RespondActivityTaskFailedByIdRequest:
		reportErr = backoff.Retry(ctx,
			func() error {
				tchCtx, cancel := newChannelContext(ctx)
				defer cancel()

				_, err := service.RespondActivityTaskFailedById(tchCtx, request)
				return err
			}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
	case *workflowservice.RespondActivityTaskCompletedByIdRequest:
		reportErr = backoff.Retry(ctx,
			func() error {
				tchCtx, cancel := newChannelContext(ctx)
				defer cancel()

				_, err := service.RespondActivityTaskCompletedById(tchCtx, request)
				return err
			}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
	}
	if reportErr == nil {
		switch request.(type) {
		case *workflowservice.RespondActivityTaskCanceledByIdRequest:
			metricsScope.Counter(metrics.ActivityTaskCanceledByIDCounter).Inc(1)
		case *workflowservice.RespondActivityTaskFailedByIdRequest:
			metricsScope.Counter(metrics.ActivityTaskFailedByIDCounter).Inc(1)
		case *workflowservice.RespondActivityTaskCompletedByIdRequest:
			metricsScope.Counter(metrics.ActivityTaskCompletedByIDCounter).Inc(1)
		}
	}

	return reportErr
}

func convertActivityResultToRespondRequest(identity string, taskToken []byte, result *commonpb.Payloads, err error,
	dataConverter DataConverter) interface{} {
	if err == ErrActivityResultPending {
		// activity result is pending and will be completed asynchronously.
		// nothing to report at this point
		return ErrActivityResultPending
	}

	if err == nil {
		return &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    result,
			Identity:  identity}
	}

	var cancelledErr *CanceledError
	if errors.As(err, &cancelledErr) {
		return &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Details:   convertErrDetailsToPayloads(cancelledErr.details, dataConverter),
			Identity:  identity}
	}
	if errors.Is(err, context.Canceled) {
		return &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Identity:  identity}
	}

	return &workflowservice.RespondActivityTaskFailedRequest{
		TaskToken: taskToken,
		Failure:   convertErrorToFailure(err, dataConverter),
		Identity:  identity}
}

func convertActivityResultToRespondRequestByID(identity, namespace, workflowID, runID, activityID string,
	result *commonpb.Payloads, err error, dataConverter DataConverter) interface{} {
	if err == ErrActivityResultPending {
		// activity result is pending and will be completed asynchronously.
		// nothing to report at this point
		return nil
	}

	if err == nil {
		return &workflowservice.RespondActivityTaskCompletedByIdRequest{
			Namespace:  namespace,
			WorkflowId: workflowID,
			RunId:      runID,
			ActivityId: activityID,
			Result:     result,
			Identity:   identity}
	}

	var cancelledErr *CanceledError
	if errors.As(err, &cancelledErr) {
		return &workflowservice.RespondActivityTaskCanceledByIdRequest{
			Namespace:  namespace,
			WorkflowId: workflowID,
			RunId:      runID,
			ActivityId: activityID,
			Details:    convertErrDetailsToPayloads(cancelledErr.details, dataConverter),
			Identity:   identity}
	}

	if errors.Is(err, context.Canceled) {
		return &workflowservice.RespondActivityTaskCanceledByIdRequest{
			Namespace:  namespace,
			WorkflowId: workflowID,
			RunId:      runID,
			ActivityId: activityID,
			Identity:   identity}
	}

	return &workflowservice.RespondActivityTaskFailedByIdRequest{
		Namespace:  namespace,
		WorkflowId: workflowID,
		RunId:      runID,
		ActivityId: activityID,
		Failure:    convertErrorToFailure(err, dataConverter),
		Identity:   identity}
}
