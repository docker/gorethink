package gorethink

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/dancannon/gorethink/encoding"
	p "github.com/dancannon/gorethink/ql2"
)

var (
	errCursorClosed = errors.New("connection closed, cannot read cursor")
)

func newCursor(conn *Connection, token int64, term *Term, opts map[string]interface{}) *Cursor {
	cursor := &Cursor{
		conn:  conn,
		token: token,
		term:  term,
		opts:  opts,
	}

	return cursor
}

// Cursor is the result of a query. Its cursor starts before the first row
// of the result set. Use Next to advance through the rows:
//
//     cursor, err := query.Run(session)
//     ...
//     defer cursor.Close()
//
//     var response interface{}
//     for cursor.Next(&response) {
//         ...
//     }
//     err = cursor.Err() // get any error encountered during iteration
//     ...
type Cursor struct {
	pc          *poolConn
	releaseConn func(error)

	conn  *Connection
	token int64
	query Query
	term  *Term
	opts  map[string]interface{}

	sync.Mutex
	lastErr   error
	fetching  int32
	closed    bool
	finished  bool
	buffer    queue
	responses queue
	profile   interface{}
}

// Profile returns the information returned from the query profiler.
func (c *Cursor) Profile() interface{} {
	c.Lock()
	defer c.Unlock()

	return c.profile
}

// Err returns nil if no errors happened during iteration, or the actual
// error otherwise.
func (c *Cursor) Err() error {
	c.Lock()
	defer c.Unlock()

	return c.lastErr
}

// Close closes the cursor, preventing further enumeration. If the end is
// encountered, the cursor is closed automatically. Close is idempotent.
func (c *Cursor) Close() error {
	c.Lock()
	defer c.Unlock()

	var err error

	if c.closed {
		return nil
	}

	conn := c.conn
	if conn == nil {
		return nil
	}
	if conn.conn == nil {
		return nil
	}

	// Stop any unfinished queries
	if !c.closed && !c.finished {
		q := Query{
			Type:  p.Query_STOP,
			Token: c.token,
		}

		_, _, err = conn.Query(q, map[string]interface{}{})
	}

	c.closed = true
	c.conn = nil
	c.releaseConn(err)

	return err
}

// Next retrieves the next document from the result set, blocking if necessary.
// This method will also automatically retrieve another batch of documents from
// the server when the current one is exhausted, or before that in background
// if possible.
//
// Next returns true if a document was successfully unmarshalled onto result,
// and false at the end of the result set or if an error happened.
// When Next returns false, the Err method should be called to verify if
// there was an error during iteration.
func (c *Cursor) Next(dest interface{}) bool {
	if c.closed {
		return false
	}

	hasMore, err := c.loadNext(dest)
	if c.handleError(err) != nil {
		c.Close()
		return false
	}

	return hasMore
}

func (c *Cursor) loadNext(dest interface{}) (bool, error) {
	c.Lock()

	// Load more data if needed
	for c.lastErr == nil && c.buffer.Len() == 0 && c.responses.Len() == 0 && !c.finished {
		// Check if response is closed/finished
		if c.closed {
			return false, errCursorClosed
		}

		c.Unlock()
		err := c.fetchMore()
		if err != nil {
			return false, err
		}
		c.Lock()
	}

	if c.buffer.Len() == 0 && c.responses.Len() == 0 && c.finished {
		c.Unlock()
		return false, nil
	}

	if c.buffer.Len() == 0 && c.responses.Len() > 0 {
		if response, ok := c.responses.Pop().(json.RawMessage); ok {
			c.Unlock()
			var value interface{}
			err := json.Unmarshal(response, &value)
			c.handleError(err)
			if err != nil {
				return false, err
			}

			value, err = recursivelyConvertPseudotype(value, c.opts)
			c.handleError(err)
			if err != nil {
				return false, err
			}

			c.Lock()
			if data, ok := value.([]interface{}); ok {
				for _, v := range data {
					c.buffer.Push(v)
				}
			} else if value == nil {
				c.buffer.Push(nil)
			} else {
				c.buffer.Push(value)
			}
		}
	}

	// Asynchronously loading next batch if possible
	if c.responses.Len() == 1 && !c.finished {
		go c.fetchMore()
	}

	if c.buffer.Len() == 0 {
		c.Unlock()
		return false, nil
	}

	data := c.buffer.Pop()
	c.Unlock()

	err := encoding.Decode(dest, data)
	c.handleError(err)
	if err != nil {
		return false, err
	}

	return true, nil
}

// All retrieves all documents from the result set into the provided slice
// and closes the cursor.
//
// The result argument must necessarily be the address for a slice. The slice
// may be nil or previously allocated.
func (c *Cursor) All(result interface{}) error {
	resultv := reflect.ValueOf(result)
	if resultv.Kind() != reflect.Ptr || resultv.Elem().Kind() != reflect.Slice {
		panic("result argument must be a slice address")
	}
	slicev := resultv.Elem()
	slicev = slicev.Slice(0, slicev.Cap())
	elemt := slicev.Type().Elem()
	i := 0
	for {
		if slicev.Len() == i {
			elemp := reflect.New(elemt)
			if !c.Next(elemp.Interface()) {
				break
			}
			slicev = reflect.Append(slicev, elemp.Elem())
			slicev = slicev.Slice(0, slicev.Cap())
		} else {
			if !c.Next(slicev.Index(i).Addr().Interface()) {
				break
			}
		}
		i++
	}
	resultv.Elem().Set(slicev.Slice(0, i))

	if err := c.Err(); err != nil {
		c.Close()
		return err
	}

	if err := c.Close(); err != nil {
		return err
	}

	return nil
}

// One retrieves a single document from the result set into the provided
// slice and closes the cursor.
func (c *Cursor) One(result interface{}) error {
	if c.IsNil() {
		return ErrEmptyResult
	}

	hasResult := c.Next(result)

	if err := c.Err(); err != nil {
		c.Close()
		return err
	}

	if err := c.Close(); err != nil {
		return err
	}

	if !hasResult {
		return ErrEmptyResult
	}

	return nil
}

// IsNil tests if the current row is nil.
func (c *Cursor) IsNil() bool {
	c.Lock()
	defer c.Unlock()
	if c.buffer.Len() > 0 {
		bufferedItem := c.buffer.Peek()
		if bufferedItem == nil {
			return true
		}

		if bufferedItem == nil {
			return true
		}

		return false
	}

	if c.responses.Len() > 0 {
		response := c.responses.Peek()
		if response == nil {
			return true
		}

		if response, ok := response.(json.RawMessage); ok {
			if string(response) == "null" {
				return true
			}
		}

		return false
	}

	return true
}

// fetchMore fetches more rows from the database.
//
// If wait is true then it will wait for the database to reply otherwise it
// will return after sending the continue query.
func (c *Cursor) fetchMore() error {
	var err error

	if atomic.CompareAndSwapInt32(&c.fetching, 0, 1) {
		c.Lock()
		conn := c.conn
		token := c.token
		q := Query{
			Type:  p.Query_CONTINUE,
			Token: token,
		}
		c.Unlock()

		_, _, err = conn.Query(q, map[string]interface{}{})
		c.handleError(err)
	}

	return err
}

// handleError sets the value of lastErr to err if lastErr is not yet set.
func (c *Cursor) handleError(err error) error {
	c.Lock()
	defer c.Unlock()

	if c.lastErr != nil {
		c.lastErr = err
	}

	return c.lastErr
}

// extend adds the result of a continue query to the cursor.
func (c *Cursor) extend(response *Response) {
	c.Lock()
	defer c.Unlock()

	for _, response := range response.Responses {
		c.responses.Push(response)
	}

	c.finished = response.Type != p.Response_SUCCESS_PARTIAL && response.Type != p.Response_SUCCESS_FEED
	atomic.StoreInt32(&c.fetching, 0)

	// Asynchronously load next batch if possible
	if c.responses.Len() == 1 && !c.finished {
		go c.fetchMore()
	}
}

// Queue structure used for storing responses

type queue struct {
	elems               []interface{}
	nelems, popi, pushi int
}

func (q *queue) Len() int {
	return q.nelems
}
func (q *queue) Push(elem interface{}) {
	if q.nelems == len(q.elems) {
		q.expand()
	}
	q.elems[q.pushi] = elem
	q.nelems++
	q.pushi = (q.pushi + 1) % len(q.elems)
}
func (q *queue) Pop() (elem interface{}) {
	if q.nelems == 0 {
		return nil
	}
	elem = q.elems[q.popi]
	q.elems[q.popi] = nil // Help GC.
	q.nelems--
	q.popi = (q.popi + 1) % len(q.elems)
	return elem
}
func (q *queue) Peek() (elem interface{}) {
	if q.nelems == 0 {
		return nil
	}
	return q.elems[q.popi]
}
func (q *queue) expand() {
	curcap := len(q.elems)
	var newcap int
	if curcap == 0 {
		newcap = 8
	} else if curcap < 1024 {
		newcap = curcap * 2
	} else {
		newcap = curcap + (curcap / 4)
	}
	elems := make([]interface{}, newcap)
	if q.popi == 0 {
		copy(elems, q.elems)
		q.pushi = curcap
	} else {
		newpopi := newcap - (curcap - q.popi)
		copy(elems, q.elems[:q.popi])
		copy(elems[newpopi:], q.elems[q.popi:])
		q.popi = newpopi
	}
	for i := range q.elems {
		q.elems[i] = nil // Help GC.
	}
	q.elems = elems
}
