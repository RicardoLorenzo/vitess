/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planbuilder

import (
	"errors"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/vindexes"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

// buildUpdatePlan builds the instructions for an UPDATE statement.
func buildUpdatePlan(upd *sqlparser.Update, vschema VSchema) (*engine.Update, error) {
	eupd := &engine.Update{
		Query:               generateQuery(upd),
		ChangedVindexValues: make(map[string][]sqltypes.PlanValue),
	}
	bldr, err := processTableExprs(upd.TableExprs, vschema)
	if err != nil {
		return nil, err
	}
	rb, ok := bldr.(*route)
	if !ok {
		return nil, errors.New("unsupported: multi-table update statement in sharded keyspace")
	}
	if rb.ERoute.TargetDestination != nil {
		return nil, errors.New("unsupported: UPDATE with a target destination")
	}
	eupd.Keyspace = rb.ERoute.Keyspace
	if !eupd.Keyspace.Sharded {
		// We only validate non-table subexpressions because the previous analysis has already validated them.
		if !validateSubquerySamePlan(rb.ERoute.Keyspace.Name, rb, vschema, upd.Exprs, upd.Where, upd.OrderBy, upd.Limit) {
			return nil, errors.New("unsupported: sharded subqueries in DML")
		}
		eupd.Opcode = engine.UpdateUnsharded
		return eupd, nil
	}

	if hasSubquery(upd) {
		return nil, errors.New("unsupported: subqueries in sharded DML")
	}
	if len(rb.Symtab().tables) != 1 {
		return nil, errors.New("unsupported: multi-table update statement in sharded keyspace")
	}

	var vindexTable *vindexes.Table
	for _, tval := range rb.Symtab().tables {
		vindexTable = tval.vindexTable
	}
	eupd.Table = vindexTable
	if eupd.Table == nil {
		return nil, errors.New("internal error: table.vindexTable is mysteriously nil")
	}
	eupd.Vindex, eupd.Values, err = getDMLRouting(upd.Where, eupd.Table)
	if err != nil {
		return nil, err
	}
	eupd.Opcode = engine.UpdateEqual

	if eupd.ChangedVindexValues, err = buildChangedVindexesValues(eupd, upd, eupd.Table.ColumnVindexes); err != nil {
		return nil, err
	}
	if len(eupd.ChangedVindexValues) != 0 {
		eupd.OwnedVindexQuery = generateUpdateSubquery(upd, eupd.Table)
	}
	return eupd, nil
}

// buildChangedVindexesValues adds to the plan all the lookup vindexes that are changing.
// Updates can only be performed to secondary lookup vindexes with no complex expressions
// in the set clause.
func buildChangedVindexesValues(eupd *engine.Update, update *sqlparser.Update, colVindexes []*vindexes.ColumnVindex) (map[string][]sqltypes.PlanValue, error) {
	changedVindexes := make(map[string][]sqltypes.PlanValue)
	for i, vindex := range colVindexes {
		var vindexValues []sqltypes.PlanValue
		for _, vcol := range vindex.Columns {
			// Searching in order of columns in colvindex.
			found := false
			for _, assignment := range update.Exprs {
				if !vcol.Equal(assignment.Name.Name) {
					continue
				}
				if found {
					return nil, vterrors.Errorf(vtrpcpb.Code_INVALID_ARGUMENT, "column has duplicate set values: '%v'", assignment.Name.Name)
				}
				found = true
				pv, err := extractValueFromUpdate(assignment, vcol)
				if err != nil {
					return nil, err
				}
				vindexValues = append(vindexValues, pv)
			}
		}
		if len(vindexValues) == 0 {
			// Vindex not changing, continue
			continue
		}
		if len(vindexValues) != len(vindex.Columns) {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: update does not have values for all the columns in vindex (%s)", vindex.Name)
		}

		if update.Limit != nil && len(update.OrderBy) == 0 {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: Need to provide order by clause when using limit. Invalid update on vindex: %v", vindex.Name)
		}
		if i == 0 {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: You can't update primary vindex columns. Invalid update on vindex: %v", vindex.Name)
		}
		if _, ok := vindex.Vindex.(vindexes.Lookup); !ok {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: You can only update lookup vindexes. Invalid update on vindex: %v", vindex.Name)
		}
		if !vindex.Owned {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: You can only update owned vindexes. Invalid update on vindex: %v", vindex.Name)
		}
		changedVindexes[vindex.Name] = vindexValues
	}

	return changedVindexes, nil
}

func generateUpdateSubquery(upd *sqlparser.Update, table *vindexes.Table) string {
	buf := sqlparser.NewTrackedBuffer(nil)
	buf.WriteString("select ")
	for vIdx, cv := range table.Owned {
		for cIdx, column := range cv.Columns {
			if cIdx == 0 && vIdx == 0 {
				buf.Myprintf("%v", column)
			} else {
				buf.Myprintf(", %v", column)
			}
		}
	}
	buf.Myprintf(" from %v%v%v%v for update", table.Name, upd.Where, upd.OrderBy, upd.Limit)
	return buf.String()
}

// extractValueFromUpdate given an UpdateExpr attempts to extracts the Value
// it's holding. At the moment it only supports: StrVal, HexVal, IntVal, ValArg.
// If a complex expression is provided (e.g set name = name + 1), the update will be rejected.
func extractValueFromUpdate(upd *sqlparser.UpdateExpr, col sqlparser.ColIdent) (pv sqltypes.PlanValue, err error) {
	if !sqlparser.IsValue(upd.Expr) {
		err := vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: Only values are supported. Invalid update on column: %v", upd.Name.Name)
		return sqltypes.PlanValue{}, err
	}
	return sqlparser.NewPlanValue(upd.Expr)
}