package river

import (
	"github.com/zhaochuanyun/go-mysql/schema"
)

// Rule is the rule for how to sync data from MySQL to MySQL.
type Rule struct {
	ID           []string `toml:"id"`
	SourceSchema string   `toml:"source_schema"`
	SourceTable  string   `toml:"source_table"`
	SinkSchema   string   `toml:"sink_schema"`
	SinkTable    string   `toml:"sink_table"`

	FieldMapping map[string]string `toml:"field"`

	// MySQL table information
	TableInfo *schema.Table

	//only MySQL fields in filter will be synced , default sync all fields
	Filter []string `toml:"filter"`
}

func newDefaultRule(schema string, table string) *Rule {
	r := new(Rule)

	r.SourceSchema = schema
	r.SinkTable = table

	r.FieldMapping = make(map[string]string)

	return r
}

func (r *Rule) prepare() error {
	if r.FieldMapping == nil {
		r.FieldMapping = make(map[string]string)
	}

	return nil
}

// CheckFilter checkers whether the field needs to be filtered.
func (r *Rule) CheckFilter(field string) bool {
	if r.Filter == nil {
		return true
	}

	for _, f := range r.Filter {
		if f == field {
			return true
		}
	}
	return false
}
