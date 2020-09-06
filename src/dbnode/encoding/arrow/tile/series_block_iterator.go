// Copyright (c) 2020 Uber Technologies, Inc.
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

package tile

import (
	"github.com/apache/arrow/go/arrow/memory"
	"github.com/m3db/m3/src/dbnode/persist/fs"
	"github.com/m3db/m3/src/x/ident"
	xtime "github.com/m3db/m3/src/x/time"
)

type seriesBlockIter struct {
	reader fs.CrossBlockReader

	err       error
	exhausted bool

	step  xtime.UnixNano
	start xtime.UnixNano

	recorder  recorder
	iter      SeriesFrameIterator
	blockIter fs.CrossBlockIterator
	tags      ident.TagIterator
	id        ident.ID
}

// NewSeriesBlockIterator creates a new SeriesBlockIterator.
func NewSeriesBlockIterator(
	reader fs.CrossBlockReader,
	opts Options,
) (SeriesBlockIterator, error) {
	var recorder recorder
	if opts.UseArrow {
		recorder = newDatapointRecorder(memory.NewGoAllocator())
	} else {
		recorder = newFlatDatapointRecorder()
	}

	return &seriesBlockIter{
		reader: reader,

		start: opts.Start,
		step:  opts.FrameSize,

		recorder:  recorder,
		iter:      newSeriesFrameIterator(recorder),
		blockIter: fs.NewCrossBlockIterator(opts.ReaderIteratorPool),
	}, nil
}

func (b *seriesBlockIter) Next() bool {
	if b.exhausted || b.err != nil {
		return false
	}

	if !b.reader.Next() {
		b.exhausted = true
		b.err = b.reader.Err()
		return false
	}

	var blockRecords []fs.BlockRecord
	b.id, b.tags, blockRecords = b.reader.Current()
	b.blockIter.Reset(blockRecords)
	b.iter.Reset(b.start, b.step, b.blockIter)
	return true
}

func (b *seriesBlockIter) Current() (SeriesFrameIterator, ident.ID, ident.TagIterator) {
	return b.iter, b.id, b.tags
}

func (b *seriesBlockIter) Close() error {
	b.recorder.release()
	b.blockIter.Close()
	return b.iter.Close()
}

func (b *seriesBlockIter) Err() error {
	return b.err
}
