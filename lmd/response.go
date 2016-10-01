package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Response contains the livestatus response data as long with some meta data
type Response struct {
	Code        int
	Result      [][]interface{}
	ResultTotal int
	Request     *Request
	Error       error
	Failed      map[string]string
	Columns     []Column
}

// VirtKeyMapTupel is used to define the virtual key mapping in the VirtKeyMap
type VirtKeyMapTupel struct {
	Index int
	Key   string
	Type  ColumnType
}

// VirtKeyMap maps the virtual columns with the peer status map entry.
// If the entry is empty, then there must be a corresponding resolve function in the GetRowValue() function.
var VirtKeyMap = map[string]VirtKeyMapTupel{
	"key":                     {Index: -1, Key: "PeerKey", Type: StringCol},
	"name":                    {Index: -2, Key: "PeerName", Type: StringCol},
	"addr":                    {Index: -4, Key: "PeerAddr", Type: StringCol},
	"status":                  {Index: -5, Key: "PeerStatus", Type: IntCol},
	"bytes_send":              {Index: -6, Key: "BytesSend", Type: IntCol},
	"bytes_received":          {Index: -7, Key: "BytesReceived", Type: IntCol},
	"queries":                 {Index: -8, Key: "Querys", Type: IntCol},
	"last_error":              {Index: -9, Key: "LastError", Type: StringCol},
	"last_online":             {Index: -10, Key: "LastOnline", Type: TimeCol},
	"last_update":             {Index: -11, Key: "LastUpdate", Type: TimeCol},
	"response_time":           {Index: -12, Key: "ReponseTime", Type: FloatCol},
	"state_order":             {Index: -13, Key: "", Type: IntCol},
	"last_state_change_order": {Index: -14, Key: "", Type: IntCol},
}

// Len returns the result length used for sorting results.
func (res Response) Len() int {
	return len(res.Result)
}

// Less returns the sort result of two data rows
func (res Response) Less(i, j int) bool {
	for _, s := range res.Request.Sort {
		Type := res.Columns[s.Index].Type
		switch Type {
		case TimeCol:
			fallthrough
		case IntCol:
			fallthrough
		case FloatCol:
			valueA := NumberToFloat(res.Result[i][s.Index])
			valueB := NumberToFloat(res.Result[j][s.Index])
			if valueA == valueB {
				continue
			}
			if s.Direction == Asc {
				return valueA < valueB
			}
			return valueA > valueB
		case StringCol:
			if s1, ok := res.Result[i][s.Index].(string); ok {
				if s2, ok := res.Result[j][s.Index].(string); ok {
					if s1 == s2 {
						continue
					}
					if s.Direction == Asc {
						return s1 < s2
					}
					return s1 > s2
				}
			}
			// not implemented
			return s.Direction == Asc
		case StringListCol:
			// not implemented
			return s.Direction == Asc
		case IntListCol:
			// not implemented
			return s.Direction == Asc
		}
		panic(fmt.Sprintf("sorting not implemented for type %d", Type))
	}
	return true
}

// Swap replaces two data rows while sorting.
func (res Response) Swap(i, j int) {
	res.Result[i], res.Result[j] = res.Result[j], res.Result[i]
}

// BuildResponse builds the response for a given request.
// It returns the Response object and any error encountered.
func BuildResponse(req *Request) (res *Response, err error) {
	log.Tracef("BuildResponse")
	res = &Response{
		Code:    200,
		Failed:  make(map[string]string),
		Request: req,
	}

	table, _ := Objects.Tables[req.Table]

	indexes, columns, err := req.BuildResponseIndexes(&table)
	if err != nil {
		return
	}
	res.Columns = columns
	numPerRow := len(indexes)

	backendsMap, numBackendsReq, err := ExpandRequestBackends(req)
	if err != nil {
		return
	}

	// check if we have to spin up updates, if so, do it parallel
	selectedPeers := []string{}
	spinUpPeers := []string{}
	for _, id := range DataStoreOrder {
		p := DataStore[id]
		if numBackendsReq > 0 {
			_, Ok := backendsMap[p.ID]
			if !Ok {
				continue
			}
		}
		selectedPeers = append(selectedPeers, id)

		// spin up required?
		p.PeerLock.RLock()
		if !table.PassthroughOnly && p.Status["Idling"].(bool) && len(table.DynamicColCacheIndexes) > 0 {
			spinUpPeers = append(spinUpPeers, id)
		}
		p.PeerLock.RUnlock()
	}

	if len(spinUpPeers) > 0 {
		SpinUpPeers(spinUpPeers)
	}

	if table.Name == "tables" || table.Name == "columns" {
		selectedPeers = []string{DataStoreOrder[0]}
	}

	if table.PassthroughOnly {
		// passthrough requests, ex.: log table
		res.BuildPassThroughResult(selectedPeers, &table, &columns, numPerRow)
		if err != nil {
			return
		}
	} else {
		for _, id := range selectedPeers {
			p := DataStore[id]
			p.BuildLocalResponseData(res, req, numPerRow, &indexes)
			log.Tracef("BuildLocalResponseData done: %s", p.Name)
			if table.Name == "tables" || table.Name == "columns" {
				break
			}
		}
	}
	if res.Result == nil {
		res.Result = make([][]interface{}, 0)
	}
	res.BuildResponsePostProcessing()
	return
}

// ExpandRequestBackends returns a map of used backends.
func ExpandRequestBackends(req *Request) (backendsMap map[string]string, numBackendsReq int, err error) {
	numBackendsReq = len(req.Backends)
	if numBackendsReq > 0 {
		backendsMap = make(map[string]string)
		for _, b := range req.Backends {
			_, Ok := DataStore[b]
			if !Ok {
				err = errors.New("bad request: backend " + b + " does not exist")
				return
			}
			backendsMap[b] = b
		}
	}
	return
}

// BuildResponsePostProcessing does all the post processing required for a request like sorting and cutting of limits and applying offsets.
func (res *Response) BuildResponsePostProcessing() {
	log.Tracef("BuildResponsePostProcessing")
	// sort our result
	if len(res.Request.Sort) > 0 {
		t1 := time.Now()
		sort.Sort(res)
		duration := time.Since(t1)
		log.Debugf("sorting result took %s", duration.String())
	}

	if res.ResultTotal == 0 {
		res.ResultTotal = len(res.Result)
	}

	// apply request offset
	if res.Request.Offset > 0 {
		if res.Request.Offset > res.ResultTotal {
			res.Result = make([][]interface{}, 0)
		} else {
			res.Result = res.Result[res.Request.Offset:]
		}
	}

	// apply request limit
	if res.Request.Limit > 0 {
		if res.Request.Limit < res.ResultTotal {
			res.Result = res.Result[0:res.Request.Limit]
		}
	}

	// final calculation of stats querys
	if len(res.Request.Stats) > 0 {
		res.Result = make([][]interface{}, 1)
		res.Result[0] = make([]interface{}, len(res.Request.Stats))
		for i, s := range res.Request.Stats {
			switch s.StatsType {
			case Counter:
				res.Result[0][i] = s.Stats
				break
			case Min:
				res.Result[0][i] = s.Stats
				break
			case Max:
				res.Result[0][i] = s.Stats
				break
			case Sum:
				res.Result[0][i] = s.Stats
				break
			case Average:
				if s.StatsCount > 0 {
					res.Result[0][i] = float64(s.Stats) / float64(s.StatsCount)
				} else {
					res.Result[0][i] = 0
				}
				break
			default:
				log.Panicf("not implemented")
				break
			}
			if s.StatsCount == 0 {
				res.Result[0][i] = 0
			}
		}
	}
	return
}

// BuildResponseIndexes returns a list of used indexes and columns for this request.
func (req *Request) BuildResponseIndexes(table *Table) (indexes []int, columns []Column, err error) {
	log.Tracef("BuildResponseIndexes")
	requestColumnsMap := make(map[string]int)
	// if no column header was given, return all columns
	// but only if this is no stats query
	if len(req.Columns) == 0 && len(req.Stats) == 0 {
		req.SendColumnsHeader = true
		for _, col := range table.Columns {
			if col.Update == StaticUpdate || col.Update == DynamicUpdate || col.Update == VirtUpdate || col.Type == VirtCol {
				req.Columns = append(req.Columns, col.Name)
			}
		}
	}
	// build array of requested columns as Column objects list
	for j, col := range req.Columns {
		col = strings.ToLower(col)
		i, ok := table.ColumnsIndex[col]
		if !ok {
			err = errors.New("bad request: table " + req.Table + " has no column " + col)
			return
		}
		if table.Columns[i].Type == VirtCol {
			indexes = append(indexes, VirtKeyMap[col].Index)
			columns = append(columns, Column{Name: col, Type: VirtKeyMap[col].Type, Index: j, RefIndex: i})
			requestColumnsMap[col] = j
			continue
		}
		indexes = append(indexes, i)
		columns = append(columns, Column{Name: col, Type: table.Columns[i].Type, Index: j})
		requestColumnsMap[col] = j
	}

	// check wether our sort columns do exist in the output
	for _, s := range req.Sort {
		_, Ok := table.ColumnsIndex[s.Name]
		if !Ok {
			err = errors.New("bad request: table " + req.Table + " has no column " + s.Name + " to sort")
			return
		}
		i, Ok := requestColumnsMap[s.Name]
		if !Ok {
			err = errors.New("bad request: sort column " + s.Name + " not in result set")
			return
		}
		s.Index = i
	}

	return
}

// Send writes converts the result object to a livestatus answer and writes the resulting bytes back to the client.
func (res *Response) Send(c net.Conn) (size int, err error) {
	resBytes := []byte{}
	if res.Request.SendColumnsHeader {
		var result [][]interface{}
		cols := make([]interface{}, len(res.Request.Columns)+len(res.Request.Stats))
		for i, v := range res.Request.Columns {
			cols[i] = v
		}
		result = append(result, cols)
		result = append(result, res.Result...)
		res.Result = result
	}
	if res.Error != nil {
		log.Warnf("client error: %s", res.Error.Error())
		resBytes = []byte(res.Error.Error())
	} else if res.Result != nil {
		if res.Request.OutputFormat == "wrapped_json" {
			resBytes = append(resBytes, []byte("{\"data\":[")...)
		}
		if res.Request.OutputFormat == "json" || res.Request.OutputFormat == "" {
			resBytes = append(resBytes, []byte("[")...)
		}
		// append result row by row
		if res.Request.OutputFormat == "wrapped_json" || res.Request.OutputFormat == "json" || res.Request.OutputFormat == "" {
			for i, row := range res.Result {
				rowBytes, jerr := json.Marshal(row)
				if jerr != nil {
					log.Errorf("json error: %s in row: %v", jerr.Error(), row)
					err = jerr
					return
				}
				if i > 0 {
					resBytes = append(resBytes, []byte(",\n")...)
				}
				resBytes = append(resBytes, rowBytes...)
			}
			resBytes = append(resBytes, []byte("]")...)
		}
		if res.Request.OutputFormat == "wrapped_json" {
			resBytes = append(resBytes, []byte("\n,\"failed\":")...)
			failBytes, _ := json.Marshal(res.Failed)
			resBytes = append(resBytes, failBytes...)
			resBytes = append(resBytes, []byte(fmt.Sprintf("\n,\"total\":%d}", res.ResultTotal))...)
		}
	}

	size = len(resBytes) + 1
	if res.Request.ResponseFixed16 {
		log.Debugf("write: %s", fmt.Sprintf("%d %11d", res.Code, size))
		_, err = c.Write([]byte(fmt.Sprintf("%d %11d\n", res.Code, size)))
		if err != nil {
			log.Warnf("write error: %s", err.Error())
		}
	}
	log.Debugf("write: %s", resBytes)
	written, err := c.Write(resBytes)
	if err != nil {
		log.Warnf("write error: %s", err.Error())
	}
	if written != size-1 {
		log.Warnf("write error: written %d, size: %d", written, size)
	}
	localAddr := c.LocalAddr().String()
	promFrontendBytesSend.WithLabelValues(localAddr).Add(float64(len(resBytes)))
	_, err = c.Write([]byte("\n"))
	return
}

// BuildPassThroughResult passes a query transparently to one or more remote sites and builds the response
// from that.
func (res *Response) BuildPassThroughResult(peers []string, table *Table, columns *[]Column, numPerRow int) (err error) {
	req := res.Request
	res.Result = make([][]interface{}, 0)

	// build columns list
	backendColumns := []string{}
	virtColumns := []Column{}
	for _, col := range *columns {
		if col.RefIndex > 0 {
			virtColumns = append(virtColumns, col)
		} else {
			backendColumns = append(backendColumns, col.Name)
		}
	}

	waitgroup := &sync.WaitGroup{}

	for _, id := range peers {
		p := DataStore[id]
		m := sync.Mutex{}

		p.PeerLock.RLock()
		if p.Status["PeerStatus"].(PeerStatus) == PeerStatusDown {
			m.Lock()
			res.Failed[p.ID] = fmt.Sprintf("%v", p.Status["LastError"])
			m.Unlock()
			p.PeerLock.RUnlock()
			continue
		}
		p.PeerLock.RUnlock()

		waitgroup.Add(1)
		go func(peer Peer, wg *sync.WaitGroup) {
			log.Debugf("[%s] starting passthrough request", p.Name)
			defer wg.Done()
			passthroughRequest := &Request{
				Table:           req.Table,
				Filter:          req.Filter,
				Stats:           req.Stats,
				Columns:         backendColumns,
				Limit:           req.Limit,
				OutputFormat:    "json",
				ResponseFixed16: true,
			}
			var result [][]interface{}
			result, err = peer.Query(passthroughRequest)
			log.Tracef("[%s] req done", p.Name)
			if err != nil {
				log.Tracef("[%s] req errored", err.Error())
				m.Lock()
				res.Failed[p.ID] = err.Error()
				m.Unlock()
				return
			}
			// insert virtual values
			if len(virtColumns) > 0 {
				for j, row := range result {
					for _, col := range virtColumns {
						i := col.Index
						row = append(row, 0)
						copy(row[i+1:], row[i:])
						row[i] = peer.GetRowValue(col.RefIndex, &row, j, table, nil, numPerRow)
					}
					result[j] = row
				}
			}
			log.Tracef("[%s] result ready", p.Name)
			m.Lock()
			res.Result = append(res.Result, result...)
			m.Unlock()
		}(p, waitgroup)
	}
	log.Tracef("waiting...")
	waitgroup.Wait()
	log.Debugf("waiting for passed through requests done")
	return
}
