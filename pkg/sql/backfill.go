// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Tamir Duberstein (tamird@gmail.com)

package sql

import (
	"sort"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/distsqlrun"
	"github.com/cockroachdb/cockroach/pkg/sql/jobs"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

const (
	// TODO(vivek): Replace these constants with a runtime budget for the
	// operation chunk involved.

	// columnTruncateAndBackfillChunkSize is the maximum number of columns
	// processed per chunk during column truncate or backfill.
	columnTruncateAndBackfillChunkSize = 200

	// indexTruncateChunkSize is the maximum number of index entries truncated
	// per chunk during an index truncation. This value is larger than the
	// other chunk constants because the operation involves only running a
	// DeleteRange().
	indexTruncateChunkSize = 600

	// indexBackfillChunkSize is the maximum number index entries backfilled
	// per chunk during an index backfill. The index backfill involves a table
	// scan, and a number of individual ops presented in a batch. This value
	// is smaller than ColumnTruncateAndBackfillChunkSize, because it involves
	// a number of individual index row updates that can be scattered over
	// many ranges.
	indexBackfillChunkSize = 100

	// checkpointInterval is the interval after which a checkpoint of the
	// schema change is posted.
	checkpointInterval = 10 * time.Second
)

var _ sort.Interface = columnsByID{}
var _ sort.Interface = indexesByID{}

type columnsByID []sqlbase.ColumnDescriptor

func (cds columnsByID) Len() int {
	return len(cds)
}
func (cds columnsByID) Less(i, j int) bool {
	return cds[i].ID < cds[j].ID
}
func (cds columnsByID) Swap(i, j int) {
	cds[i], cds[j] = cds[j], cds[i]
}

type indexesByID []sqlbase.IndexDescriptor

func (ids indexesByID) Len() int {
	return len(ids)
}
func (ids indexesByID) Less(i, j int) bool {
	return ids[i].ID < ids[j].ID
}
func (ids indexesByID) Swap(i, j int) {
	ids[i], ids[j] = ids[j], ids[i]
}

func (sc *SchemaChanger) getChunkSize(chunkSize int64) int64 {
	if sc.testingKnobs.BackfillChunkSize > 0 {
		return sc.testingKnobs.BackfillChunkSize
	}
	return chunkSize
}

// runBackfill runs the backfill for the schema changer.
func (sc *SchemaChanger) runBackfill(
	ctx context.Context, lease *sqlbase.TableDescriptor_SchemaChangeLease, evalCtx parser.EvalContext,
) error {
	if sc.testingKnobs.RunBeforeBackfill != nil {
		if err := sc.testingKnobs.RunBeforeBackfill(); err != nil {
			return err
		}
	}
	if err := sc.ExtendLease(ctx, lease); err != nil {
		return err
	}

	// Mutations are applied in a FIFO order. Only apply the first set of
	// mutations. Collect the elements that are part of the mutation.
	var droppedIndexDescs []sqlbase.IndexDescriptor
	var addedIndexDescs []sqlbase.IndexDescriptor
	// Indexes within the Mutations slice for checkpointing.
	mutationSentinel := -1
	var droppedIndexMutationIdx int

	var tableDesc *sqlbase.TableDescriptor
	if err := sc.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		var err error
		tableDesc, err = sqlbase.GetTableDescFromID(ctx, txn, sc.tableID)
		return err
	}); err != nil {
		return err
	}
	// Short circuit the backfill if the table has been deleted.
	if tableDesc.Dropped() {
		return nil
	}
	version := tableDesc.Version

	log.VEventf(ctx, 0, "Running backfill for %q, v=%d, m=%d",
		tableDesc.Name, tableDesc.Version, sc.mutationID)

	needColumnBackfill := false
	for i, m := range tableDesc.Mutations {
		if m.MutationID != sc.mutationID {
			break
		}
		switch m.Direction {
		case sqlbase.DescriptorMutation_ADD:
			switch t := m.Descriptor_.(type) {
			case *sqlbase.DescriptorMutation_Column:
				desc := m.GetColumn()
				if desc.DefaultExpr != nil || !desc.Nullable {
					needColumnBackfill = true
				}
			case *sqlbase.DescriptorMutation_Index:
				addedIndexDescs = append(addedIndexDescs, *t.Index)
			default:
				return errors.Errorf("unsupported mutation: %+v", m)
			}

		case sqlbase.DescriptorMutation_DROP:
			switch t := m.Descriptor_.(type) {
			case *sqlbase.DescriptorMutation_Column:
				needColumnBackfill = true
			case *sqlbase.DescriptorMutation_Index:
				droppedIndexDescs = append(droppedIndexDescs, *t.Index)
				if droppedIndexMutationIdx == mutationSentinel {
					droppedIndexMutationIdx = i
				}
			default:
				return errors.Errorf("unsupported mutation: %+v", m)
			}
		}
	}

	// First drop indexes, then add/drop columns, and only then add indexes.

	// Drop indexes.
	if err := sc.truncateIndexes(
		ctx, lease, version, droppedIndexDescs, droppedIndexMutationIdx,
	); err != nil {
		return err
	}

	// Add and drop columns.
	if needColumnBackfill {
		if err := sc.truncateAndBackfillColumns(ctx, evalCtx, lease, version); err != nil {
			return err
		}
	}

	// Add new indexes.
	if len(addedIndexDescs) > 0 {
		if err := sc.backfillIndexes(ctx, evalCtx, lease, version); err != nil {
			return err
		}
	}

	return nil
}

func (sc *SchemaChanger) maybeWriteResumeSpan(
	ctx context.Context,
	txn *client.Txn,
	version sqlbase.DescriptorVersion,
	resume roachpb.Span,
	mutationIdx int,
	lastCheckpoint *time.Time,
) error {
	checkpointInterval := checkpointInterval
	if sc.testingKnobs.WriteCheckpointInterval > 0 {
		checkpointInterval = sc.testingKnobs.WriteCheckpointInterval
	}
	if timeutil.Since(*lastCheckpoint) < checkpointInterval {
		return nil
	}
	tableDesc, err := sqlbase.GetTableDescFromID(ctx, txn, sc.tableID)
	if err != nil {
		return err
	}
	if tableDesc.Version != version {
		return errors.Errorf("table version mismatch: %d, expected: %d", tableDesc.Version, version)
	}
	if len(tableDesc.Mutations[mutationIdx].ResumeSpans) > 0 {
		tableDesc.Mutations[mutationIdx].ResumeSpans[0] = resume
	} else {
		tableDesc.Mutations[mutationIdx].ResumeSpans = append(tableDesc.Mutations[mutationIdx].ResumeSpans, resume)
	}
	if err := txn.SetSystemConfigTrigger(); err != nil {
		return err
	}
	if err := txn.Put(
		ctx,
		sqlbase.MakeDescMetadataKey(tableDesc.GetID()),
		sqlbase.WrapDescriptor(tableDesc),
	); err != nil {
		return err
	}
	*lastCheckpoint = timeutil.Now()
	return nil
}

func (sc *SchemaChanger) getTableVersion(
	ctx context.Context, txn *client.Txn, tc *TableCollection, version sqlbase.DescriptorVersion,
) (*sqlbase.TableDescriptor, error) {
	tableDesc, err := tc.getTableVersionByID(ctx, txn, sc.tableID)
	if err != nil {
		return nil, err
	}
	if version != tableDesc.Version {
		return nil, errors.Errorf("table version mismatch: %d, expected=%d", tableDesc.Version, version)
	}
	return tableDesc, nil
}

func (sc *SchemaChanger) truncateIndexes(
	ctx context.Context,
	lease *sqlbase.TableDescriptor_SchemaChangeLease,
	version sqlbase.DescriptorVersion,
	dropped []sqlbase.IndexDescriptor,
	mutationIdx int,
) error {
	chunkSize := sc.getChunkSize(indexTruncateChunkSize)
	if sc.testingKnobs.BackfillChunkSize > 0 {
		chunkSize = sc.testingKnobs.BackfillChunkSize
	}
	alloc := &sqlbase.DatumAlloc{}
	for _, desc := range dropped {
		var resume roachpb.Span
		lastCheckpoint := timeutil.Now()
		for row, done := int64(0), false; !done; row += chunkSize {
			// First extend the schema change lease.
			if err := sc.ExtendLease(ctx, lease); err != nil {
				return err
			}

			resumeAt := resume
			if log.V(2) {
				log.Infof(ctx, "drop index (%d, %d) at row: %d, span: %s",
					sc.tableID, sc.mutationID, row, resume)
			}
			if err := sc.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
				if sc.testingKnobs.RunBeforeBackfillChunk != nil {
					if err := sc.testingKnobs.RunBeforeBackfillChunk(resume); err != nil {
						return err
					}
				}
				if sc.testingKnobs.RunAfterBackfillChunk != nil {
					defer sc.testingKnobs.RunAfterBackfillChunk()
				}

				// TODO(vivek): See comment in backfillIndexesChunk.
				if err := txn.SetSystemConfigTrigger(); err != nil {
					return err
				}

				tc := &TableCollection{leaseMgr: sc.leaseMgr}
				defer tc.releaseTables(ctx)
				tableDesc, err := sc.getTableVersion(ctx, txn, tc, version)
				if err != nil {
					return err
				}

				rd, err := sqlbase.MakeRowDeleter(txn, tableDesc, nil, nil, false, alloc)
				if err != nil {
					return err
				}
				td := tableDeleter{rd: rd, alloc: alloc}
				if err := td.init(txn); err != nil {
					return err
				}
				resume, err = td.deleteIndex(
					ctx, &desc, resumeAt, chunkSize, false, /* traceKV */
				)
				if err != nil {
					return err
				}
				if err := sc.maybeWriteResumeSpan(ctx, txn, version, resume, mutationIdx, &lastCheckpoint); err != nil {
					return err
				}
				done = resume.Key == nil
				return nil
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

type backfillType int

const (
	_ backfillType = iota
	columnBackfill
	indexBackfill
)

// getMutationToBackfill returns the the first mutation enqueued on the table
// descriptor that passes the input mutationFilter.
//
// Returns nil if the backfill is complete.
func (sc *SchemaChanger) getMutationToBackfill(
	ctx context.Context,
	version sqlbase.DescriptorVersion,
	backfillType backfillType,
	filter distsqlrun.MutationFilter,
) (*sqlbase.DescriptorMutation, error) {
	var mutation *sqlbase.DescriptorMutation
	err := sc.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		mutation = nil
		tableDesc, err := sqlbase.GetTableDescFromID(ctx, txn, sc.tableID)
		if err != nil {
			return err
		}
		if tableDesc.Version != version {
			return errors.Errorf("table version mismatch: %d, expected: %d", tableDesc.Version, version)
		}
		if len(tableDesc.Mutations) > 0 {
			mutationID := tableDesc.Mutations[0].MutationID
			for i := range tableDesc.Mutations {
				if tableDesc.Mutations[i].MutationID != mutationID {
					break
				}
				if filter(tableDesc.Mutations[i]) {
					mutation = &tableDesc.Mutations[i]
					break
				}
			}
		}
		return nil
	})
	return mutation, err
}

// nRanges returns the number of ranges that cover a set of spans.
func (sc *SchemaChanger) nRanges(
	ctx context.Context, txn *client.Txn, spans []roachpb.Span,
) (int, error) {
	spanResolver := sc.distSQLPlanner.spanResolver.NewSpanResolverIterator(txn)
	rangeIds := make(map[int64]struct{})
	for _, span := range spans {
		// For each span, iterate the spanResolver until it's exhausted, storing
		// the found range ids in the map to de-duplicate them.
		spanResolver.Seek(ctx, span, kv.Ascending)
		for {
			if !spanResolver.Valid() {
				return 0, spanResolver.Error()
			}
			rangeIds[int64(spanResolver.Desc().RangeID)] = struct{}{}
			if !spanResolver.NeedAnother() {
				break
			}
			spanResolver.Next(ctx)
		}
	}

	return len(rangeIds), nil
}

// distBackfill runs (or continues) a backfill for the first mutation
// enqueued on the SchemaChanger's table descriptor that passes the input
// MutationFilter.
func (sc *SchemaChanger) distBackfill(
	ctx context.Context,
	evalCtx parser.EvalContext,
	lease *sqlbase.TableDescriptor_SchemaChangeLease,
	version sqlbase.DescriptorVersion,
	backfillType backfillType,
	backfillChunkSize int64,
	filter distsqlrun.MutationFilter,
) error {
	duration := checkpointInterval
	if sc.testingKnobs.WriteCheckpointInterval > 0 {
		duration = sc.testingKnobs.WriteCheckpointInterval
	}
	chunkSize := sc.getChunkSize(backfillChunkSize)

	origNRanges := -1
	origFractionCompleted := sc.job.Payload().FractionCompleted
	fractionLeft := 1 - origFractionCompleted
	for {
		// Repeat until getMutationToBackfill returns a mutation with no remaining
		// ResumeSpans, indicating that the backfill is complete.
		mutation, err := sc.getMutationToBackfill(ctx, version, backfillType, filter)
		if err != nil {
			return err
		}
		if mutation == nil {
			break
		}
		spans := mutation.ResumeSpans
		if len(spans) <= 0 {
			break
		}

		if err := sc.ExtendLease(ctx, lease); err != nil {
			return err
		}
		log.VEventf(ctx, 2, "backfill: process %+v spans", spans)
		if err := sc.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
			// Report schema change progress. We define progress at this point
			// as the the fraction of fully-backfilled ranges of the primary index of
			// the table being scanned. Since we may have already modified the
			// fraction completed of our job from the 10% allocated to completing the
			// schema change state machine or from a previous backfill attempt,
			// we scale that fraction of ranges completed by the remaining fraction
			// of the job's progress bar.
			nRanges, err := sc.nRanges(ctx, txn, spans)
			if err != nil {
				return err
			}
			if origNRanges == -1 {
				origNRanges = nRanges
			}

			if nRanges < origNRanges {
				fractionRangesFinished := float32(origNRanges-nRanges) / float32(origNRanges)
				fractionCompleted := origFractionCompleted + fractionLeft*fractionRangesFinished
				if err := sc.job.Progressed(ctx, fractionCompleted, jobs.Noop); err != nil {
					log.Infof(ctx, "Ignoring error reporting progress %f for job %d: %v", fractionCompleted, *sc.job.ID(), err)
				}
			}

			tc := &TableCollection{leaseMgr: sc.leaseMgr}
			// Use a leased table descriptor for the backfill.
			defer tc.releaseTables(ctx)
			tableDesc, err := sc.getTableVersion(ctx, txn, tc, version)
			if err != nil {
				return err
			}
			// otherTableDescs contains any other table descriptors required by the
			// backfiller processor.
			var otherTableDescs []sqlbase.TableDescriptor
			if backfillType == columnBackfill {
				fkTables := sqlbase.TablesNeededForFKs(*tableDesc, sqlbase.CheckUpdates)
				for k := range fkTables {
					table, err := tc.getTableVersionByID(ctx, txn, k)
					if err != nil {
						return err
					}
					otherTableDescs = append(otherTableDescs, *table)
				}
			}
			// TODO(andrei): pass the right caches. I think this will crash without
			// them.
			recv, err := makeDistSQLReceiver(
				ctx,
				nil, /* sink */
				nil, /* rangeCache */
				nil, /* leaseCache */
				nil, /* txn - the flow does not run wholly in a txn */
				// updateClock - the flow will not generate errors with time signal.
				// TODO(andrei): plumb a clock update handler here regardless of whether
				// it will actually be used or not.
				nil,
			)
			if err != nil {
				return err
			}
			planCtx := sc.distSQLPlanner.NewPlanningCtx(ctx, txn)
			plan, err := sc.distSQLPlanner.CreateBackfiller(
				&planCtx, backfillType, *tableDesc, duration, chunkSize, spans, otherTableDescs, sc.readAsOf,
			)
			if err != nil {
				return err
			}
			if err := sc.distSQLPlanner.Run(&planCtx, txn, &plan, &recv, evalCtx); err != nil {
				return err
			}

			return recv.err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (sc *SchemaChanger) backfillIndexes(
	ctx context.Context,
	evalCtx parser.EvalContext,
	lease *sqlbase.TableDescriptor_SchemaChangeLease,
	version sqlbase.DescriptorVersion,
) error {
	// Pick a read timestamp for our index backfill, or reuse the previously
	// stored one.
	if err := sc.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		details := *sc.job.WithTxn(txn).Payload().Details.(*jobs.Payload_SchemaChange).SchemaChange
		if details.ReadAsOf == (hlc.Timestamp{}) {
			details.ReadAsOf = txn.OrigTimestamp()
			if err := sc.job.WithTxn(txn).SetDetails(ctx, details); err != nil {
				log.Warningf(ctx, "failed to store readAsOf on job %v after completing state machine: %v",
					sc.job.ID(), err)
			}
		}
		sc.readAsOf = details.ReadAsOf
		return nil
	}); err != nil {
		return err
	}

	if fn := sc.testingKnobs.RunBeforeIndexBackfill; fn != nil {
		fn()
	}

	return sc.distBackfill(
		ctx, evalCtx, lease, version, indexBackfill, indexBackfillChunkSize,
		distsqlrun.IndexMutationFilter)
}

func (sc *SchemaChanger) truncateAndBackfillColumns(
	ctx context.Context,
	evalCtx parser.EvalContext,
	lease *sqlbase.TableDescriptor_SchemaChangeLease,
	version sqlbase.DescriptorVersion,
) error {
	return sc.distBackfill(
		ctx, evalCtx,
		lease, version, columnBackfill, columnTruncateAndBackfillChunkSize,
		distsqlrun.ColumnMutationFilter)
}
