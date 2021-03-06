// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"

	"github.com/pingcap/tidb/config"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
)

// DeleteExec represents a delete executor.
// See https://dev.mysql.com/doc/refman/5.7/en/delete.html
type DeleteExec struct {
	baseExecutor

	IsMultiTable bool
	tblID2Table  map[int64]table.Table

	// tblColPosInfos stores relationship between column ordinal to its table handle.
	// the columns ordinals is present in ordinal range format, @see plannercore.TblColPosInfos
	tblColPosInfos plannercore.TblColPosInfoSlice
}

// Next implements the Executor Next interface.
func (e *DeleteExec) Next(ctx context.Context, req *chunk.Chunk) error {
	req.Reset()
	if e.IsMultiTable {
		return e.deleteMultiTablesByChunk(ctx)
	}
	return e.deleteSingleTableByChunk(ctx)
}

func (e *DeleteExec) deleteOneRow(tbl table.Table, handleIndex int, isExtraHandle bool, row []types.Datum) error {
	end := len(row)
	if isExtraHandle {
		end--
	}
	handle := row[handleIndex].GetInt64()
	err := e.removeRow(e.ctx, tbl, handle, row[:end])
	if err != nil {
		return err
	}

	return nil
}

func (e *DeleteExec) deleteSingleTableByChunk(ctx context.Context) error {
	var (
		tbl           table.Table
		isExtrahandle bool
		handleIndex   int
		rowCount      int
	)
	for _, info := range e.tblColPosInfos {
		tbl = e.tblID2Table[info.TblID]
		handleIndex = info.HandleOrdinal
		isExtrahandle = handleIsExtra(e.children[0].Schema().Columns[info.HandleOrdinal])
	}

	// If tidb_batch_delete is ON and not in a transaction, we could use BatchDelete mode.
	batchDelete := e.ctx.GetSessionVars().BatchDelete && !e.ctx.GetSessionVars().InTxn() && config.GetGlobalConfig().EnableBatchDML
	batchDMLSize := e.ctx.GetSessionVars().DMLBatchSize
	fields := retTypes(e.children[0])
	chk := newFirstChunk(e.children[0])
	for {
		iter := chunk.NewIterator4Chunk(chk)

		err := Next(ctx, e.children[0], chk)
		if err != nil {
			return err
		}
		if chk.NumRows() == 0 {
			break
		}

		for chunkRow := iter.Begin(); chunkRow != iter.End(); chunkRow = iter.Next() {
			if batchDelete && rowCount >= batchDMLSize {
				if err = e.ctx.StmtCommit(nil); err != nil {
					return err
				}
				if err = e.ctx.NewTxn(ctx); err != nil {
					// We should return a special error for batch insert.
					return ErrBatchInsertFail.GenWithStack("BatchDelete failed with error: %v", err)
				}
				rowCount = 0
			}

			datumRow := chunkRow.GetDatumRow(fields)
			err = e.deleteOneRow(tbl, handleIndex, isExtrahandle, datumRow)
			if err != nil {
				return err
			}
			rowCount++
		}
		chk = chunk.Renew(chk, e.maxChunkSize)
	}

	return nil
}

func (e *DeleteExec) composeTblRowMap(tblRowMap tableRowMapType, colPosInfos []plannercore.TblColPosInfo, joinedRow []types.Datum) {
	// iterate all the joined tables, and got the copresonding rows in joinedRow.
	for _, info := range colPosInfos {
		if tblRowMap[info.TblID] == nil {
			tblRowMap[info.TblID] = make(map[int64][]types.Datum)
		}
		handle := joinedRow[info.HandleOrdinal].GetInt64()
		// tblRowMap[info.TblID][handle] hold the row datas binding to this table and this handle.
		tblRowMap[info.TblID][handle] = joinedRow[info.Start:info.End]
	}
}

func (e *DeleteExec) deleteMultiTablesByChunk(ctx context.Context) error {
	colPosInfos := e.tblColPosInfos
	tblRowMap := make(tableRowMapType)
	fields := retTypes(e.children[0])
	chk := newFirstChunk(e.children[0])
	for {
		iter := chunk.NewIterator4Chunk(chk)
		err := Next(ctx, e.children[0], chk)
		if err != nil {
			return err
		}
		if chk.NumRows() == 0 {
			break
		}

		for joinedChunkRow := iter.Begin(); joinedChunkRow != iter.End(); joinedChunkRow = iter.Next() {
			joinedDatumRow := joinedChunkRow.GetDatumRow(fields)
			e.composeTblRowMap(tblRowMap, colPosInfos, joinedDatumRow)
		}
		chk = chunk.Renew(chk, e.maxChunkSize)
	}

	return e.removeRowsInTblRowMap(tblRowMap)
}

func (e *DeleteExec) removeRowsInTblRowMap(tblRowMap tableRowMapType) error {
	for id, rowMap := range tblRowMap {
		for handle, data := range rowMap {
			err := e.removeRow(e.ctx, e.tblID2Table[id], handle, data)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (e *DeleteExec) removeRow(ctx sessionctx.Context, t table.Table, h int64, data []types.Datum) error {
	err := t.RemoveRecord(ctx, h, data)
	if err != nil {
		return err
	}
	ctx.GetSessionVars().StmtCtx.AddAffectedRows(1)
	return nil
}

// Close implements the Executor Close interface.
func (e *DeleteExec) Close() error {
	return e.children[0].Close()
}

// Open implements the Executor Open interface.
func (e *DeleteExec) Open(ctx context.Context) error {
	return e.children[0].Open(ctx)
}

// tableRowMapType is a map for unique (Table, Row) pair. key is the tableID.
// the key in map[int64]Row is the joined table handle, which represent a unique reference row.
// the value in map[int64]Row is the deleting row.
type tableRowMapType map[int64]map[int64][]types.Datum
