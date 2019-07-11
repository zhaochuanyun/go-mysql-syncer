package mysql

import (
	"bytes"
	"fmt"
	"github.com/juju/errors"
	"github.com/siddontang/go-log/log"
	mysqlConn "github.com/zhaochuanyun/go-mysql/client"
	"github.com/zhaochuanyun/go-mysql/mysql"
	s "strings"
)

type Client struct {
	conn *mysqlConn.Conn
}

// ClientConfig is the configuration for the client.
type ClientConfig struct {
	Addr     string
	User     string
	Password string
	Schema   string
	Table    string
}

// NewClient creates the Cient with configuration.
func NewClient(conf *ClientConfig) *Client {
	c := new(Client)
	c.conn, _ = mysqlConn.Connect(conf.Addr, conf.User, conf.Password, conf.Schema)
	return c
}

const (
	ActionInsert = "insert"
	ActionUpdate = "update"
	ActionDelete = "delete"
)

// BulkRequest is used to send multi request in batch.
type BulkRequest struct {
	Action string
	Schema string
	Table  string
	Data   map[string]interface{}

	PkName  string
	PkValue interface{}

	Index    string
	Type     string
	ID       interface{}
	Parent   string
	Pipeline string
}

// Bulk sends the bulk request.
func (c *Client) Bulk(reqs []*BulkRequest) (*mysql.Result, error) {
	return c.DoBulk(reqs)
}

// DoBulk sends the bulk.
func (c *Client) DoBulk(reqs []*BulkRequest) (*mysql.Result, error) {
	var buf bytes.Buffer

	for _, req := range reqs {
		if err := req.bulk(&buf); err != nil {
			return nil, errors.Trace(err)
		}
	}

	ret, err := c.conn.Execute(buf.String())

	log.Infof("Execute --> %v", buf.String())

	if err != nil {
		return nil, errors.Trace(err)
	}

	return ret, errors.Trace(err)
}

func (r *BulkRequest) bulk(buf *bytes.Buffer) error {
	switch r.Action {
	case ActionDelete:
		// for delete
		buf.WriteString(" DELETE FROM ")
		buf.WriteString(r.Schema + "." + r.Table)
		buf.WriteString(" WHERE " + r.PkName + " = " + trans(r.PkValue))
	case ActionUpdate:
		// for update
		keys := make([]string, 0, len(r.Data))
		values := make([]interface{}, 0, len(r.Data))
		for k, v := range r.Data {
			keys = append(keys, k)
			values = append(values, v)
		}

		buf.WriteString(" UPDATE ")
		buf.WriteString(r.Schema + "." + r.Table + " SET ")
		buf.WriteString(keys[0] + " = " + trans(values[0]))

		for i, v := range keys[1:] {
			buf.WriteString(", " + v + " = " + trans(values[i+1]))
		}

		buf.WriteString(" WHERE " + r.PkName + " = " + trans(r.PkValue))
	default:
		// for insert
		keys := make([]string, 0, len(r.Data))
		values := make([]interface{}, 0, len(r.Data))
		for k, v := range r.Data {
			keys = append(keys, k)
			values = append(values, v)
		}

		buf.WriteString(" INSERT INTO ")
		buf.WriteString(r.Schema + "." + r.Table)
		buf.WriteString(" ( ")
		buf.WriteString(s.Join(keys, ","))
		buf.WriteString(" ) ")

		buf.WriteString(" VALUES ( ")
		buf.WriteString(Join(values, ","))
		buf.WriteString(" ) ")
	}
	return nil
}

func Join(a []interface{}, sep string) string {
	if len(a) == 0 {
		return ""
	}
	if len(a) == 1 {
		return fmt.Sprintf("%v", a[0])
	}

	buffer := &bytes.Buffer{}

	buffer.WriteString(trans(a[0]))
	for i := 1; i < len(a); i++ {
		buffer.WriteString(sep)
		buffer.WriteString(trans(a[i]))
	}
	return buffer.String()
}

func trans(v interface{}) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case string:
		return fmt.Sprintf("\"%v\"", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
