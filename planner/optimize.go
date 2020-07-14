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

package planner

import (
	"context"

	"github.com/pingcap/parser/ast"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/planner/cascades"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/privilege"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

// GetPreparedStmt extract the prepared statement from the execute statement.
func GetPreparedStmt(stmt *ast.ExecuteStmt, vars *variable.SessionVars) (ast.StmtNode, error) {
	var ok bool
	execID := stmt.ExecID
	if stmt.Name != "" {
		if execID, ok = vars.PreparedStmtNameToID[stmt.Name]; !ok {
			return nil, plannercore.ErrStmtNotFound
		}
	}
	if prepared, ok := vars.PreparedStmts[execID]; ok {
		return prepared.Stmt, nil
	}
	return nil, plannercore.ErrStmtNotFound
}

// IsReadOnly check whether the ast.Node is a read only statement.
func IsReadOnly(node ast.Node, vars *variable.SessionVars) bool {
	if execStmt, ok := node.(*ast.ExecuteStmt); ok {
		s, err := GetPreparedStmt(execStmt, vars)
		if err != nil {
			logutil.Logger(context.Background()).Error("getPreparedStmt failed", zap.Error(err))
			return false
		}
		return ast.IsReadOnly(s)
	}
	return ast.IsReadOnly(node)
}

// Optimize does optimization and creates a Plan.
// The node must be prepared first.
func Optimize(ctx context.Context, sctx sessionctx.Context, node ast.Node, is infoschema.InfoSchema) (plannercore.Plan, error) {
	sessVars := sctx.GetSessionVars()

	// Because for write stmt, TiFlash has a different results when lock the data in point get plan. We ban the TiFlash
	// engine in not read only stmt.
	if _, isolationReadContainTiFlash := sessVars.IsolationReadEngines[kv.TiFlash]; isolationReadContainTiFlash && !IsReadOnly(node, sessVars) {
		delete(sessVars.IsolationReadEngines, kv.TiFlash)
		defer func() {
			sessVars.IsolationReadEngines[kv.TiFlash] = struct{}{}
		}()
	}

	if _, containTiKV := sctx.GetSessionVars().GetIsolationReadEngines()[kv.TiKV]; containTiKV {
		fp := plannercore.TryFastPlan(sctx, node)
		if fp != nil {
			if !isPointGetWithoutDoubleRead(sctx, fp) {
				sctx.PrepareTxnFuture(ctx)
			}
			return fp, nil
		}
	}

	sctx.PrepareTxnFuture(ctx)

	// build logical plan
	sctx.GetSessionVars().PlanID = 0
	sctx.GetSessionVars().PlanColumnID = 0
	hintProcessor := &plannercore.BlockHintProcessor{Ctx: sctx}
	node.Accept(hintProcessor)
	builder := plannercore.NewPlanBuilder(sctx, is, hintProcessor)
	p, err := builder.Build(ctx, node)
	if err != nil {
		return nil, err
	}

	sctx.GetSessionVars().StmtCtx.Tables = builder.GetDBTableInfo()
	activeRoles := sctx.GetSessionVars().ActiveRoles
	// Check privilege. Maybe it's better to move this to the Preprocess, but
	// we need the table information to check privilege, which is collected
	// into the visitInfo in the logical plan builder.
	if pm := privilege.GetPrivilegeManager(sctx); pm != nil {
		if err := plannercore.CheckPrivilege(activeRoles, pm, builder.GetVisitInfo()); err != nil {
			return nil, err
		}
	}

	if err := plannercore.CheckTableLock(sctx, is, builder.GetVisitInfo()); err != nil {
		return nil, err
	}

	// Handle the execute statement.
	if execPlan, ok := p.(*plannercore.Execute); ok {
		err := execPlan.OptimizePreparedPlan(ctx, sctx, is)
		return p, err
	}

	// Handle the non-logical plan statement.
	logic, isLogicalPlan := p.(plannercore.LogicalPlan)
	if !isLogicalPlan {
		return p, nil
	}

	// Handle the logical plan statement, use cascades planner if enabled.
	if sctx.GetSessionVars().EnableCascadesPlanner {
		return cascades.FindBestPlan(sctx, logic)
	}
	return plannercore.DoOptimize(ctx, builder.GetOptFlag(), logic)
}

// isPointGetWithoutDoubleRead returns true when meets following conditions:
//  1. ctx is auto commit tagged.
//  2. plan is point get by pk.
func isPointGetWithoutDoubleRead(ctx sessionctx.Context, p plannercore.Plan) bool {
	if !ctx.GetSessionVars().IsAutocommit() {
		return false
	}

	v, ok := p.(*plannercore.PointGetPlan)
	return ok && v.IndexInfo == nil
}

func init() {
	plannercore.OptimizeAstNode = Optimize
}
