package river

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/juju/errors"
	"github.com/siddontang/go-log/log"
	mysqlsink "github.com/zhaochuanyun/go-mysql-syncer/mysql"
	"github.com/zhaochuanyun/go-mysql/canal"
	"github.com/zhaochuanyun/go-mysql/mysql"
	"github.com/zhaochuanyun/go-mysql/replication"
	"github.com/zhaochuanyun/go-mysql/schema"
)

const (
	syncInsertDoc = iota
	syncDeleteDoc
	syncUpdateDoc
)

const (
	fieldTypeList = "list"
	// for the mysql int type to es date type
	// set the [rule.field] created_time = ",date"
	fieldTypeDate = "date"
)

const mysqlDateFormat = "2006-01-02"

type posSaver struct {
	pos   mysql.Position
	force bool
}

type eventHandler struct {
	r *River
}

func (h *eventHandler) OnRotate(e *replication.RotateEvent) error {
	pos := mysql.Position{
		Name: string(e.NextLogName),
		Pos:  uint32(e.Position),
	}

	h.r.syncCh <- posSaver{pos, true}

	return h.r.ctx.Err()
}

func (h *eventHandler) OnTableChanged(schema, table string) error {
	err := h.r.updateRule(schema, table)
	if err != nil && err != ErrRuleNotExist {
		return errors.Trace(err)
	}
	return nil
}

func (h *eventHandler) OnDDL(nextPos mysql.Position, _ *replication.QueryEvent) error {
	h.r.syncCh <- posSaver{nextPos, true}
	return h.r.ctx.Err()
}

func (h *eventHandler) OnXID(nextPos mysql.Position) error {
	h.r.syncCh <- posSaver{nextPos, false}
	return h.r.ctx.Err()
}

func (h *eventHandler) OnRow(e *canal.RowsEvent) error {
	rule, ok := h.r.rules[ruleKey(e.Table.Schema, e.Table.Name)]
	if !ok {
		return nil
	}

	var reqs []*mysqlsink.BulkRequest
	var err error
	switch e.Action {
	case canal.InsertAction:
		reqs, err = h.r.makeInsertRequest(rule, e.Rows)
	case canal.DeleteAction:
		reqs, err = h.r.makeDeleteRequest(rule, e.Rows)
	case canal.UpdateAction:
		reqs, err = h.r.makeUpdateRequest(rule, e.Rows)
	default:
		err = errors.Errorf("invalid rows action %s", e.Action)
	}

	if err != nil {
		h.r.cancel()
		return errors.Errorf("make %s ES request err %v, close sync", e.Action, err)
	}

	h.r.syncCh <- reqs

	return h.r.ctx.Err()
}

func (h *eventHandler) OnGTID(gtid mysql.GTIDSet) error {
	return nil
}

func (h *eventHandler) OnPosSynced(pos mysql.Position, force bool) error {
	return nil
}

func (h *eventHandler) String() string {
	return "ESRiverEventHandler"
}

func (r *River) syncLoop() {
	bulkSize := r.c.BulkSize
	if bulkSize == 0 {
		bulkSize = 128
	}

	interval := r.c.FlushBulkTime.Duration
	if interval == 0 {
		interval = 200 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer r.wg.Done()

	lastSavedTime := time.Now()
	reqs := make([]*mysqlsink.BulkRequest, 0, 1024)

	var pos mysql.Position

	for {
		needFlush := false
		needSavePos := false

		select {
		case v := <-r.syncCh:
			switch v := v.(type) {
			case posSaver:
				now := time.Now()
				if v.force || now.Sub(lastSavedTime) > 3*time.Second {
					lastSavedTime = now
					needFlush = true
					needSavePos = true
					pos = v.pos
				}
			case []*mysqlsink.BulkRequest:
				reqs = append(reqs, v...)
				needFlush = len(reqs) >= bulkSize
			}
		case <-ticker.C:
			needFlush = true
		case <-r.ctx.Done():
			return
		}

		if needFlush {
			// TODO: retry some times?
			if err := r.doBulk(reqs); err != nil {
				log.Errorf("do bulk err %v, close sync", err)
				r.cancel()
				return
			}
			reqs = reqs[0:0]
		}

		if needSavePos {
			if err := r.master.Save(pos); err != nil {
				log.Errorf("save sync position %s err %v, close sync", pos, err)
				r.cancel()
				return
			}
		}
	}
}

func (r *River) makeInsertRequest(rule *Rule, rows [][]interface{}) ([]*mysqlsink.BulkRequest, error) {
	return r.makeRequest(rule, canal.InsertAction, rows)
}

func (r *River) makeDeleteRequest(rule *Rule, rows [][]interface{}) ([]*mysqlsink.BulkRequest, error) {
	return r.makeRequest(rule, canal.DeleteAction, rows)
}

// for insert and delete
func (r *River) makeRequest(rule *Rule, action string, rows [][]interface{}) ([]*mysqlsink.BulkRequest, error) {
	reqs := make([]*mysqlsink.BulkRequest, 0, len(rows))

	for _, values := range rows {
		pkName, id, err := r.getDocID(rule, values)
		if err != nil {
			return nil, errors.Trace(err)
		}

		req := &mysqlsink.BulkRequest{Schema: rule.SinkSchema, Table: rule.SinkTable, PkName: pkName, PkValue: id, Index: rule.Index, Type: rule.Type, ID: id, Pipeline: rule.Pipeline}

		if action == canal.DeleteAction {
			req.Action = mysqlsink.ActionDelete
			r.st.DeleteNum.Add(1)
		} else {
			r.makeInsertReqData(req, rule, values)
			r.st.InsertNum.Add(1)
		}

		reqs = append(reqs, req)
	}

	return reqs, nil
}

func (r *River) makeUpdateRequest(rule *Rule, rows [][]interface{}) ([]*mysqlsink.BulkRequest, error) {
	if len(rows)%2 != 0 {
		return nil, errors.Errorf("invalid update rows event, must have 2x rows, but %d", len(rows))
	}

	reqs := make([]*mysqlsink.BulkRequest, 0, len(rows))

	for i := 0; i < len(rows); i += 2 {
		pkName, beforeID, err := r.getDocID(rule, rows[i])
		if err != nil {
			return nil, errors.Trace(err)
		}

		_, afterID, err := r.getDocID(rule, rows[i+1])

		if err != nil {
			return nil, errors.Trace(err)
		}

		req := &mysqlsink.BulkRequest{Schema: rule.SinkSchema, Table: rule.SinkTable, PkName: pkName, PkValue: beforeID, Index: rule.Index, Type: rule.Type, ID: beforeID}

		if beforeID != afterID {
			req.Action = mysqlsink.ActionDelete
			reqs = append(reqs, req)

			req = &mysqlsink.BulkRequest{Index: rule.Index, Type: rule.Type, ID: afterID, Pipeline: rule.Pipeline}
			r.makeInsertReqData(req, rule, rows[i+1])

			r.st.DeleteNum.Add(1)
			r.st.InsertNum.Add(1)
		} else {
			if len(rule.Pipeline) > 0 {
				// Pipelines can only be specified on index action
				r.makeInsertReqData(req, rule, rows[i+1])
				// Make sure action is index, not create
				req.Action = mysqlsink.ActionUpdate
				req.Pipeline = rule.Pipeline
			} else {
				r.makeUpdateReqData(req, rule, rows[i], rows[i+1])
			}
			r.st.UpdateNum.Add(1)
		}

		reqs = append(reqs, req)
	}

	return reqs, nil
}

func (r *River) makeReqColumnData(col *schema.TableColumn, value interface{}) interface{} {
	switch col.Type {
	case schema.TYPE_ENUM:
		switch value := value.(type) {
		case int64:
			// for binlog, ENUM may be int64, but for dump, enum is string
			eNum := value - 1
			if eNum < 0 || eNum >= int64(len(col.EnumValues)) {
				// we insert invalid enum value before, so return empty
				log.Warnf("invalid binlog enum index %d, for enum %v", eNum, col.EnumValues)
				return ""
			}

			return col.EnumValues[eNum]
		}
	case schema.TYPE_SET:
		switch value := value.(type) {
		case int64:
			// for binlog, SET may be int64, but for dump, SET is string
			bitmask := value
			sets := make([]string, 0, len(col.SetValues))
			for i, s := range col.SetValues {
				if bitmask&int64(1<<uint(i)) > 0 {
					sets = append(sets, s)
				}
			}
			return strings.Join(sets, ",")
		}
	case schema.TYPE_BIT:
		switch value := value.(type) {
		case string:
			// for binlog, BIT is int64, but for dump, BIT is string
			// for dump 0x01 is for 1, \0 is for 0
			if value == "\x01" {
				return int64(1)
			}

			return int64(0)
		}
	case schema.TYPE_STRING:
		switch value := value.(type) {
		case []byte:
			return string(value[:])
		}
	case schema.TYPE_JSON:
		var f interface{}
		var err error
		switch v := value.(type) {
		case string:
			err = json.Unmarshal([]byte(v), &f)
		case []byte:
			err = json.Unmarshal(v, &f)
		}
		if err == nil && f != nil {
			return f
		}
	case schema.TYPE_DATETIME, schema.TYPE_TIMESTAMP:
		switch v := value.(type) {
		case string:
			return v
		}
	case schema.TYPE_DATE:
		switch v := value.(type) {
		case string:
			vt, err := time.Parse(mysqlDateFormat, string(v))
			if err != nil || vt.IsZero() { // failed to parse date or zero date
				return nil
			}
			return vt.Format(mysqlDateFormat)
		}
	}

	return value
}

func (r *River) getFieldParts(k string, v string) (string, string, string) {
	composedField := strings.Split(v, ",")

	mysql := k
	elastic := composedField[0]
	fieldType := ""

	if 0 == len(elastic) {
		elastic = mysql
	}
	if 2 == len(composedField) {
		fieldType = composedField[1]
	}

	return mysql, elastic, fieldType
}

func (r *River) makeInsertReqData(req *mysqlsink.BulkRequest, rule *Rule, values []interface{}) {
	req.Data = make(map[string]interface{}, len(values))
	req.Action = mysqlsink.ActionInsert

	for i, c := range rule.TableInfo.Columns {
		if !rule.CheckFilter(c.Name) {
			continue
		}
		mapped := false
		for k, v := range rule.FieldMapping {
			mysql, elastic, fieldType := r.getFieldParts(k, v)
			if mysql == c.Name {
				mapped = true
				req.Data[elastic] = r.getFieldValue(&c, fieldType, values[i])
			}
		}
		if mapped == false {
			req.Data[c.Name] = r.makeReqColumnData(&c, values[i])
		}
	}
}

func (r *River) makeUpdateReqData(req *mysqlsink.BulkRequest, rule *Rule, beforeValues []interface{}, afterValues []interface{}) {
	req.Data = make(map[string]interface{}, len(beforeValues))

	// maybe dangerous if something wrong delete before?
	req.Action = mysqlsink.ActionUpdate

	for i, c := range rule.TableInfo.Columns {
		mapped := false
		if !rule.CheckFilter(c.Name) {
			continue
		}
		if reflect.DeepEqual(beforeValues[i], afterValues[i]) {
			//nothing changed
			continue
		}
		for k, v := range rule.FieldMapping {
			mysql, elastic, fieldType := r.getFieldParts(k, v)
			if mysql == c.Name {
				mapped = true
				req.Data[elastic] = r.getFieldValue(&c, fieldType, afterValues[i])
			}
		}
		if mapped == false {
			req.Data[c.Name] = r.makeReqColumnData(&c, afterValues[i])
		}

	}
}

// If id in toml file is none, get primary keys in one row and format them into a string, and PK must not be nil
// Else get the ID's column in one row and format them into a string
func (r *River) getDocID(rule *Rule, row []interface{}) (string, interface{}, error) {
	var (
		ids map[string]interface{}
		err error
	)

	if rule.ID == nil {
		ids, err = rule.TableInfo.GetPKPairs(row)
		if err != nil {
			return "", "", err
		}
	}

	for key, value := range ids {
		if value == nil {
			return "", "", errors.Errorf("The %ds id or PK value is nil", key)
		}

		return fmt.Sprintf("%v", key), value, nil
	}

	return "", "", nil
}

func (r *River) doBulk(reqs []*mysqlsink.BulkRequest) error {
	if len(reqs) == 0 {
		return nil
	}

	if _, err := r.mysql.Bulk(reqs); err != nil {
		log.Errorf("sync docs err %v after binlog %s", err, r.canal.SyncedPosition())
		return errors.Trace(err)
	}

	return nil
}

// get mysql field value and convert it to specific value to es
func (r *River) getFieldValue(col *schema.TableColumn, fieldType string, value interface{}) interface{} {
	var fieldValue interface{}
	switch fieldType {
	case fieldTypeList:
		v := r.makeReqColumnData(col, value)
		if str, ok := v.(string); ok {
			fieldValue = strings.Split(str, ",")
		} else {
			fieldValue = v
		}

	case fieldTypeDate:
		if col.Type == schema.TYPE_NUMBER {
			col.Type = schema.TYPE_DATETIME

			v := reflect.ValueOf(value)
			switch v.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				fieldValue = r.makeReqColumnData(col, time.Unix(v.Int(), 0).Format(mysql.TimeFormat))
			}
		}
	}

	if fieldValue == nil {
		fieldValue = r.makeReqColumnData(col, value)
	}
	return fieldValue
}
