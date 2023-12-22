// Copyright (c) 2021 Uber Technologies, Inc.
// Portions of the Software are attributed to Copyright (c) 2020 Temporal Technologies Inc.
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

package cassandra

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/pborman/uuid"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/checksum"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/persistence/nosql/nosqlplugin"
	"github.com/uber/cadence/common/persistence/nosql/nosqlplugin/cassandra/gocql"
	"github.com/uber/cadence/common/types"
)

// fakeSession is fake implementation of gocql.Session
type fakeSession struct {
	// inputs
	iter                      *fakeIter
	mapExecuteBatchCASApplied bool
	mapExecuteBatchCASPrev    map[string]any
	mapExecuteBatchCASErr     error
}

func (s *fakeSession) Query(string, ...interface{}) gocql.Query {
	return nil
}

func (s *fakeSession) NewBatch(gocql.BatchType) gocql.Batch {
	return nil
}

func (s *fakeSession) ExecuteBatch(gocql.Batch) error {
	return nil
}

func (s *fakeSession) MapExecuteBatchCAS(batch gocql.Batch, prev map[string]interface{}) (bool, gocql.Iter, error) {
	for k, v := range s.mapExecuteBatchCASPrev {
		prev[k] = v
	}
	return s.mapExecuteBatchCASApplied, s.iter, s.mapExecuteBatchCASErr
}

// fakeBatch is fake implementation of gocql.Batch
type fakeBatch struct {
	// outputs
	queries []string
}

// Query is fake implementation of gocql.Batch.Query
func (b *fakeBatch) Query(queryTmpl string, args ...interface{}) {
	argsSanitized := make([]interface{}, len(args))
	for i, arg := range args {
		// use values instead of pointer so that we can compare them in tests
		if reflect.ValueOf(arg).Kind() == reflect.Ptr && !reflect.ValueOf(arg).IsZero() {
			argsSanitized[i] = reflect.ValueOf(arg).Elem().Interface()
		} else {
			argsSanitized[i] = arg
		}

		if t, ok := argsSanitized[i].(time.Time); ok {
			// use fixed time format to avoid flakiness
			argsSanitized[i] = t.UTC().Format(time.RFC3339)
		}

	}
	queryTmpl = strings.ReplaceAll(queryTmpl, "?", "%v")
	b.queries = append(b.queries, fmt.Sprintf(queryTmpl, argsSanitized...))
}

// WithContext is fake implementation of gocql.Batch.WithContext
func (b *fakeBatch) WithContext(context.Context) gocql.Batch {
	return nil
}

// WithTimestamp is fake implementation of gocql.Batch.WithTimestamp
func (b *fakeBatch) WithTimestamp(int64) gocql.Batch {
	return nil
}

// Consistency is fake implementation of gocql.Batch.Consistency
func (b *fakeBatch) Consistency(gocql.Consistency) gocql.Batch {
	return nil
}

// fakeQuery is fake implementation of gocql.Query
func (s *fakeSession) Close() {
}

// fakeIter is fake implementation of gocql.Iter
type fakeIter struct {
	// output parameters
	closed bool
}

// Scan is fake implementation of gocql.Iter.Scan
func (i *fakeIter) Scan(...interface{}) bool {
	return false
}

// MapScan is fake implementation of gocql.Iter.MapScan
func (i *fakeIter) MapScan(map[string]interface{}) bool {
	return false
}

// PageState is fake implementation of gocql.Iter.PageState
func (i *fakeIter) PageState() []byte {
	return nil
}

// Close is fake implementation of gocql.Iter.Close
func (i *fakeIter) Close() error {
	i.closed = true
	return nil
}

func TestExecuteCreateWorkflowBatchTransaction(t *testing.T) {
	tests := []struct {
		// fake setup parameters
		desc    string
		session *fakeSession

		// executeCreateWorkflowBatchTransaction args
		batch        *fakeBatch
		currentWFReq *nosqlplugin.CurrentWorkflowWriteRequest
		execution    *nosqlplugin.WorkflowExecutionRequest
		shardCond    *nosqlplugin.ShardCondition

		// expectations
		wantErr error
	}{
		{
			desc:  "applied",
			batch: &fakeBatch{},
			session: &fakeSession{
				mapExecuteBatchCASApplied: true,
				iter:                      &fakeIter{},
			},
		},
		{
			desc:  "CAS error",
			batch: &fakeBatch{},
			session: &fakeSession{
				mapExecuteBatchCASErr: fmt.Errorf("db operation failed for some reason"),
				iter:                  &fakeIter{},
			},
			wantErr: fmt.Errorf("db operation failed for some reason"),
		},
		{
			desc: "shard range id mismatch",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":     rowTypeShard,
					"run_id":   uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
					"range_id": int64(200),
				},
			},
			batch:        &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				ShardRangeIDNotMatch: common.Int64Ptr(200),
			},
		},
		{
			desc: "execution already exists",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":                        rowTypeExecution,
					"run_id":                      uuid.Parse(permanentRunID),
					"range_id":                    int64(100),
					"workflow_last_write_version": int64(3),
					"execution": map[string]any{
						"workflow_id": "test-workflow-id",
						"run_id":      uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
						"state":       1,
					},
				},
			},
			batch:        &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				CurrentWorkflowConditionFailInfo: common.StringPtr(
					"Workflow execution already running. WorkflowId: test-workflow-id, " +
						"RunId: bda9cd9c-32fb-4267-b120-346e5351fc46, rangeID: 100"),
			},
		},
		{
			desc: "execution already exists and write mode is CurrentWorkflowWriteModeInsert",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":                        rowTypeExecution,
					"run_id":                      uuid.Parse(permanentRunID),
					"range_id":                    int64(100),
					"workflow_last_write_version": int64(3),
					"execution": map[string]any{
						"workflow_id": "test-workflow-id",
						"run_id":      uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
						"state":       1,
					},
				},
			},
			batch: &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: nosqlplugin.CurrentWorkflowWriteModeInsert,
			},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				WorkflowExecutionAlreadyExists: &nosqlplugin.WorkflowExecutionAlreadyExists{
					RunID:            "bda9cd9c-32fb-4267-b120-346e5351fc46",
					State:            1,
					LastWriteVersion: 3,
					OtherInfo:        "Workflow execution already running. WorkflowId: test-workflow-id, RunId: bda9cd9c-32fb-4267-b120-346e5351fc46, rangeID: 100",
				},
			},
		},
		{
			desc: "creation condition failed by mismatch runID",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":                        rowTypeExecution,
					"run_id":                      uuid.Parse(permanentRunID),
					"range_id":                    int64(100),
					"workflow_last_write_version": int64(3),
					"current_run_id":              uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
				},
			},
			batch: &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{
				Condition: &nosqlplugin.CurrentWorkflowWriteCondition{
					CurrentRunID: common.StringPtr("fd88863f-bb32-4daa-8878-49e08b91545e"), // not matching current_run_id above on purpose
				},
			},
			execution: &nosqlplugin.WorkflowExecutionRequest{
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					WorkflowID: "wfid",
				},
			},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				CurrentWorkflowConditionFailInfo: common.StringPtr(
					"Workflow execution creation condition failed by mismatch runID. " +
						"WorkflowId: wfid, Expected Current RunID: fd88863f-bb32-4daa-8878-49e08b91545e, " +
						"Actual Current RunID: bda9cd9c-32fb-4267-b120-346e5351fc46"),
			},
		},
		{
			desc: "creation condition failed",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":                        rowTypeExecution,
					"run_id":                      uuid.Parse(permanentRunID),
					"range_id":                    int64(100),
					"workflow_last_write_version": int64(3),
					"current_run_id":              uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
				},
			},
			batch:        &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{},
			execution: &nosqlplugin.WorkflowExecutionRequest{
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					WorkflowID: "wfid",
					RunID:      "bda9cd9c-32fb-4267-b120-346e5351fc46",
				},
			},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				CurrentWorkflowConditionFailInfo: common.StringPtr(
					"Workflow execution creation condition failed. WorkflowId: wfid, " +
						"CurrentRunID: bda9cd9c-32fb-4267-b120-346e5351fc46"),
			},
		},
		{
			desc: "execution already running",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":                        rowTypeExecution,
					"run_id":                      uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
					"range_id":                    int64(100),
					"workflow_last_write_version": int64(3),
				},
			},
			batch:        &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{},
			execution: &nosqlplugin.WorkflowExecutionRequest{
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					WorkflowID:      "wfid",
					RunID:           "bda9cd9c-32fb-4267-b120-346e5351fc46",
					CreateRequestID: "reqid_123",
					State:           2,
				},
			},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				WorkflowExecutionAlreadyExists: &nosqlplugin.WorkflowExecutionAlreadyExists{
					OtherInfo: "Workflow execution already running. WorkflowId: wfid, " +
						"RunId: bda9cd9c-32fb-4267-b120-346e5351fc46, rangeID: 100",
					CreateRequestID:  "reqid_123",
					RunID:            "bda9cd9c-32fb-4267-b120-346e5351fc46",
					State:            2,
					LastWriteVersion: 3,
				},
			},
		},
		{
			desc: "unknown condition failure",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":                        rowTypeExecution,
					"run_id":                      uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
					"range_id":                    int64(100),
					"workflow_last_write_version": int64(3),
				},
			},
			batch:        &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{},
			execution: &nosqlplugin.WorkflowExecutionRequest{
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					RunID: "something else",
				},
			},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				UnknownConditionFailureDetails: common.StringPtr("Failed to operate on workflow execution.  Request RangeID: 100"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := executeCreateWorkflowBatchTransaction(tc.session, tc.batch, tc.currentWFReq, tc.execution, tc.shardCond)
			if diff := errDiff(tc.wantErr, err); diff != "" {
				t.Fatalf("Error mismatch (-want +got):\n%s", diff)
			}
			if !tc.session.iter.closed {
				t.Error("iterator not closed")
			}
		})
	}
}

func TestExecuteUpdateWorkflowBatchTransaction(t *testing.T) {
	tests := []struct {
		// fake setup parameters
		desc    string
		session *fakeSession

		// executeUpdateWorkflowBatchTransaction args
		batch               *fakeBatch
		currentWFReq        *nosqlplugin.CurrentWorkflowWriteRequest
		prevNextEventIDCond int64
		shardCond           *nosqlplugin.ShardCondition

		// expectations
		wantErr error
	}{
		{
			desc:  "applied",
			batch: &fakeBatch{},
			session: &fakeSession{
				mapExecuteBatchCASApplied: true,
				iter:                      &fakeIter{},
			},
		},
		{
			desc:  "CAS error",
			batch: &fakeBatch{},
			session: &fakeSession{
				mapExecuteBatchCASErr: fmt.Errorf("db operation failed for some reason"),
				iter:                  &fakeIter{},
			},
			wantErr: fmt.Errorf("db operation failed for some reason"),
		},
		{
			desc: "range id mismatch for shard row",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":     rowTypeShard,
					"run_id":   uuid.Parse("bda9cd9c-32fb-4267-b120-346e5351fc46"),
					"range_id": int64(200),
				},
			},
			batch:        &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 100,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				ShardRangeIDNotMatch: common.Int64Ptr(200),
			},
		},
		{
			desc: "nextEventID mismatch for execution row",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":          rowTypeExecution,
					"run_id":        uuid.Parse("0875863e-dcef-496a-b8a2-3210b2958e25"),
					"next_event_id": int64(10),
				},
			},
			batch:               &fakeBatch{},
			prevNextEventIDCond: 11, // not matching next_event_id above on purpose
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{
				Row: nosqlplugin.CurrentWorkflowRow{
					RunID: "0875863e-dcef-496a-b8a2-3210b2958e25",
				},
			},
			shardCond: &nosqlplugin.ShardCondition{
				RangeID: 200,
			},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				UnknownConditionFailureDetails: common.StringPtr(
					"Failed to update mutable state. " +
						"previousNextEventIDCondition: 11, actualNextEventID: 10, Request Current RunID: 0875863e-dcef-496a-b8a2-3210b2958e25"),
			},
		},
		{
			desc: "runID mismatch for current execution row",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":           rowTypeExecution,
					"run_id":         uuid.Parse(permanentRunID),
					"current_run_id": uuid.Parse("0875863e-dcef-496a-b8a2-3210b2958e25"),
				},
			},
			batch: &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{
				Condition: &nosqlplugin.CurrentWorkflowWriteCondition{
					CurrentRunID: common.StringPtr("fd88863f-bb32-4daa-8878-49e08b91545e"), // not matching current_run_id above on purpose
				},
			},
			shardCond: &nosqlplugin.ShardCondition{},
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				CurrentWorkflowConditionFailInfo: common.StringPtr(
					"Failed to update mutable state. requestConditionalRunID: fd88863f-bb32-4daa-8878-49e08b91545e, " +
						"Actual Value: 0875863e-dcef-496a-b8a2-3210b2958e25"),
			},
		},
		{
			desc: "unknown condition failure",
			session: &fakeSession{
				mapExecuteBatchCASApplied: false,
				iter:                      &fakeIter{},
				mapExecuteBatchCASPrev: map[string]any{
					"type":           rowTypeExecution,
					"run_id":         uuid.Parse(permanentRunID),
					"current_run_id": uuid.Parse("0875863e-dcef-496a-b8a2-3210b2958e25"),
				},
			},
			batch: &fakeBatch{},
			currentWFReq: &nosqlplugin.CurrentWorkflowWriteRequest{
				Condition: &nosqlplugin.CurrentWorkflowWriteCondition{
					CurrentRunID: common.StringPtr("0875863e-dcef-496a-b8a2-3210b2958e25"), // not matching current_run_id above on purpose
				},
			},
			shardCond: &nosqlplugin.ShardCondition{
				ShardID: 345,
				RangeID: 200,
			},
			prevNextEventIDCond: 11,
			wantErr: &nosqlplugin.WorkflowOperationConditionFailure{
				UnknownConditionFailureDetails: common.StringPtr(
					"Failed to update mutable state. ShardID: 345, RangeID: 200, previousNextEventIDCondition: 11, requestConditionalRunID: 0875863e-dcef-496a-b8a2-3210b2958e25"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := executeUpdateWorkflowBatchTransaction(tc.session, tc.batch, tc.currentWFReq, tc.prevNextEventIDCond, tc.shardCond)
			if diff := errDiff(tc.wantErr, err); diff != "" {
				t.Fatalf("Error mismatch (-want +got):\n%s", diff)
			}
			if !tc.session.iter.closed {
				t.Error("iterator not closed")
			}
		})
	}
}

func TestCreateTimerTasks(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-12T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		timerTasks []*nosqlplugin.TimerTask
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain_xyz",
			workflowID: "workflow_xyz",
			timerTasks: []*nosqlplugin.TimerTask{
				{
					RunID:               "rundid_1",
					TaskID:              1,
					TaskType:            1,
					TimeoutType:         1,
					EventID:             10,
					ScheduleAttempt:     0,
					Version:             0,
					VisibilityTimestamp: ts,
				},
				{
					RunID:               "rundid_1",
					TaskID:              2,
					TaskType:            1,
					TimeoutType:         1,
					EventID:             11,
					ScheduleAttempt:     0,
					Version:             0,
					VisibilityTimestamp: ts.Add(time.Minute),
				},
			},
			wantQueries: []string{
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, timer, visibility_ts, task_id) ` +
					`VALUES(1000, 3, 10000000-4000-f000-f000-000000000000, 20000000-4000-f000-f000-000000000000, 30000000-4000-f000-f000-000000000000, ` +
					`{domain_id: domain_xyz, workflow_id: workflow_xyz, run_id: rundid_1, visibility_ts: 1702418921000, task_id: 1, type: 1, timeout_type: 1, event_id: 10, schedule_attempt: 0, version: 0}, ` +
					`1702418921000, 1)`,
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, timer, visibility_ts, task_id) ` +
					`VALUES(1000, 3, 10000000-4000-f000-f000-000000000000, 20000000-4000-f000-f000-000000000000, 30000000-4000-f000-f000-000000000000, ` +
					`{domain_id: domain_xyz, workflow_id: workflow_xyz, run_id: rundid_1, visibility_ts: 1702418981000, task_id: 2, type: 1, timeout_type: 1, event_id: 11, schedule_attempt: 0, version: 0}, ` +
					`1702418981000, 2)`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}
			err := createTimerTasks(batch, tc.shardID, tc.domainID, tc.workflowID, tc.timerTasks)
			if err != nil {
				t.Fatalf("createTimerTasks failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReplicationTasks(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-12T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		replTasks  []*nosqlplugin.ReplicationTask
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain_xyz",
			workflowID: "workflow_xyz",
			replTasks: []*nosqlplugin.ReplicationTask{
				{
					RunID:             "rundid_1",
					TaskID:            644,
					TaskType:          0,
					FirstEventID:      5,
					NextEventID:       8,
					Version:           0,
					ScheduledID:       common.EmptyEventID,
					NewRunBranchToken: []byte{'a', 'b', 'c'},
					CreationTime:      ts,
				},
				{
					RunID:             "rundid_1",
					TaskID:            645,
					TaskType:          0,
					FirstEventID:      25,
					NextEventID:       28,
					Version:           0,
					ScheduledID:       common.EmptyEventID,
					NewRunBranchToken: []byte{'a', 'b', 'c'},
					CreationTime:      ts.Add(time.Hour),
				},
			},
			wantQueries: []string{
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, replication, visibility_ts, task_id) ` +
					`VALUES(1000, 4, 10000000-5000-f000-f000-000000000000, 20000000-5000-f000-f000-000000000000, 30000000-5000-f000-f000-000000000000, ` +
					`{domain_id: domain_xyz, workflow_id: workflow_xyz, run_id: rundid_1, task_id: 644, type: 0, ` +
					`first_event_id: 5,next_event_id: 8,version: 0,scheduled_id: -23, event_store_version: 2, branch_token: [], ` +
					`new_run_event_store_version: 2, new_run_branch_token: [97 98 99], created_time: 1702418921000000000 }, 946684800000, 644)`,
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, replication, visibility_ts, task_id) ` +
					`VALUES(1000, 4, 10000000-5000-f000-f000-000000000000, 20000000-5000-f000-f000-000000000000, 30000000-5000-f000-f000-000000000000, ` +
					`{domain_id: domain_xyz, workflow_id: workflow_xyz, run_id: rundid_1, task_id: 645, type: 0, ` +
					`first_event_id: 25,next_event_id: 28,version: 0,scheduled_id: -23, event_store_version: 2, branch_token: [], ` +
					`new_run_event_store_version: 2, new_run_branch_token: [97 98 99], created_time: 1702422521000000000 }, 946684800000, 645)`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}
			err := createReplicationTasks(batch, tc.shardID, tc.domainID, tc.workflowID, tc.replTasks)
			if err != nil {
				t.Fatalf("createReplicationTasks failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTransferTasks(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-12T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc          string
		shardID       int
		domainID      string
		workflowID    string
		transferTasks []*nosqlplugin.TransferTask
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain_xyz",
			workflowID: "workflow_xyz",
			transferTasks: []*nosqlplugin.TransferTask{
				{
					RunID:                   "rundid_1",
					TaskID:                  355,
					TaskType:                0,
					Version:                 1,
					VisibilityTimestamp:     ts,
					TargetDomainID:          "e2bf2c8f-0ddf-4451-8840-27cfe8addd62",
					TargetWorkflowID:        persistence.TransferTaskTransferTargetWorkflowID,
					TargetRunID:             persistence.TransferTaskTransferTargetRunID,
					TargetChildWorkflowOnly: true,
					TaskList:                "tasklist_1",
					ScheduleID:              14,
				},
				{
					RunID:                   "rundid_2",
					TaskID:                  220,
					TaskType:                0,
					Version:                 1,
					VisibilityTimestamp:     ts.Add(time.Minute),
					TargetDomainID:          "e2bf2c8f-0ddf-4451-8840-27cfe8addd62",
					TargetWorkflowID:        persistence.TransferTaskTransferTargetWorkflowID,
					TargetRunID:             persistence.TransferTaskTransferTargetRunID,
					TargetChildWorkflowOnly: true,
					TaskList:                "tasklist_2",
					ScheduleID:              3,
				},
			},
			wantQueries: []string{
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, transfer, visibility_ts, task_id) ` +
					`VALUES(1000, 2, 10000000-3000-f000-f000-000000000000, 20000000-3000-f000-f000-000000000000, 30000000-3000-f000-f000-000000000000, ` +
					`{domain_id: domain_xyz, workflow_id: workflow_xyz, run_id: rundid_1, visibility_ts: 2023-12-12T22:08:41Z, ` +
					`task_id: 355, target_domain_id: e2bf2c8f-0ddf-4451-8840-27cfe8addd62, target_domain_ids: map[],` +
					`target_workflow_id: 20000000-0000-f000-f000-000000000001, target_run_id: 30000000-0000-f000-f000-000000000002, ` +
					`target_child_workflow_only: true, task_list: tasklist_1, type: 0, schedule_id: 14, record_visibility: false, version: 1}, ` +
					`946684800000, 355)`,
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, transfer, visibility_ts, task_id) ` +
					`VALUES(1000, 2, 10000000-3000-f000-f000-000000000000, 20000000-3000-f000-f000-000000000000, 30000000-3000-f000-f000-000000000000, ` +
					`{domain_id: domain_xyz, workflow_id: workflow_xyz, run_id: rundid_2, visibility_ts: 2023-12-12T22:09:41Z, ` +
					`task_id: 220, target_domain_id: e2bf2c8f-0ddf-4451-8840-27cfe8addd62, target_domain_ids: map[],` +
					`target_workflow_id: 20000000-0000-f000-f000-000000000001, target_run_id: 30000000-0000-f000-f000-000000000002, ` +
					`target_child_workflow_only: true, task_list: tasklist_2, type: 0, schedule_id: 3, record_visibility: false, version: 1}, ` +
					`946684800000, 220)`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}
			err := createTransferTasks(batch, tc.shardID, tc.domainID, tc.workflowID, tc.transferTasks)
			if err != nil {
				t.Fatalf("createTransferTasks failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCrossClusterTasks(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-12T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc          string
		shardID       int
		domainID      string
		workflowID    string
		xClusterTasks []*nosqlplugin.CrossClusterTask
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain_xyz",
			workflowID: "workflow_xyz",
			xClusterTasks: []*nosqlplugin.CrossClusterTask{
				{
					TransferTask: nosqlplugin.TransferTask{
						RunID:                   "rundid_1",
						TaskID:                  355,
						TaskType:                0,
						Version:                 1,
						VisibilityTimestamp:     ts,
						TargetDomainID:          "e2bf2c8f-0ddf-4451-8840-27cfe8addd62",
						TargetWorkflowID:        persistence.TransferTaskTransferTargetWorkflowID,
						TargetRunID:             persistence.TransferTaskTransferTargetRunID,
						TargetChildWorkflowOnly: true,
						TaskList:                "tasklist_1",
						ScheduleID:              14,
					},
				},
			},
			wantQueries: []string{
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, cross_cluster, visibility_ts, task_id) ` +
					`VALUES(1000, 6, 10000000-7000-f000-f000-000000000000, , 30000000-7000-f000-f000-000000000000, ` +
					`{domain_id: domain_xyz, workflow_id: workflow_xyz, run_id: rundid_1, visibility_ts: 2023-12-12T22:08:41Z, ` +
					`task_id: 355, target_domain_id: e2bf2c8f-0ddf-4451-8840-27cfe8addd62, target_domain_ids: map[],` +
					`target_workflow_id: 20000000-0000-f000-f000-000000000001, target_run_id: 30000000-0000-f000-f000-000000000002, ` +
					`target_child_workflow_only: true, task_list: tasklist_1, type: 0, schedule_id: 14, record_visibility: false, version: 1}, ` +
					`946684800000, 355)`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}
			err := createCrossClusterTasks(batch, tc.shardID, tc.domainID, tc.workflowID, tc.xClusterTasks)
			if err != nil {
				t.Fatalf("createCrossClusterTasks failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResetSignalsRequested(t *testing.T) {
	tests := []struct {
		desc         string
		shardID      int
		domainID     string
		workflowID   string
		runID        string
		signalReqIDs []string
		// expectations
		wantQueries []string
	}{
		{
			desc:         "ok",
			shardID:      1000,
			domainID:     "domain_123",
			workflowID:   "workflow_123",
			runID:        "runid_123",
			signalReqIDs: []string{"signalReqID_1", "signalReqID_2"},
			wantQueries: []string{
				`UPDATE executions SET signal_requested = [signalReqID_1 signalReqID_2] WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain_123 and workflow_id = workflow_123 and ` +
					`run_id = runid_123 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}
			err := resetSignalsRequested(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.signalReqIDs)
			if err != nil {
				t.Fatalf("resetSignalsRequested failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateSignalsRequested(t *testing.T) {
	tests := []struct {
		desc               string
		shardID            int
		domainID           string
		workflowID         string
		runID              string
		signalReqIDs       []string
		deleteSignalReqIDs []string
		// expectations
		wantQueries []string
	}{
		{
			desc:               "update only",
			shardID:            1000,
			domainID:           "domain_abc",
			workflowID:         "workflow_abc",
			runID:              "runid_abc",
			signalReqIDs:       []string{"signalReqID_3", "signalReqID_4"},
			deleteSignalReqIDs: []string{},
			wantQueries: []string{
				`UPDATE executions SET signal_requested = signal_requested + [signalReqID_3 signalReqID_4] WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain_abc and workflow_id = workflow_abc and ` +
					`run_id = runid_abc and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
		{
			desc:               "delete only",
			shardID:            1001,
			domainID:           "domain_def",
			workflowID:         "workflow_def",
			runID:              "runid_def",
			signalReqIDs:       []string{},
			deleteSignalReqIDs: []string{"signalReqID_5", "signalReqID_6"},
			wantQueries: []string{
				`UPDATE executions SET signal_requested = signal_requested - [signalReqID_5 signalReqID_6] WHERE ` +
					`shard_id = 1001 and type = 1 and domain_id = domain_def and workflow_id = workflow_def and ` +
					`run_id = runid_def and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
		{
			desc:               "update and delete",
			shardID:            1002,
			domainID:           "domain_ghi",
			workflowID:         "workflow_ghi",
			runID:              "runid_ghi",
			signalReqIDs:       []string{"signalReqID_7"},
			deleteSignalReqIDs: []string{"signalReqID_8"},
			wantQueries: []string{
				`UPDATE executions SET signal_requested = signal_requested + [signalReqID_7] WHERE ` +
					`shard_id = 1002 and type = 1 and domain_id = domain_ghi and workflow_id = workflow_ghi and ` +
					`run_id = runid_ghi and visibility_ts = 946684800000 and task_id = -10 `,
				`UPDATE executions SET signal_requested = signal_requested - [signalReqID_8] WHERE ` +
					`shard_id = 1002 and type = 1 and domain_id = domain_ghi and workflow_id = workflow_ghi and ` +
					`run_id = runid_ghi and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}
			err := updateSignalsRequested(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.signalReqIDs, tc.deleteSignalReqIDs)
			if err != nil {
				t.Fatalf("updateSignalsRequested failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResetSignalInfos(t *testing.T) {
	tests := []struct {
		desc        string
		shardID     int
		domainID    string
		workflowID  string
		runID       string
		signalInfos map[int64]*persistence.SignalInfo
		// expectations
		wantQueries []string
	}{
		{
			desc:       "single signal info",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			signalInfos: map[int64]*persistence.SignalInfo{
				1: {
					Version:               1,
					InitiatedID:           1,
					InitiatedEventBatchID: 2,
					SignalRequestID:       "request1",
					SignalName:            "signal1",
					Input:                 []byte("input1"),
					Control:               []byte("control1"),
				},
				2: {
					Version:               1,
					InitiatedID:           5,
					InitiatedEventBatchID: 6,
					SignalRequestID:       "request2",
					SignalName:            "signal2",
					Input:                 []byte("input2"),
					Control:               []byte("control2"),
				},
			},
			wantQueries: []string{
				`UPDATE executions SET signal_map = map[` +
					`1:map[control:[99 111 110 116 114 111 108 49] initiated_event_batch_id:2 initiated_id:1 input:[105 110 112 117 116 49] signal_name:signal1 signal_request_id:request1 version:1] ` +
					`5:map[control:[99 111 110 116 114 111 108 50] initiated_event_batch_id:6 initiated_id:5 input:[105 110 112 117 116 50] signal_name:signal2 signal_request_id:request2 version:1]` +
					`] WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := resetSignalInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.signalInfos)
			if err != nil {
				t.Fatalf("resetSignalInfos failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateSignalInfos(t *testing.T) {
	tests := []struct {
		desc        string
		shardID     int
		domainID    string
		workflowID  string
		runID       string
		signalInfos map[int64]*persistence.SignalInfo
		deleteInfos []int64
		// expectations
		wantQueries []string
	}{
		{
			desc:       "update and delete signal infos",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			signalInfos: map[int64]*persistence.SignalInfo{
				1: {
					Version:               1,
					InitiatedID:           1,
					InitiatedEventBatchID: 2,
					SignalRequestID:       "request1",
					SignalName:            "signal1",
					Input:                 []byte("input1"),
					Control:               []byte("control1"),
				},
			},
			deleteInfos: []int64{2},
			wantQueries: []string{
				`UPDATE executions SET signal_map[ 1 ] = {` +
					`version: 1, initiated_id: 1, initiated_event_batch_id: 2, signal_request_id: request1, ` +
					`signal_name: signal1, input: [105 110 112 117 116 49], ` +
					`control: [99 111 110 116 114 111 108 49]` +
					`} WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and ` +
					`workflow_id = workflow1 and run_id = runid1 and ` +
					`visibility_ts = 946684800000 and task_id = -10 `,
				`DELETE signal_map[ 2 ] FROM executions WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and ` +
					`workflow_id = workflow1 and run_id = runid1 ` +
					`and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := updateSignalInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.signalInfos, tc.deleteInfos)
			if err != nil {
				t.Fatalf("updateSignalInfos failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResetRequestCancelInfos(t *testing.T) {
	tests := []struct {
		desc               string
		shardID            int
		domainID           string
		workflowID         string
		runID              string
		requestCancelInfos map[int64]*persistence.RequestCancelInfo
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			requestCancelInfos: map[int64]*persistence.RequestCancelInfo{
				1: {
					Version:               1,
					InitiatedID:           1,
					InitiatedEventBatchID: 2,
					CancelRequestID:       "cancelRequest1",
				},
				3: {
					Version:               2,
					InitiatedID:           3,
					InitiatedEventBatchID: 4,
					CancelRequestID:       "cancelRequest3",
				},
			},
			wantQueries: []string{
				`UPDATE executions SET request_cancel_map = map[` +
					`1:map[cancel_request_id:cancelRequest1 initiated_event_batch_id:2 initiated_id:1 version:1] ` +
					`3:map[cancel_request_id:cancelRequest3 initiated_event_batch_id:4 initiated_id:3 version:2]` +
					`]WHERE shard_id = 1000 and type = 1 and domain_id = domain1 and ` +
					`workflow_id = workflow1 and run_id = runid1 and ` +
					`visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := resetRequestCancelInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.requestCancelInfos)
			if err != nil {
				t.Fatalf("resetRequestCancelInfos failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateRequestCancelInfos(t *testing.T) {
	tests := []struct {
		desc               string
		shardID            int
		domainID           string
		workflowID         string
		runID              string
		requestCancelInfos map[int64]*persistence.RequestCancelInfo
		deleteInfos        []int64
		// expectations
		wantQueries []string
	}{
		{
			desc:       "update and delete request cancel infos",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			requestCancelInfos: map[int64]*persistence.RequestCancelInfo{
				1: {
					Version:               1,
					InitiatedID:           1,
					InitiatedEventBatchID: 2,
					CancelRequestID:       "cancelRequest1",
				},
			},
			deleteInfos: []int64{2},
			wantQueries: []string{
				`UPDATE executions SET request_cancel_map[ 1 ] = ` +
					`{version: 1,initiated_id: 1, initiated_event_batch_id: 2, cancel_request_id: cancelRequest1 } ` +
					`WHERE shard_id = 1000 and type = 1 and domain_id = domain1 and ` +
					`workflow_id = workflow1 and run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
				`DELETE request_cancel_map[ 2 ] FROM executions WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := updateRequestCancelInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.requestCancelInfos, tc.deleteInfos)
			if err != nil {
				t.Fatalf("updateRequestCancelInfos failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResetChildExecutionInfos(t *testing.T) {
	tests := []struct {
		desc                string
		shardID             int
		domainID            string
		workflowID          string
		runID               string
		childExecutionInfos map[int64]*persistence.InternalChildExecutionInfo
		// expectations
		wantQueries []string
		wantErr     bool
	}{
		{
			desc:       "execution info with runid",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			childExecutionInfos: map[int64]*persistence.InternalChildExecutionInfo{
				1: {
					Version:               1,
					InitiatedID:           1,
					InitiatedEventBatchID: 2,
					StartedID:             3,
					StartedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
					},
					StartedWorkflowID: "startedWorkflowID1",
					StartedRunID:      "startedRunID1",
					CreateRequestID:   "createRequestID1",
					InitiatedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
					},
					DomainID:          "domain1",
					WorkflowTypeName:  "workflowType1",
					ParentClosePolicy: types.ParentClosePolicyAbandon,
				},
			},
			wantQueries: []string{
				`UPDATE executions SET child_executions_map = ` +
					`map[1:map[` +
					`create_request_id:createRequestID1 domain_id:domain1 domain_name: event_data_encoding:thriftrw ` +
					`initiated_event:[] initiated_event_batch_id:2 initiated_id:1 parent_close_policy:0 ` +
					`started_event:[] started_id:3 started_run_id:startedRunID1 started_workflow_id:startedWorkflowID1 ` +
					`version:1 workflow_type_name:workflowType1` +
					`]]` +
					`WHERE shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
		{
			desc:       "execution info without runid",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      emptyRunID,
			childExecutionInfos: map[int64]*persistence.InternalChildExecutionInfo{
				1: {
					Version:               1,
					InitiatedID:           1,
					InitiatedEventBatchID: 2,
					StartedID:             3,
					StartedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
					},
					StartedWorkflowID: "startedWorkflowID1",
					StartedRunID:      "", // leave empty and validate it's querying empty runid
					CreateRequestID:   "createRequestID1",
					InitiatedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
					},
					DomainID:          "domain1",
					WorkflowTypeName:  "workflowType1",
					ParentClosePolicy: types.ParentClosePolicyAbandon,
				},
			},
			wantQueries: []string{
				`UPDATE executions SET child_executions_map = ` +
					`map[1:map[` +
					`create_request_id:createRequestID1 domain_id:domain1 domain_name: event_data_encoding:thriftrw ` +
					`initiated_event:[] initiated_event_batch_id:2 initiated_id:1 parent_close_policy:0 ` +
					`started_event:[] started_id:3 started_run_id:30000000-0000-f000-f000-000000000000 started_workflow_id:startedWorkflowID1 ` +
					`version:1 workflow_type_name:workflowType1` +
					`]]` +
					`WHERE shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = 30000000-0000-f000-f000-000000000000 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := resetChildExecutionInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.childExecutionInfos)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resetChildExecutionInfos() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateChildExecutionInfos(t *testing.T) {
	tests := []struct {
		desc                string
		shardID             int
		domainID            string
		workflowID          string
		runID               string
		childExecutionInfos map[int64]*persistence.InternalChildExecutionInfo
		deleteInfos         []int64
		// expectations
		wantQueries []string
	}{
		{
			desc:       "update and delete child execution infos",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			childExecutionInfos: map[int64]*persistence.InternalChildExecutionInfo{
				1: {
					Version:               1,
					InitiatedID:           1,
					InitiatedEventBatchID: 2,
					StartedID:             3,
					StartedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
					},
					StartedWorkflowID: "startedWorkflowID1",
					StartedRunID:      "startedRunID1",
					CreateRequestID:   "createRequestID1",
					InitiatedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
					},
					DomainID:          "domain1",
					WorkflowTypeName:  "workflowType1",
					ParentClosePolicy: types.ParentClosePolicyAbandon,
				},
			},
			deleteInfos: []int64{2},
			wantQueries: []string{
				`UPDATE executions SET child_executions_map[ 1 ] = {` +
					`version: 1, initiated_id: 1, initiated_event_batch_id: 2, initiated_event: [], ` +
					`started_id: 3, started_workflow_id: startedWorkflowID1, started_run_id: startedRunID1, ` +
					`started_event: [], create_request_id: createRequestID1, event_data_encoding: thriftrw, ` +
					`domain_id: domain1, domain_name: , workflow_type_name: workflowType1, parent_close_policy: 0` +
					`} WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
				`DELETE child_executions_map[ 2 ] FROM executions WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := updateChildExecutionInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.childExecutionInfos, tc.deleteInfos)
			if err != nil {
				t.Fatalf("updateChildExecutionInfos failed: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResetTimerInfos(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-12T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		runID      string
		timerInfos map[string]*persistence.TimerInfo
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			timerInfos: map[string]*persistence.TimerInfo{
				"timer1": {
					Version:    1,
					TimerID:    "timer1",
					StartedID:  2,
					ExpiryTime: ts.UTC(),
					TaskStatus: 1,
				},
			},
			wantQueries: []string{
				`UPDATE executions SET timer_map = map[` +
					`timer1:map[expiry_time:2023-12-12 22:08:41 +0000 UTC started_id:2 task_id:1 timer_id:timer1 version:1]` +
					`] WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := resetTimerInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.timerInfos)
			if err != nil {
				t.Fatalf("resetTimerInfos() error = %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateTimerInfos(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-19T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc        string
		shardID     int
		domainID    string
		workflowID  string
		runID       string
		timerInfos  map[string]*persistence.TimerInfo
		deleteInfos []string
		// expectations
		wantQueries []string
	}{
		{
			desc:       "update and delete timer infos",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			timerInfos: map[string]*persistence.TimerInfo{
				"timer1": {
					TimerID:    "timer1",
					Version:    1,
					StartedID:  2,
					ExpiryTime: ts.UTC(),
					TaskStatus: 1,
				},
			},
			deleteInfos: []string{"timer2"},
			wantQueries: []string{
				`UPDATE executions SET timer_map[ timer1 ] = {` +
					`version: 1, timer_id: timer1, started_id: 2, expiry_time: 2023-12-19T22:08:41Z, task_id: 1` +
					`} WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
				`DELETE timer_map[ timer2 ] FROM executions WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := updateTimerInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.timerInfos, tc.deleteInfos)
			if err != nil {
				t.Fatalf("updateTimerInfos() error = %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResetActivityInfos(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-19T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc          string
		shardID       int
		domainID      string
		workflowID    string
		runID         string
		activityInfos map[int64]*persistence.InternalActivityInfo
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			activityInfos: map[int64]*persistence.InternalActivityInfo{
				1: {
					Version: 1,
					ScheduledEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
						Data:     []byte("thrift-encoded-scheduled-event-data"),
					},
					ScheduledTime: ts.UTC(),
					ScheduleID:    1,
					StartedID:     2,
					StartedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
						Data:     []byte("thrift-encoded-started-event-data"),
					},
					ActivityID:             "activity1",
					ScheduleToStartTimeout: 1 * time.Minute,
					ScheduleToCloseTimeout: 2 * time.Minute,
					StartToCloseTimeout:    3 * time.Minute,
					HeartbeatTimeout:       1 * time.Minute,
					Attempt:                3,
					MaximumAttempts:        5,
					TaskList:               "tasklist1",
					HasRetryPolicy:         true,
					LastFailureReason:      "retry reason",
				},
				2: {
					Version: 1,
					ScheduledEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
						Data:     []byte("thrift-encoded-scheduled-event-data"),
					},
					ScheduledTime: ts.UTC(),
					ScheduleID:    2,
					StartedID:     3,
					StartedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
						Data:     []byte("thrift-encoded-started-event-data"),
					},
					ActivityID:             "activity2",
					ScheduleToStartTimeout: 1 * time.Minute,
					ScheduleToCloseTimeout: 2 * time.Minute,
					StartToCloseTimeout:    3 * time.Minute,
					HeartbeatTimeout:       1 * time.Minute,
					Attempt:                1,
					MaximumAttempts:        5,
					TaskList:               "tasklist1",
					HasRetryPolicy:         true,
					LastFailureReason:      "another retry reason",
				},
			},
			wantQueries: []string{
				`UPDATE executions SET activity_map = map[` +
					`1:map[` +
					`activity_id:activity1 attempt:3 backoff_coefficient:0 cancel_request_id:0 cancel_requested:false ` +
					`details:[] event_data_encoding:thriftrw expiration_time:0001-01-01 00:00:00 +0000 UTC has_retry_policy:true ` +
					`heart_beat_timeout:60 init_interval:0 last_failure_details:[] last_failure_reason:retry reason ` +
					`last_hb_updated_time:0001-01-01 00:00:00 +0000 UTC last_worker_identity: max_attempts:5 max_interval:0 ` +
					`non_retriable_errors:[] request_id: schedule_id:1 schedule_to_close_timeout:120 schedule_to_start_timeout:60 ` +
					`scheduled_event:[116 104 114 105 102 116 45 101 110 99 111 100 101 100 45 115 99 104 101 100 117 108 101 100 45 101 118 101 110 116 45 100 97 116 97] ` +
					`scheduled_event_batch_id:0 scheduled_time:2023-12-19 22:08:41 +0000 UTC start_to_close_timeout:180 ` +
					`started_event:[116 104 114 105 102 116 45 101 110 99 111 100 101 100 45 115 116 97 114 116 101 100 45 101 118 101 110 116 45 100 97 116 97] ` +
					`started_id:2 started_identity: started_time:0001-01-01 00:00:00 +0000 UTC task_list:tasklist1 timer_task_status:0 version:1` +
					`] ` +
					`2:map[` +
					`activity_id:activity2 attempt:1 backoff_coefficient:0 cancel_request_id:0 cancel_requested:false ` +
					`details:[] event_data_encoding:thriftrw expiration_time:0001-01-01 00:00:00 +0000 UTC has_retry_policy:true ` +
					`heart_beat_timeout:60 init_interval:0 last_failure_details:[] last_failure_reason:another retry reason ` +
					`last_hb_updated_time:0001-01-01 00:00:00 +0000 UTC last_worker_identity: max_attempts:5 max_interval:0 ` +
					`non_retriable_errors:[] request_id: schedule_id:2 schedule_to_close_timeout:120 schedule_to_start_timeout:60 ` +
					`scheduled_event:[116 104 114 105 102 116 45 101 110 99 111 100 101 100 45 115 99 104 101 100 117 108 101 100 45 101 118 101 110 116 45 100 97 116 97] ` +
					`scheduled_event_batch_id:0 scheduled_time:2023-12-19 22:08:41 +0000 UTC start_to_close_timeout:180 ` +
					`started_event:[116 104 114 105 102 116 45 101 110 99 111 100 101 100 45 115 116 97 114 116 101 100 45 101 118 101 110 116 45 100 97 116 97] ` +
					`started_id:3 started_identity: started_time:0001-01-01 00:00:00 +0000 UTC task_list:tasklist1 timer_task_status:0 version:1` +
					`]` +
					`] WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := resetActivityInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.activityInfos)
			if err != nil {
				t.Fatalf("resetActivityInfos() error = %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateActivityInfos(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-19T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc          string
		shardID       int
		domainID      string
		workflowID    string
		runID         string
		activityInfos map[int64]*persistence.InternalActivityInfo
		deleteInfos   []int64
		// expectations
		wantQueries []string
	}{
		{
			desc:       "update and delete activity infos",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			runID:      "runid1",
			activityInfos: map[int64]*persistence.InternalActivityInfo{
				1: {
					Version: 1,
					ScheduledEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
						Data:     []byte("thrift-encoded-scheduled-event-data"),
					},
					ScheduledTime: ts.UTC(),
					ScheduleID:    1,
					StartedID:     2,
					StartedEvent: &persistence.DataBlob{
						Encoding: common.EncodingTypeThriftRW,
						Data:     []byte("thrift-encoded-started-event-data"),
					},
					ActivityID:             "activity1",
					ScheduleToStartTimeout: 1 * time.Minute,
					ScheduleToCloseTimeout: 2 * time.Minute,
					StartToCloseTimeout:    3 * time.Minute,
					HeartbeatTimeout:       1 * time.Minute,
					Attempt:                3,
					MaximumAttempts:        5,
					TaskList:               "tasklist1",
					HasRetryPolicy:         true,
					LastFailureReason:      "retry reason",
				},
			},
			deleteInfos: []int64{2},
			wantQueries: []string{
				`UPDATE executions SET activity_map[ 1 ] = {` +
					`version: 1, schedule_id: 1, scheduled_event_batch_id: 0, ` +
					`scheduled_event: [116 104 114 105 102 116 45 101 110 99 111 100 101 100 45 115 99 104 101 100 117 108 101 100 45 101 118 101 110 116 45 100 97 116 97], ` +
					`scheduled_time: 2023-12-19T22:08:41Z, started_id: 2, ` +
					`started_event: [116 104 114 105 102 116 45 101 110 99 111 100 101 100 45 115 116 97 114 116 101 100 45 101 118 101 110 116 45 100 97 116 97], ` +
					`started_time: 0001-01-01T00:00:00Z, activity_id: activity1, request_id: , ` +
					`details: [], schedule_to_start_timeout: 60, schedule_to_close_timeout: 120, start_to_close_timeout: 180, ` +
					`heart_beat_timeout: 60, cancel_requested: false, cancel_request_id: 0, last_hb_updated_time: 0001-01-01T00:00:00Z, ` +
					`timer_task_status: 0, attempt: 3, task_list: tasklist1, started_identity: , has_retry_policy: true, ` +
					`init_interval: 0, backoff_coefficient: 0, max_interval: 0, expiration_time: 0001-01-01T00:00:00Z, ` +
					`max_attempts: 5, non_retriable_errors: [], last_failure_reason: retry reason, last_worker_identity: , ` +
					`last_failure_details: [], event_data_encoding: thriftrw` +
					`} WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
				`DELETE activity_map[ 2 ] FROM executions ` +
					`WHERE shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := updateActivityInfos(batch, tc.shardID, tc.domainID, tc.workflowID, tc.runID, tc.activityInfos, tc.deleteInfos)
			if err != nil {
				t.Fatalf("updateActivityInfos() error = %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateWorkflowExecutionWithMergeMaps(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-19T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		execution  *nosqlplugin.WorkflowExecutionRequest
		// expectations
		wantQueries int
		wantErr     bool
	}{
		{
			desc:       "EventBufferWriteMode is not None",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeAppend,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
			},
			wantErr: true,
		},
		{
			desc:       "MapsWriteMode is not Create",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeNone,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeUpdate,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
			},
			wantErr: true,
		},
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeNone,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeCreate,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
				ActivityInfos: map[int64]*persistence.InternalActivityInfo{
					1: {
						Version: 1,
						ScheduledEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
							Data:     []byte("thrift-encoded-scheduled-event-data"),
						},
						ScheduledTime: ts.UTC(),
						ScheduleID:    1,
						StartedID:     2,
						StartedEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
							Data:     []byte("thrift-encoded-started-event-data"),
						},
						ActivityID:             "activity1",
						ScheduleToStartTimeout: 1 * time.Minute,
						ScheduleToCloseTimeout: 2 * time.Minute,
						StartToCloseTimeout:    3 * time.Minute,
						HeartbeatTimeout:       1 * time.Minute,
						Attempt:                3,
						MaximumAttempts:        5,
						TaskList:               "tasklist1",
						HasRetryPolicy:         true,
						LastFailureReason:      "retry reason",
					},
				},
				TimerInfos: map[string]*persistence.TimerInfo{
					"timer1": {
						Version:    1,
						TimerID:    "timer1",
						StartedID:  2,
						ExpiryTime: ts,
						TaskStatus: 1,
					},
				},
				ChildWorkflowInfos: map[int64]*persistence.InternalChildExecutionInfo{
					1: {
						Version:               1,
						InitiatedID:           1,
						InitiatedEventBatchID: 2,
						StartedID:             3,
						StartedEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
						},
						StartedWorkflowID: "startedWorkflowID1",
						StartedRunID:      "startedRunID1",
						CreateRequestID:   "createRequestID1",
						InitiatedEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
						},
						DomainID:          "domain1",
						WorkflowTypeName:  "workflowType1",
						ParentClosePolicy: types.ParentClosePolicyAbandon,
					},
				},
				RequestCancelInfos: map[int64]*persistence.RequestCancelInfo{
					1: {
						Version:               1,
						InitiatedID:           1,
						InitiatedEventBatchID: 2,
						CancelRequestID:       "cancelRequest1",
					},
				},
				SignalInfos: map[int64]*persistence.SignalInfo{
					1: {
						Version:               1,
						InitiatedID:           1,
						InitiatedEventBatchID: 2,
						SignalRequestID:       "request1",
						SignalName:            "signal1",
						Input:                 []byte("input1"),
						Control:               []byte("control1"),
					},
				},
				SignalRequestedIDs: []string{"signalRequestedID1"},
			},
			// expecting 7 queries:
			// - 1 for execution record
			// - 1 for activity info
			// - 1 for timer info
			// - 1 for child execution info
			// - 1 for request cancel info
			// - 1 for signal info
			// - 1 for signal requested IDs
			wantQueries: 7,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := createWorkflowExecutionWithMergeMaps(batch, tc.shardID, tc.domainID, tc.workflowID, tc.execution)
			gotErr := (err != nil)
			if gotErr != tc.wantErr {
				t.Fatalf("Got error: %v, want?: %v", err, tc.wantErr)
			}
			if gotErr {
				return
			}

			// actual queries generated by helper functions are covered in other unit tests. check the numer of total queries here.
			if got := len(batch.queries); got != tc.wantQueries {
				t.Fatalf("len(queries): %v, want: %v", got, tc.wantQueries)
			}
		})
	}
}

func TestResetWorkflowExecutionAndMapsAndEventBuffer(t *testing.T) {
	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		execution  *nosqlplugin.WorkflowExecutionRequest
		// expectations
		wantQueries int
		wantErr     bool
	}{
		{
			desc:       "EventBufferWriteMode is not Clear",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeAppend,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
			},
			wantErr: true,
		},
		{
			desc:       "MapsWriteMode is not Reset",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeClear,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeUpdate, // Incorrect mode
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
			},
			wantErr: true,
		},
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeClear,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeReset,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
			},
			// expecting 8 queries:
			// - 1 for execution record
			// - 1 for deletion of buffered events
			// - 1 for activity info map reset
			// - 1 for timer info map reset
			// - 1 for child execution info map reset
			// - 1 for request cancel info map reset
			// - 1 for signal info map reset
			// - 1 for signal requested IDs reset
			wantQueries: 8,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := resetWorkflowExecutionAndMapsAndEventBuffer(batch, tc.shardID, tc.domainID, tc.workflowID, tc.execution)
			gotErr := (err != nil)
			if gotErr != tc.wantErr {
				t.Fatalf("Got error: %v, want?: %v", err, tc.wantErr)
			}
			if gotErr {
				return
			}

			// Check the number of total queries, actual queries generated by helper functions are covered in other unit tests.
			if got := len(batch.queries); got != tc.wantQueries {
				t.Fatalf("len(queries): %v, want: %v", got, tc.wantQueries)
			}
		})
	}
}

func TestUpdateWorkflowExecutionAndEventBufferWithMergeAndDeleteMaps(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-19T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		execution  *nosqlplugin.WorkflowExecutionRequest
		// expectations
		wantQueries int
		wantErr     bool
	}{
		{
			desc:       "MapsWriteMode is not Update",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeClear,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeCreate, // Incorrect mode
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
			},
			wantErr: true,
		},
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeClear,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeUpdate,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					CompletionEvent: &persistence.DataBlob{},
					AutoResetPoints: &persistence.DataBlob{},
				},
				VersionHistories: &persistence.DataBlob{},
				Checksums:        &checksum.Checksum{},
				ActivityInfos: map[int64]*persistence.InternalActivityInfo{
					1: {
						Version: 1,
						ScheduledEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
							Data:     []byte("thrift-encoded-scheduled-event-data"),
						},
						ScheduledTime: ts.UTC(),
						ScheduleID:    1,
						StartedID:     2,
						StartedEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
							Data:     []byte("thrift-encoded-started-event-data"),
						},
						ActivityID:             "activity1",
						ScheduleToStartTimeout: 1 * time.Minute,
						ScheduleToCloseTimeout: 2 * time.Minute,
						StartToCloseTimeout:    3 * time.Minute,
						HeartbeatTimeout:       1 * time.Minute,
						Attempt:                3,
						MaximumAttempts:        5,
						TaskList:               "tasklist1",
						HasRetryPolicy:         true,
						LastFailureReason:      "retry reason",
					},
				},
				TimerInfos: map[string]*persistence.TimerInfo{
					"timer1": {
						Version:    1,
						TimerID:    "timer1",
						StartedID:  2,
						ExpiryTime: ts.UTC(),
						TaskStatus: 1,
					},
				},
				ChildWorkflowInfos: map[int64]*persistence.InternalChildExecutionInfo{
					1: {
						Version:               1,
						InitiatedID:           1,
						InitiatedEventBatchID: 2,
						StartedID:             3,
						StartedEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
						},
						StartedWorkflowID: "startedWorkflowID1",
						StartedRunID:      "startedRunID1",
						CreateRequestID:   "createRequestID1",
						InitiatedEvent: &persistence.DataBlob{
							Encoding: common.EncodingTypeThriftRW,
						},
						DomainID:          "domain1",
						WorkflowTypeName:  "workflowType1",
						ParentClosePolicy: types.ParentClosePolicyAbandon,
					},
				},
				RequestCancelInfos: map[int64]*persistence.RequestCancelInfo{
					1: {
						Version:               1,
						InitiatedID:           1,
						InitiatedEventBatchID: 2,
						CancelRequestID:       "cancelRequest1",
					},
				},
				SignalInfos: map[int64]*persistence.SignalInfo{
					1: {
						Version:               1,
						InitiatedID:           1,
						InitiatedEventBatchID: 2,
						SignalRequestID:       "request1",
						SignalName:            "signal1",
						Input:                 []byte("input1"),
						Control:               []byte("control1"),
					},
				},
				SignalRequestedIDs: []string{"signalRequestedID1"},
			},
			// expecting 8 queries:
			// - 1 for execution record
			// - 1 for deletion of buffered events
			// - 1 for activity info map update
			// - 1 for timer info map update
			// - 1 for child execution info map update
			// - 1 for request cancel info map update
			// - 1 for signal info map update
			// - 1 for signal requested IDs update
			wantQueries: 8,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := updateWorkflowExecutionAndEventBufferWithMergeAndDeleteMaps(batch, tc.shardID, tc.domainID, tc.workflowID, tc.execution)
			gotErr := (err != nil)
			if gotErr != tc.wantErr {
				t.Fatalf("Got error: %v, want?: %v", err, tc.wantErr)
			}
			if gotErr {
				return
			}

			// Check the number of total queries, actual queries generated by helper functions are covered in other unit tests.
			if got := len(batch.queries); got != tc.wantQueries {
				t.Fatalf("len(queries): %v, want: %v", got, tc.wantQueries)
			}
		})
	}
}

func TestUpdateWorkflowExecution(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-19T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		execution  *nosqlplugin.WorkflowExecutionRequest
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeClear,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeUpdate,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					DomainID:             "domain1",
					WorkflowID:           "workflow1",
					RunID:                "runid1",
					ParentRunID:          "parentRunID1",
					WorkflowTypeName:     "workflowType1",
					TaskList:             "tasklist1",
					StartTimestamp:       ts,
					LastUpdatedTimestamp: ts.Add(1 * time.Minute),
					DecisionScheduleID:   2,
					DecisionStartedID:    3,
					CompletionEvent:      &persistence.DataBlob{},
					AutoResetPoints:      &persistence.DataBlob{},
				},
				PreviousNextEventIDCondition: common.Int64Ptr(10),
				VersionHistories:             &persistence.DataBlob{},
				Checksums:                    &checksum.Checksum{},
			},
			wantQueries: []string{
				`UPDATE executions SET execution = {` +
					`domain_id: domain1, workflow_id: workflow1, run_id: runid1, first_run_id: , parent_domain_id: , parent_workflow_id: , ` +
					`parent_run_id: parentRunID1, initiated_id: 0, completion_event_batch_id: 0, completion_event: [], ` +
					`completion_event_data_encoding: , task_list: tasklist1, workflow_type_name: workflowType1, workflow_timeout: 0, ` +
					`decision_task_timeout: 0, execution_context: [], state: 0, close_status: 0, last_first_event_id: 0, last_event_task_id: 0, ` +
					`next_event_id: 0, last_processed_event: 0, start_time: 2023-12-19T22:08:41Z, last_updated_time: 2023-12-19T22:09:41Z, ` +
					`create_request_id: , signal_count: 0, history_size: 0, decision_version: 0, decision_schedule_id: 2, decision_started_id: 3, ` +
					`decision_request_id: , decision_timeout: 0, decision_attempt: 0, decision_timestamp: -6795364578871345152, ` +
					`decision_scheduled_timestamp: -6795364578871345152, decision_original_scheduled_timestamp: -6795364578871345152, ` +
					`cancel_requested: false, cancel_request_id: , sticky_task_list: , sticky_schedule_to_start_timeout: 0,client_library_version: , ` +
					`client_feature_version: , client_impl: , auto_reset_points: [], auto_reset_points_encoding: , attempt: 0, has_retry_policy: false, ` +
					`init_interval: 0, backoff_coefficient: 0, max_interval: 0, expiration_time: 0001-01-01T00:00:00Z, max_attempts: 0, ` +
					`non_retriable_errors: [], event_store_version: 2, branch_token: [], cron_schedule: , expiration_seconds: 0, search_attributes: map[], ` +
					`memo: map[], partition_config: map[] ` +
					`}, next_event_id = 0 , version_histories = [] , version_histories_encoding =  , checksum = {version: 0, flavor: 0, value: [] }, workflow_last_write_version = 0 , workflow_state = 0 ` +
					`WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = runid1 and visibility_ts = 946684800000 and task_id = -10 ` +
					`IF next_event_id = 10 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := updateWorkflowExecution(batch, tc.shardID, tc.domainID, tc.workflowID, tc.execution)
			if err != nil {
				t.Fatalf("updateWorkflowExecution failed, err: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateWorkflowExecution(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2023-12-19T22:08:41Z")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		execution  *nosqlplugin.WorkflowExecutionRequest
		// expectations
		wantQueries []string
	}{
		{
			desc:       "ok",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.WorkflowExecutionRequest{
				EventBufferWriteMode: nosqlplugin.EventBufferWriteModeClear,
				MapsWriteMode:        nosqlplugin.WorkflowExecutionMapsWriteModeUpdate,
				InternalWorkflowExecutionInfo: persistence.InternalWorkflowExecutionInfo{
					DomainID:             "domain1",
					WorkflowID:           "workflow1",
					RunID:                "runid1",
					ParentRunID:          "parentRunID1",
					WorkflowTypeName:     "workflowType1",
					TaskList:             "tasklist1",
					StartTimestamp:       ts,
					LastUpdatedTimestamp: ts.Add(1 * time.Minute),
					DecisionScheduleID:   2,
					DecisionStartedID:    3,
					CompletionEvent:      &persistence.DataBlob{},
					AutoResetPoints:      &persistence.DataBlob{},
				},
				PreviousNextEventIDCondition: common.Int64Ptr(10),
				VersionHistories:             &persistence.DataBlob{},
				Checksums:                    &checksum.Checksum{},
			},
			wantQueries: []string{
				`INSERT INTO executions (shard_id, domain_id, workflow_id, run_id, type, execution, next_event_id, visibility_ts, task_id, version_histories, version_histories_encoding, checksum, workflow_last_write_version, workflow_state) ` +
					`VALUES(1000, domain1, workflow1, runid1, 1, ` +
					`{domain_id: domain1, workflow_id: workflow1, run_id: runid1, first_run_id: , parent_domain_id: , parent_workflow_id: , ` +
					`parent_run_id: parentRunID1, initiated_id: 0, completion_event_batch_id: 0, completion_event: [], completion_event_data_encoding: , ` +
					`task_list: tasklist1, workflow_type_name: workflowType1, workflow_timeout: 0, decision_task_timeout: 0, execution_context: [], state: 0, ` +
					`close_status: 0, last_first_event_id: 0, last_event_task_id: 0, next_event_id: 0, last_processed_event: 0, start_time: 2023-12-19T22:08:41Z, ` +
					`last_updated_time: 2023-12-19T22:09:41Z, create_request_id: , signal_count: 0, history_size: 0, decision_version: 0, ` +
					`decision_schedule_id: 2, decision_started_id: 3, decision_request_id: , decision_timeout: 0, decision_attempt: 0, ` +
					`decision_timestamp: -6795364578871345152, decision_scheduled_timestamp: -6795364578871345152, decision_original_scheduled_timestamp: -6795364578871345152, ` +
					`cancel_requested: false, cancel_request_id: , sticky_task_list: , sticky_schedule_to_start_timeout: 0,client_library_version: , client_feature_version: , ` +
					`client_impl: , auto_reset_points: [], auto_reset_points_encoding: , attempt: 0, has_retry_policy: false, init_interval: 0, ` +
					`backoff_coefficient: 0, max_interval: 0, expiration_time: 0001-01-01T00:00:00Z, max_attempts: 0, non_retriable_errors: [], ` +
					`event_store_version: 2, branch_token: [], cron_schedule: , expiration_seconds: 0, search_attributes: map[], memo: map[], partition_config: map[] ` +
					`}, 0, 946684800000, -10, [], , {version: 0, flavor: 0, value: [] }, 0, 0) IF NOT EXISTS `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := createWorkflowExecution(batch, tc.shardID, tc.domainID, tc.workflowID, tc.execution)
			if err != nil {
				t.Fatalf("createWorkflowExecution failed, err: %v", err)
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateOrUpdateWorkflowExecution(t *testing.T) {
	tests := []struct {
		desc       string
		shardID    int
		domainID   string
		workflowID string
		execution  *nosqlplugin.CurrentWorkflowWriteRequest
		// expectations
		wantQueries []string
		wantErr     bool
	}{
		{
			desc:       "unknown write mode",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: 255, // unknown write mode
			},
			wantErr: true,
		},
		{
			desc:       "CurrentWorkflowWriteModeNoop",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: nosqlplugin.CurrentWorkflowWriteModeNoop,
			},
			wantQueries: nil,
		},
		{
			desc:       "CurrentWorkflowWriteModeInsert",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: nosqlplugin.CurrentWorkflowWriteModeInsert,
				Row: nosqlplugin.CurrentWorkflowRow{
					RunID:           "runid1",
					CreateRequestID: "createRequestID1",
					State:           persistence.WorkflowStateCreated,
					CloseStatus:     persistence.WorkflowCloseStatusNone,
				},
			},
			wantQueries: []string{
				`INSERT INTO executions (shard_id, type, domain_id, workflow_id, run_id, visibility_ts, task_id, current_run_id, execution, workflow_last_write_version, workflow_state) ` +
					`VALUES(1000, 1, domain1, workflow1, 30000000-0000-f000-f000-000000000001, 946684800000, -10, runid1, ` +
					`{run_id: runid1, create_request_id: createRequestID1, state: 0, close_status: 0}, 0, 0) ` +
					`IF NOT EXISTS USING TTL 0 `,
			},
		},
		{
			desc:       "CurrentWorkflowWriteModeUpdate and condition missing",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: nosqlplugin.CurrentWorkflowWriteModeUpdate,
				Row: nosqlplugin.CurrentWorkflowRow{
					RunID:           "runid1",
					CreateRequestID: "createRequestID1",
					State:           persistence.WorkflowStateCreated,
					CloseStatus:     persistence.WorkflowCloseStatusNone,
				},
			},
			wantErr: true,
		},
		{
			desc:       "CurrentWorkflowWriteModeUpdate and condition runid missing",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: nosqlplugin.CurrentWorkflowWriteModeUpdate,
				Condition: &nosqlplugin.CurrentWorkflowWriteCondition{
					CurrentRunID: nil,
				},
				Row: nosqlplugin.CurrentWorkflowRow{
					RunID:           "runid1",
					CreateRequestID: "createRequestID1",
					State:           persistence.WorkflowStateCreated,
					CloseStatus:     persistence.WorkflowCloseStatusNone,
				},
			},
			wantErr: true,
		},
		{
			desc:       "CurrentWorkflowWriteModeUpdate with LastWriteVersion",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: nosqlplugin.CurrentWorkflowWriteModeUpdate,
				Condition: &nosqlplugin.CurrentWorkflowWriteCondition{
					CurrentRunID:     common.StringPtr("runid1"),
					LastWriteVersion: common.Int64Ptr(1),
				},
				Row: nosqlplugin.CurrentWorkflowRow{
					RunID:           "runid1",
					CreateRequestID: "createRequestID1",
					State:           persistence.WorkflowStateCreated,
					CloseStatus:     persistence.WorkflowCloseStatusNone,
				},
			},
			wantQueries: []string{
				`UPDATE executions USING TTL 0 SET ` +
					`current_run_id = runid1, ` +
					`execution = {run_id: runid1, create_request_id: createRequestID1, state: 0, close_status: 0}, ` +
					`workflow_last_write_version = 0, workflow_state = 0 ` +
					`WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = 30000000-0000-f000-f000-000000000001 and visibility_ts = 946684800000 and task_id = -10 ` +
					`IF current_run_id = runid1 `,
			},
		},
		{
			desc:       "CurrentWorkflowWriteModeUpdate",
			shardID:    1000,
			domainID:   "domain1",
			workflowID: "workflow1",
			execution: &nosqlplugin.CurrentWorkflowWriteRequest{
				WriteMode: nosqlplugin.CurrentWorkflowWriteModeUpdate,
				Condition: &nosqlplugin.CurrentWorkflowWriteCondition{
					CurrentRunID: common.StringPtr("runid1"),
				},
				Row: nosqlplugin.CurrentWorkflowRow{
					RunID:           "runid1",
					CreateRequestID: "createRequestID1",
					State:           persistence.WorkflowStateCreated,
					CloseStatus:     persistence.WorkflowCloseStatusNone,
				},
			},
			wantQueries: []string{
				`UPDATE executions USING TTL 0 SET ` +
					`current_run_id = runid1, ` +
					`execution = {run_id: runid1, create_request_id: createRequestID1, state: 0, close_status: 0}, ` +
					`workflow_last_write_version = 0, workflow_state = 0 ` +
					`WHERE ` +
					`shard_id = 1000 and type = 1 and domain_id = domain1 and workflow_id = workflow1 and ` +
					`run_id = 30000000-0000-f000-f000-000000000001 and visibility_ts = 946684800000 and task_id = -10 ` +
					`IF current_run_id = runid1 `,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			batch := &fakeBatch{}

			err := createOrUpdateCurrentWorkflow(batch, tc.shardID, tc.domainID, tc.workflowID, tc.execution)
			gotErr := (err != nil)
			if gotErr != tc.wantErr {
				t.Fatalf("Got error: %v, want?: %v", err, tc.wantErr)
			}
			if gotErr {
				return
			}

			if diff := cmp.Diff(tc.wantQueries, batch.queries); diff != "" {
				t.Fatalf("Query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMustConvertToSlice(t *testing.T) {
	tests := []struct {
		desc      string
		in        interface{}
		want      []interface{}
		wantPanic bool
	}{
		{
			desc:      "nil",
			in:        nil,
			wantPanic: true,
		},
		{
			desc: "empty",
			in:   []string{},
			want: []interface{}{},
		},
		{
			desc: "slice",
			in:   []string{"a", "b", "c"},
			want: []interface{}{"a", "b", "c"},
		},
		{
			desc: "array",
			in:   [3]string{"a", "b", "c"},
			want: []interface{}{"a", "b", "c"},
		},
		{
			desc:      "non-slice",
			in:        "a",
			wantPanic: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			defer func() {
				r := recover()
				if (r != nil) != tc.wantPanic {
					t.Fatalf("Got panic: %v, want panic?: %v", r, tc.wantPanic)
				}
			}()

			got := mustConvertToSlice(tc.in)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Slice mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func errDiff(want, got error) string {
	wantCondFailure, wantOk := want.(*nosqlplugin.WorkflowOperationConditionFailure)
	gotCondFailure, gotOk := got.(*nosqlplugin.WorkflowOperationConditionFailure)
	if wantOk && gotOk {
		arg1 := trimWorkflowConditionalFailureErr(wantCondFailure)
		arg2 := trimWorkflowConditionalFailureErr(gotCondFailure)
		return cmp.Diff(arg1, arg2)
	}

	wantMsg := ""
	if want != nil {
		wantMsg = want.Error()
	}
	gotMsg := ""
	if got != nil {
		gotMsg = got.Error()
	}
	return cmp.Diff(wantMsg, gotMsg)
}

func trimWorkflowConditionalFailureErr(condFailure *nosqlplugin.WorkflowOperationConditionFailure) any {
	trimColumnsPart(condFailure.CurrentWorkflowConditionFailInfo)
	trimColumnsPart(condFailure.UnknownConditionFailureDetails)
	if condFailure.WorkflowExecutionAlreadyExists != nil {
		trimColumnsPart(&condFailure.WorkflowExecutionAlreadyExists.OtherInfo)
	}
	return condFailure
}

func trimColumnsPart(s *string) {
	if s == nil {
		return
	}
	re := regexp.MustCompile(`, columns: \(.*\)`)
	trimmed := re.ReplaceAllString(*s, "")
	*s = trimmed
}