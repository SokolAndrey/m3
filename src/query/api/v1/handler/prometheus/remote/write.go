// Copyright (c) 2018 Uber Technologies, Inc.
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

package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/m3db/m3/src/cmd/services/m3coordinator/ingest"
	"github.com/m3db/m3/src/dbnode/client"
	"github.com/m3db/m3/src/metrics/policy"
	"github.com/m3db/m3/src/query/api/v1/handler"
	"github.com/m3db/m3/src/query/api/v1/handler/prometheus"
	"github.com/m3db/m3/src/query/generated/proto/prompb"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/ts"
	"github.com/m3db/m3/src/query/util/logging"
	"github.com/m3db/m3/src/x/clock"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/ident"
	"github.com/m3db/m3/src/x/instrument"
	xhttp "github.com/m3db/m3/src/x/net/http"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/golang/protobuf/proto"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

const (
	// PromWriteURL is the url for the prom write handler
	PromWriteURL = handler.RoutePrefixV1 + "/prom/remote/write"

	// PromWriteHTTPMethod is the HTTP method used with this resource.
	PromWriteHTTPMethod = http.MethodPost

	// emptyStoragePolicyVar for code readability.
	emptyStoragePolicyVar = ""
)

var (
	errNoDownsamplerAndWriter       = errors.New("no downsampler and writer set")
	errNoTagOptions                 = errors.New("no tag options set")
	errNoNowFn                      = errors.New("no now fn set")
	errUnaggregatedStoragePolicySet = errors.New("storage policy should not be set for unaggregated metrics")
)

// PromWriteHandler represents a handler for prometheus write endpoint.
type PromWriteHandler struct {
	downsamplerAndWriter ingest.DownsamplerAndWriter
	tagOptions           models.TagOptions
	nowFn                clock.NowFn
	writeBytesPool       *writeBytesPool
	instrumentOpts       instrument.Options
	metrics              promWriteMetrics
}

// NewPromWriteHandler returns a new instance of handler.
func NewPromWriteHandler(
	downsamplerAndWriter ingest.DownsamplerAndWriter,
	tagOptions models.TagOptions,
	nowFn clock.NowFn,
	instrumentOpts instrument.Options,
) (http.Handler, error) {
	if downsamplerAndWriter == nil {
		return nil, errNoDownsamplerAndWriter
	}
	if tagOptions == nil {
		return nil, errNoTagOptions
	}
	if nowFn == nil {
		return nil, errNoNowFn
	}

	metrics, err := newPromWriteMetrics(instrumentOpts.MetricsScope())
	if err != nil {
		return nil, err
	}

	return &PromWriteHandler{
		downsamplerAndWriter: downsamplerAndWriter,
		tagOptions:           tagOptions,
		nowFn:                nowFn,
		writeBytesPool:       newWriteBytesPool(),
		metrics:              metrics,
		instrumentOpts:       instrumentOpts,
	}, nil
}

type promWriteMetrics struct {
	writeSuccess         tally.Counter
	writeErrorsServer    tally.Counter
	writeErrorsClient    tally.Counter
	ingestLatency        tally.Histogram
	ingestLatencyBuckets tally.DurationBuckets
}

func newPromWriteMetrics(scope tally.Scope) (promWriteMetrics, error) {
	upTo1sBuckets, err := tally.LinearDurationBuckets(0, 100*time.Millisecond, 10)
	if err != nil {
		return promWriteMetrics{}, err
	}

	upTo10sBuckets, err := tally.LinearDurationBuckets(time.Second, 500*time.Millisecond, 18)
	if err != nil {
		return promWriteMetrics{}, err
	}

	upTo60sBuckets, err := tally.LinearDurationBuckets(10*time.Second, 5*time.Second, 11)
	if err != nil {
		return promWriteMetrics{}, err
	}

	upTo60mBuckets, err := tally.LinearDurationBuckets(0, 5*time.Minute, 12)
	if err != nil {
		return promWriteMetrics{}, err
	}
	upTo60mBuckets = upTo60mBuckets[1:] // Remove the first 0s to get 5 min aligned buckets

	upTo6hBuckets, err := tally.LinearDurationBuckets(time.Hour, 30*time.Minute, 12)
	if err != nil {
		return promWriteMetrics{}, err
	}

	upTo24hBuckets, err := tally.LinearDurationBuckets(6*time.Hour, time.Hour, 19)
	if err != nil {
		return promWriteMetrics{}, err
	}
	upTo24hBuckets = upTo24hBuckets[1:] // Remove the first 6h to get 1 hour aligned buckets

	var ingestLatencyBuckets tally.DurationBuckets
	ingestLatencyBuckets = append(ingestLatencyBuckets, upTo1sBuckets...)
	ingestLatencyBuckets = append(ingestLatencyBuckets, upTo10sBuckets...)
	ingestLatencyBuckets = append(ingestLatencyBuckets, upTo60sBuckets...)
	ingestLatencyBuckets = append(ingestLatencyBuckets, upTo60mBuckets...)
	ingestLatencyBuckets = append(ingestLatencyBuckets, upTo6hBuckets...)
	ingestLatencyBuckets = append(ingestLatencyBuckets, upTo24hBuckets...)
	return promWriteMetrics{
		writeSuccess:         scope.SubScope("write").Counter("success"),
		writeErrorsServer:    scope.SubScope("write").Tagged(map[string]string{"code": "5XX"}).Counter("errors"),
		writeErrorsClient:    scope.SubScope("write").Tagged(map[string]string{"code": "4XX"}).Counter("errors"),
		ingestLatency:        scope.SubScope("ingest").Histogram("latency", ingestLatencyBuckets),
		ingestLatencyBuckets: ingestLatencyBuckets,
	}, nil
}

func (h *PromWriteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, opts, rErr := h.parseRequest(r)
	if rErr != nil {
		h.metrics.writeErrorsClient.Inc(1)
		xhttp.Error(w, rErr.Inner(), rErr.Code())
		return
	}

	batchErr := h.write(r.Context(), req, opts)

	// Record ingestion delay latency
	now := h.nowFn()
	for _, series := range req.Timeseries {
		for _, sample := range series.Samples {
			age := now.Sub(storage.PromTimestampToTime(sample.Timestamp))
			h.metrics.ingestLatency.RecordDuration(age)
		}
	}

	if batchErr != nil {
		var (
			errs              = batchErr.Errors()
			lastRegularErr    string
			lastBadRequestErr string
			numRegular        int
			numBadRequest     int
		)
		for _, err := range errs {
			switch {
			case client.IsBadRequestError(err):
				numBadRequest++
				lastBadRequestErr = err.Error()
			case xerrors.IsInvalidParams(err):
				numBadRequest++
				lastBadRequestErr = err.Error()
			default:
				numRegular++
				lastRegularErr = err.Error()
			}
		}

		var status int
		switch {
		case numBadRequest == len(errs):
			status = http.StatusBadRequest
			h.metrics.writeErrorsClient.Inc(1)
		default:
			status = http.StatusInternalServerError
			h.metrics.writeErrorsServer.Inc(1)
		}

		logger := logging.WithContext(r.Context(), h.instrumentOpts)
		logger.Error("write error",
			zap.String("remoteAddr", r.RemoteAddr),
			zap.Int("httpResponseStatusCode", status),
			zap.Int("numRegularErrors", numRegular),
			zap.Int("numBadRequestErrors", numBadRequest),
			zap.String("lastRegularError", lastRegularErr),
			zap.String("lastBadRequestErr", lastBadRequestErr))

		var resultErr string
		if lastRegularErr != "" {
			resultErr = fmt.Sprintf("retryable_errors: count=%d, last=%s",
				numRegular, lastRegularErr)
		}
		if lastBadRequestErr != "" {
			var sep string
			if lastRegularErr != "" {
				sep = ", "
			}
			resultErr = fmt.Sprintf("%s%sbad_request_errors: count=%d, last=%s",
				resultErr, sep, numBadRequest, lastBadRequestErr)
		}
		xhttp.Error(w, errors.New(resultErr), status)
		return
	}

	h.metrics.writeSuccess.Inc(1)
}

func (h *PromWriteHandler) parseRequest(
	r *http.Request,
) (*prompb.WriteRequest, ingest.WriteOptions, *xhttp.ParseError) {
	var opts ingest.WriteOptions
	if v := strings.TrimSpace(r.Header.Get(handler.MetricsTypeHeader)); v != "" {
		// Allow the metrics type and storage policies to override
		// the default rules and policies if specified.
		metricsType, err := storage.ParseMetricsType(v)
		if err != nil {
			return nil, ingest.WriteOptions{},
				xhttp.NewParseError(err, http.StatusBadRequest)
		}

		// Ensure ingest options specify we are overriding the
		// downsampling rules with zero rules to be applied (so
		// only direct writes will be made).
		opts.DownsampleOverride = true
		opts.DownsampleMappingRules = nil

		strPolicy := strings.TrimSpace(r.Header.Get(handler.MetricsStoragePolicyHeader))
		switch metricsType {
		case storage.UnaggregatedMetricsType:
			if strPolicy != emptyStoragePolicyVar {
				err := errUnaggregatedStoragePolicySet
				return nil, ingest.WriteOptions{},
					xhttp.NewParseError(err, http.StatusBadRequest)
			}
		default:
			parsed, err := policy.ParseStoragePolicy(strPolicy)
			if err != nil {
				err = fmt.Errorf("could not parse storage policy: %v", err)
				return nil, ingest.WriteOptions{},
					xhttp.NewParseError(err, http.StatusBadRequest)
			}

			// Make sure this specific storage policy is used for the writes.
			opts.WriteOverride = true
			opts.WriteStoragePolicies = policy.StoragePolicies{
				parsed,
			}
		}
	}

	var (
		dst = h.writeBytesPool.Get()
		req = &prompb.WriteRequest{}
		err *xhttp.ParseError
	)
	defer func() {
		h.writeBytesPool.Put(dst)
	}()

	dst, err = prometheus.ParsePromCompressedRequest(dst, r)
	if err != nil {
		return nil, ingest.WriteOptions{}, err
	}

	if err := proto.Unmarshal(dst, req); err != nil {
		return nil, ingest.WriteOptions{},
			xhttp.NewParseError(err, http.StatusBadRequest)
	}

	return req, opts, nil
}

func (h *PromWriteHandler) write(
	ctx context.Context,
	r *prompb.WriteRequest,
	opts ingest.WriteOptions,
) ingest.BatchError {
	iter := NewTimeSeriesIter(r.Timeseries, h.tagOptions)
	return h.downsamplerAndWriter.WriteBatch(ctx, iter, opts)
}

var _ ingest.DownsampleAndWriteIter = &promTSIter{}

type promTSIter struct {
	idx        int
	timeseries []*prompb.TimeSeries
	results    [][]writeState
	tagOpts    models.TagOptions

	datapoints ts.Datapoints
	tagIter    *tagIterator
}

type writeState struct {
	result storage.WriteQueryResult
	state  interface{}
}

// NewTimeSeriesIter is used to create a downsample and write iterator
// from a list of Prometheus protobuf time series.
func NewTimeSeriesIter(
	series []*prompb.TimeSeries,
	tagOpts models.TagOptions,
) ingest.DownsampleAndWriteIter {
	// Calculate number of datapoints
	numDatapoints := 0
	for _, promTS := range series {
		numDatapoints += len(promTS.Samples)
	}

	// Construct the tags and datapoints up front so that if the iterator
	// is reset, we don't have to generate them twice.
	var (
		results         = make([][]writeState, len(series))
		preallocResults = make([]writeState, numDatapoints)
	)
	for i, promTS := range series {
		// Grab reference to prealloc results, reset to samples.
		ref := preallocResults[:len(promTS.Samples)]
		results[i] = ref

		// Make sure labels are ordered.
		sort.Sort(labelsByName(promTS.Labels))

		// Move the prealloc results slice along.
		preallocResults = preallocResults[len(promTS.Samples):]
	}

	return &promTSIter{
		idx:        -1,
		timeseries: series,
		results:    results,
		tagOpts:    tagOpts,
		tagIter:    newTagIterator(),
		datapoints: nil,
	}
}

func (i *promTSIter) Restart() {
	i.idx = -1
	i.tagIter.Restart()
	i.datapoints = i.datapoints[:0]
}

func (i *promTSIter) DatapointResult(
	datapointIdx int,
) storage.WriteQueryResult {
	return i.results[i.idx][datapointIdx].result
}

func (i *promTSIter) DatapointState(
	datapointIdx int,
) interface{} {
	return i.results[i.idx][datapointIdx].state
}

func (i *promTSIter) SetDatapointResult(
	datapointIdx int,
	result storage.WriteQueryResult,
) {
	i.results[i.idx][datapointIdx].result = result
}

func (i *promTSIter) SetDatapointState(
	datapointIdx int,
	state interface{},
) {
	i.results[i.idx][datapointIdx].state = state
}

func (i *promTSIter) TagOptions() models.TagOptions {
	return i.tagOpts
}

func (i *promTSIter) Next() bool {
	i.idx++
	next := i.idx < len(i.timeseries)
	if !next {
		return false
	}

	i.tagIter.Reset(i.timeseries[i.idx].Labels)
	i.datapoints = i.datapoints[:0]
	for _, dp := range i.timeseries[i.idx].Samples {
		i.datapoints = append(i.datapoints, ts.Datapoint{
			Timestamp: storage.PromTimestampToTime(dp.Timestamp),
			Value:     dp.Value,
		})
	}
	return true
}

func (i *promTSIter) Current() (ident.TagIterator, ts.Datapoints, xtime.Unit) {
	if len(i.timeseries) == 0 || i.idx < 0 || i.idx >= len(i.timeseries) {
		return nil, nil, 0
	}

	return i.tagIter, i.datapoints, xtime.Millisecond
}

func (i *promTSIter) Err() error {
	return nil
}

type tagIterator struct {
	numTags    int
	idx        int
	labels     []*prompb.Label
	nameBytes  *reuseableBytesID
	valueBytes *reuseableBytesID
	tag        ident.Tag
}

func newTagIterator() *tagIterator {
	i := &tagIterator{
		nameBytes:  &reuseableBytesID{},
		valueBytes: &reuseableBytesID{},
	}
	i.tag = ident.Tag{
		Name:  i.nameBytes,
		Value: i.valueBytes,
	}
	i.Reset(nil)
	return i
}

func (i *tagIterator) Reset(labels []*prompb.Label) {
	i.numTags = len(labels)
	i.idx = -1
	i.labels = labels
	i.nameBytes.reset(nil)
	i.valueBytes.reset(nil)
}

func (i *tagIterator) Next() bool {
	i.idx++
	next := i.idx < i.numTags
	if !next {
		return false
	}
	i.nameBytes.reset(i.labels[i.idx].Name)
	i.valueBytes.reset(i.labels[i.idx].Value)
	return true
}

func (i *tagIterator) Current() ident.Tag {
	return i.tag
}

func (i *tagIterator) CurrentIndex() int {
	return i.idx
}

func (i *tagIterator) Err() error {
	return nil
}

func (i *tagIterator) Close() {
	i.Reset(nil)
}

func (i *tagIterator) Len() int {
	return i.numTags
}

func (i *tagIterator) Remaining() int {
	if i.idx < 0 {
		return i.numTags
	}
	return i.numTags - i.idx
}

func (i *tagIterator) Duplicate() ident.TagIterator {
	result := newTagIterator()
	result.Reset(i.labels)
	return result
}

func (i *tagIterator) Restart() {
	i.Reset(i.labels)
}

var _ sort.Interface = labelsByName(nil)

type labelsByName []*prompb.Label

func (l labelsByName) Len() int {
	return len(l)
}

func (l labelsByName) Less(i, j int) bool {
	return bytes.Compare(l[i].Name, l[j].Name) < 0
}

func (l labelsByName) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

var _ ident.ID = &reuseableBytesID{}

type reuseableBytesID struct {
	bytes []byte
}

func (i *reuseableBytesID) reset(bytes []byte) {
	i.bytes = bytes
}

func (i *reuseableBytesID) Bytes() []byte {
	return i.bytes
}

func (i *reuseableBytesID) Equal(value ident.ID) bool {
	return bytes.Equal(i.bytes, value.Bytes())
}

func (i *reuseableBytesID) NoFinalize() {
}

func (i *reuseableBytesID) IsNoFinalize() bool {
	// Labels as IDs are always not able to be finalized as this ID is reused
	// with reset.
	return false
}

func (i *reuseableBytesID) Finalize() {
	// Noop.
}

func (i *reuseableBytesID) String() string {
	return string(i.bytes)
}

type writeBytesPool struct {
	pool sync.Pool
}

func newWriteBytesPool() *writeBytesPool {
	return &writeBytesPool{
		pool: sync.Pool{
			New: func() interface{} {
				return []byte(nil)
			},
		},
	}
}

func (p *writeBytesPool) Get() []byte {
	return p.pool.Get().([]byte)[:0]
}

func (p *writeBytesPool) Put(v []byte) {
	p.pool.Put(v)
}

// TODO remove? not needed
// type bufferPool struct {
// 	pool sync.Pool
// }

// func newBufferPool() *bufferPool {
// 	return &bufferPool{
// 		pool: sync.Pool{
// 			New: func() interface{} {
// 				return bytes.NewBuffer(nil)
// 			},
// 		},
// 	}
// }

// func (p *bufferPool) Get() *bytes.Buffer {
// 	return p.pool.Get().(*bytes.Buffer)
// }

// func (p *bufferPool) Put(v *bytes.Buffer) {
// 	v.Reset()
// 	p.pool.Put(v)
// }
