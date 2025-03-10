// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tsdb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/go-kit/log/level"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/index"
)

func (h *Head) ExemplarQuerier(ctx context.Context) (storage.ExemplarQuerier, error) {
	return h.exemplars.ExemplarQuerier(ctx)
}

// Index returns an IndexReader against the block.
func (h *Head) Index() (IndexReader, error) {
	return h.indexRange(math.MinInt64, math.MaxInt64), nil
}

func (h *Head) indexRange(mint, maxt int64) *headIndexReader {
	if hmin := h.MinTime(); hmin > mint {
		mint = hmin
	}
	return &headIndexReader{head: h, mint: mint, maxt: maxt}
}

type headIndexReader struct {
	head       *Head
	mint, maxt int64
}

func (h *headIndexReader) Close() error {
	return nil
}

func (h *headIndexReader) Symbols() index.StringIter {
	return h.head.postings.Symbols()
}

// SortedLabelValues returns label values present in the head for the
// specific label name that are within the time range mint to maxt.
// If matchers are specified the returned result set is reduced
// to label values of metrics matching the matchers.
func (h *headIndexReader) SortedLabelValues(ctx context.Context, name string, matchers ...*labels.Matcher) ([]string, error) {
	values, err := h.LabelValues(ctx, name, matchers...)
	if err == nil {
		slices.Sort(values)
	}
	return values, err
}

// LabelValues returns label values present in the head for the
// specific label name that are within the time range mint to maxt.
// If matchers are specified the returned result set is reduced
// to label values of metrics matching the matchers.
func (h *headIndexReader) LabelValues(ctx context.Context, name string, matchers ...*labels.Matcher) ([]string, error) {
	if h.maxt < h.head.MinTime() || h.mint > h.head.MaxTime() {
		return []string{}, nil
	}

	if len(matchers) == 0 {
		return h.head.postings.LabelValues(ctx, name), nil
	}

	return labelValuesWithMatchers(ctx, h, name, matchers...)
}

// LabelNames returns all the unique label names present in the head
// that are within the time range mint to maxt.
func (h *headIndexReader) LabelNames(ctx context.Context, matchers ...*labels.Matcher) ([]string, error) {
	if h.maxt < h.head.MinTime() || h.mint > h.head.MaxTime() {
		return []string{}, nil
	}

	if len(matchers) == 0 {
		labelNames := h.head.postings.LabelNames()
		slices.Sort(labelNames)
		return labelNames, nil
	}

	return labelNamesWithMatchers(ctx, h, matchers...)
}

// Postings returns the postings list iterator for the label pairs.
func (h *headIndexReader) Postings(ctx context.Context, name string, values ...string) (index.Postings, error) {
	switch len(values) {
	case 0:
		return index.EmptyPostings(), nil
	case 1:
		return h.head.postings.Get(name, values[0]), nil
	default:
		res := make([]index.Postings, 0, len(values))
		for _, value := range values {
			if p := h.head.postings.Get(name, value); !index.IsEmptyPostingsType(p) {
				res = append(res, p)
			}
		}
		return index.Merge(ctx, res...), nil
	}
}

func (h *headIndexReader) PostingsForLabelMatching(ctx context.Context, name string, match func(string) bool) index.Postings {
	return h.head.postings.PostingsForLabelMatching(ctx, name, match)
}

func (h *headIndexReader) PostingsForMatchers(ctx context.Context, concurrent bool, ms ...*labels.Matcher) (index.Postings, error) {
	return h.head.pfmc.PostingsForMatchers(ctx, h, concurrent, ms...)
}

func (h *headIndexReader) SortedPostings(p index.Postings) index.Postings {
	series := make([]*memSeries, 0, 128)

	// Fetch all the series only once.
	for p.Next() {
		s := h.head.series.getByID(chunks.HeadSeriesRef(p.At()))
		if s == nil {
			level.Debug(h.head.logger).Log("msg", "Looked up series not found")
		} else {
			series = append(series, s)
		}
	}
	if err := p.Err(); err != nil {
		return index.ErrPostings(fmt.Errorf("expand postings: %w", err))
	}

	slices.SortFunc(series, func(a, b *memSeries) int {
		return labels.Compare(a.labels(), b.labels())
	})

	// Convert back to list.
	ep := make([]storage.SeriesRef, 0, len(series))
	for _, p := range series {
		ep = append(ep, storage.SeriesRef(p.ref))
	}
	return index.NewListPostings(ep)
}

// ShardedPostings implements IndexReader. This function returns an failing postings list if sharding
// has not been enabled in the Head.
func (h *headIndexReader) ShardedPostings(p index.Postings, shardIndex, shardCount uint64) index.Postings {
	if !h.head.opts.EnableSharding {
		return index.ErrPostings(errors.New("sharding is disabled"))
	}

	out := make([]storage.SeriesRef, 0, 128)

	for p.Next() {
		s := h.head.series.getByID(chunks.HeadSeriesRef(p.At()))
		if s == nil {
			level.Debug(h.head.logger).Log("msg", "Looked up series not found")
			continue
		}

		// Check if the series belong to the shard.
		if s.shardHash%shardCount != shardIndex {
			continue
		}

		out = append(out, storage.SeriesRef(s.ref))
	}

	return index.NewListPostings(out)
}

// LabelValuesFor returns LabelValues for the given label name in the series referred to by postings.
func (h *headIndexReader) LabelValuesFor(postings index.Postings, name string) storage.LabelValues {
	return h.head.postings.LabelValuesFor(postings, name)
}

// LabelValuesExcluding returns LabelValues for the given label name in all other series than those referred to by postings.
// This is useful for obtaining label values for other postings than the ones you wish to exclude.
func (h *headIndexReader) LabelValuesExcluding(postings index.Postings, name string) storage.LabelValues {
	return h.head.postings.LabelValuesExcluding(postings, name)
}

// Series returns the series for the given reference.
// Chunks are skipped if chks is nil.
func (h *headIndexReader) Series(ref storage.SeriesRef, builder *labels.ScratchBuilder, chks *[]chunks.Meta) error {
	s := h.head.series.getByID(chunks.HeadSeriesRef(ref))

	if s == nil {
		h.head.metrics.seriesNotFound.Inc()
		return storage.ErrNotFound
	}
	builder.Assign(s.labels())

	if chks == nil {
		return nil
	}

	s.Lock()
	defer s.Unlock()

	*chks = (*chks)[:0]

	for i, c := range s.mmappedChunks {
		// Do not expose chunks that are outside of the specified range.
		if !c.OverlapsClosedInterval(h.mint, h.maxt) {
			continue
		}
		*chks = append(*chks, chunks.Meta{
			MinTime: c.minTime,
			MaxTime: c.maxTime,
			Ref:     chunks.ChunkRef(chunks.NewHeadChunkRef(s.ref, s.headChunkID(i))),
		})
	}

	if s.headChunks != nil {
		var maxTime int64
		var i, j int
		for i = s.headChunks.len() - 1; i >= 0; i-- {
			chk := s.headChunks.atOffset(i)
			if i == 0 {
				// Set the head chunk as open (being appended to) for the first headChunk.
				maxTime = math.MaxInt64
			} else {
				maxTime = chk.maxTime
			}
			if chk.OverlapsClosedInterval(h.mint, h.maxt) {
				*chks = append(*chks, chunks.Meta{
					MinTime: chk.minTime,
					MaxTime: maxTime,
					Ref:     chunks.ChunkRef(chunks.NewHeadChunkRef(s.ref, s.headChunkID(len(s.mmappedChunks)+j))),
				})
			}
			j++
		}
	}

	return nil
}

// headChunkID returns the HeadChunkID referred to by the given position.
// * 0 <= pos < len(s.mmappedChunks) refer to s.mmappedChunks[pos]
// * pos >= len(s.mmappedChunks) refers to s.headChunks linked list.
func (s *memSeries) headChunkID(pos int) chunks.HeadChunkID {
	return chunks.HeadChunkID(pos) + s.firstChunkID
}

// oooHeadChunkID returns the HeadChunkID referred to by the given position.
// * 0 <= pos < len(s.oooMmappedChunks) refer to s.oooMmappedChunks[pos]
// * pos == len(s.oooMmappedChunks) refers to s.oooHeadChunk
// The caller must ensure that s.ooo is not nil.
func (s *memSeries) oooHeadChunkID(pos int) chunks.HeadChunkID {
	return chunks.HeadChunkID(pos) + s.ooo.firstOOOChunkID
}

// LabelValueFor returns label value for the given label name in the series referred to by ID.
func (h *headIndexReader) LabelValueFor(_ context.Context, id storage.SeriesRef, label string) (string, error) {
	memSeries := h.head.series.getByID(chunks.HeadSeriesRef(id))
	if memSeries == nil {
		return "", storage.ErrNotFound
	}

	value := memSeries.labels().Get(label)
	if value == "" {
		return "", storage.ErrNotFound
	}

	return value, nil
}

// LabelNamesFor returns all the label names for the series referred to by the postings.
// The names returned are sorted.
func (h *headIndexReader) LabelNamesFor(ctx context.Context, series index.Postings) ([]string, error) {
	namesMap := make(map[string]struct{})
	i := 0
	for series.Next() {
		i++
		if i%checkContextEveryNIterations == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		memSeries := h.head.series.getByID(chunks.HeadSeriesRef(series.At()))
		if memSeries == nil {
			// Series not found, this happens during compaction,
			// when series was garbage collected after the caller got the series IDs.
			continue
		}
		memSeries.labels().Range(func(lbl labels.Label) {
			namesMap[lbl.Name] = struct{}{}
		})
	}
	if err := series.Err(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(namesMap))
	for name := range namesMap {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// Chunks returns a ChunkReader against the block.
func (h *Head) Chunks() (ChunkReader, error) {
	return h.chunksRange(math.MinInt64, math.MaxInt64, h.iso.State(math.MinInt64, math.MaxInt64))
}

func (h *Head) chunksRange(mint, maxt int64, is *isolationState) (*headChunkReader, error) {
	h.closedMtx.Lock()
	defer h.closedMtx.Unlock()
	if h.closed {
		return nil, errors.New("can't read from a closed head")
	}
	if hmin := h.MinTime(); hmin > mint {
		mint = hmin
	}
	return &headChunkReader{
		head:     h,
		mint:     mint,
		maxt:     maxt,
		isoState: is,
	}, nil
}

type headChunkReader struct {
	head       *Head
	mint, maxt int64
	isoState   *isolationState
}

func (h *headChunkReader) Close() error {
	if h.isoState != nil {
		h.isoState.Close()
	}
	return nil
}

// ChunkOrIterable returns the chunk for the reference number.
func (h *headChunkReader) ChunkOrIterable(meta chunks.Meta) (chunkenc.Chunk, chunkenc.Iterable, error) {
	chk, _, err := h.chunk(meta, false)
	return chk, nil, err
}

// ChunkWithCopy returns the chunk for the reference number.
// If the chunk is the in-memory chunk, then it makes a copy and returns the copied chunk.
func (h *headChunkReader) ChunkWithCopy(meta chunks.Meta) (chunkenc.Chunk, int64, error) {
	return h.chunk(meta, true)
}

// chunk returns the chunk for the reference number.
// If copyLastChunk is true, then it makes a copy of the head chunk if asked for it.
// Also returns max time of the chunk.
func (h *headChunkReader) chunk(meta chunks.Meta, copyLastChunk bool) (chunkenc.Chunk, int64, error) {
	sid, cid := chunks.HeadChunkRef(meta.Ref).Unpack()

	s := h.head.series.getByID(sid)
	// This means that the series has been garbage collected.
	if s == nil {
		return nil, 0, storage.ErrNotFound
	}

	s.Lock()
	c, headChunk, isOpen, err := s.chunk(cid, h.head.chunkDiskMapper, &h.head.memChunkPool)
	if err != nil {
		s.Unlock()
		return nil, 0, err
	}
	defer func() {
		if !headChunk {
			// Set this to nil so that Go GC can collect it after it has been used.
			c.chunk = nil
			c.prev = nil
			h.head.memChunkPool.Put(c)
		}
	}()

	// This means that the chunk is outside the specified range.
	if !c.OverlapsClosedInterval(h.mint, h.maxt) {
		s.Unlock()
		return nil, 0, storage.ErrNotFound
	}

	chk, maxTime := c.chunk, c.maxTime
	if headChunk && isOpen && copyLastChunk {
		// The caller may ask to copy the head chunk in order to take the
		// bytes of the chunk without causing the race between read and append.
		b := s.headChunks.chunk.Bytes()
		newB := make([]byte, len(b))
		copy(newB, b) // TODO(codesome): Use bytes.Clone() when we upgrade to Go 1.20.
		// TODO(codesome): Put back in the pool (non-trivial).
		chk, err = h.head.opts.ChunkPool.Get(s.headChunks.chunk.Encoding(), newB)
		if err != nil {
			return nil, 0, err
		}
	}
	s.Unlock()

	return &safeHeadChunk{
		Chunk:    chk,
		s:        s,
		cid:      cid,
		isoState: h.isoState,
	}, maxTime, nil
}

// chunk returns the chunk for the HeadChunkID from memory or by m-mapping it from the disk.
// If headChunk is false, it means that the returned *memChunk
// (and not the chunkenc.Chunk inside it) can be garbage collected after its usage.
// if isOpen is true, it means that the returned *memChunk is used for appends.
func (s *memSeries) chunk(id chunks.HeadChunkID, cdm chunkDiskMapper, memChunkPool *sync.Pool) (chunk *memChunk, headChunk, isOpen bool, err error) {
	// ix represents the index of chunk in the s.mmappedChunks slice. The chunk id's are
	// incremented by 1 when new chunk is created, hence (id - firstChunkID) gives the slice index.
	// The max index for the s.mmappedChunks slice can be len(s.mmappedChunks)-1, hence if the ix
	// is >= len(s.mmappedChunks), it represents one of the chunks on s.headChunks linked list.
	// The order of elemens is different for slice and linked list.
	// For s.mmappedChunks slice newer chunks are appended to it.
	// For s.headChunks list newer chunks are prepended to it.
	//
	// memSeries {
	//   mmappedChunks: [t0, t1, t2]
	//   headChunk:     {t5}->{t4}->{t3}
	// }
	ix := int(id) - int(s.firstChunkID)

	var headChunksLen int
	if s.headChunks != nil {
		headChunksLen = s.headChunks.len()
	}

	if ix < 0 || ix > len(s.mmappedChunks)+headChunksLen-1 {
		return nil, false, false, storage.ErrNotFound
	}

	if ix < len(s.mmappedChunks) {
		chk, err := cdm.Chunk(s.mmappedChunks[ix].ref)
		if err != nil {
			var cerr *chunks.CorruptionErr
			if errors.As(err, &cerr) {
				panic(err)
			}
			return nil, false, false, err
		}
		mc := memChunkPool.Get().(*memChunk)
		mc.chunk = chk
		mc.minTime = s.mmappedChunks[ix].minTime
		mc.maxTime = s.mmappedChunks[ix].maxTime
		return mc, false, false, nil
	}

	ix -= len(s.mmappedChunks)

	offset := headChunksLen - ix - 1
	// headChunks is a linked list where first element is the most recent one and the last one is the oldest.
	// This order is reversed when compared with mmappedChunks, since mmappedChunks[0] is the oldest chunk,
	// while headChunk.atOffset(0) would give us the most recent chunk.
	// So when calling headChunk.atOffset() we need to reverse the value of ix.
	elem := s.headChunks.atOffset(offset)
	if elem == nil {
		// This should never really happen and would mean that headChunksLen value is NOT equal
		// to the length of the headChunks list.
		return nil, false, false, storage.ErrNotFound
	}
	return elem, true, offset == 0, nil
}

// oooMergedChunks return an iterable over one or more OOO chunks for the given
// chunks.Meta reference from memory or by m-mapping it from the disk. The
// returned iterable will be a merge of all the overlapping chunks, if any,
// amongst all the chunks in the OOOHead.
// This function is not thread safe unless the caller holds a lock.
// The caller must ensure that s.ooo is not nil.
func (s *memSeries) oooMergedChunks(meta chunks.Meta, cdm chunkDiskMapper, mint, maxt int64, maxMmapRef chunks.ChunkDiskMapperRef) (*mergedOOOChunks, error) {
	_, cid := chunks.HeadChunkRef(meta.Ref).Unpack()

	// ix represents the index of chunk in the s.mmappedChunks slice. The chunk meta's are
	// incremented by 1 when new chunk is created, hence (meta - firstChunkID) gives the slice index.
	// The max index for the s.mmappedChunks slice can be len(s.mmappedChunks)-1, hence if the ix
	// is len(s.mmappedChunks), it represents the next chunk, which is the head chunk.
	ix := int(cid) - int(s.ooo.firstOOOChunkID)
	if ix < 0 || ix > len(s.ooo.oooMmappedChunks) {
		return nil, storage.ErrNotFound
	}

	if ix == len(s.ooo.oooMmappedChunks) {
		if s.ooo.oooHeadChunk == nil {
			return nil, errors.New("invalid ooo head chunk")
		}
	}

	// We create a temporary slice of chunk metas to hold the information of all
	// possible chunks that may overlap with the requested chunk.
	tmpChks := make([]chunkMetaAndChunkDiskMapperRef, 0, len(s.ooo.oooMmappedChunks)+1)

	for i, c := range s.ooo.oooMmappedChunks {
		if maxMmapRef != 0 && c.ref > maxMmapRef {
			break
		}
		if c.OverlapsClosedInterval(mint, maxt) {
			tmpChks = append(tmpChks, chunkMetaAndChunkDiskMapperRef{
				meta: chunks.Meta{
					MinTime: c.minTime,
					MaxTime: c.maxTime,
					Ref:     chunks.ChunkRef(chunks.NewHeadChunkRef(s.ref, s.oooHeadChunkID(i))),
				},
				ref: c.ref,
			})
		}
	}
	// Add in data copied from the head OOO chunk.
	if meta.Chunk != nil {
		tmpChks = append(tmpChks, chunkMetaAndChunkDiskMapperRef{meta: meta})
	}

	// Next we want to sort all the collected chunks by min time so we can find
	// those that overlap and stop when we know the rest don't.
	slices.SortFunc(tmpChks, refLessByMinTimeAndMinRef)

	mc := &mergedOOOChunks{}
	absoluteMax := int64(math.MinInt64)
	for _, c := range tmpChks {
		if c.meta.Ref != meta.Ref && (len(mc.chunkIterables) == 0 || c.meta.MinTime > absoluteMax) {
			continue
		}
		var iterable chunkenc.Iterable
		if c.meta.Chunk != nil {
			iterable = c.meta.Chunk
		} else {
			chk, err := cdm.Chunk(c.ref)
			if err != nil {
				var cerr *chunks.CorruptionErr
				if errors.As(err, &cerr) {
					return nil, fmt.Errorf("invalid ooo mmapped chunk: %w", err)
				}
				return nil, err
			}
			iterable = chk
		}
		mc.chunkIterables = append(mc.chunkIterables, iterable)
		if c.meta.MaxTime > absoluteMax {
			absoluteMax = c.meta.MaxTime
		}
	}

	return mc, nil
}

// safeHeadChunk makes sure that the chunk can be accessed without a race condition.
type safeHeadChunk struct {
	chunkenc.Chunk
	s        *memSeries
	cid      chunks.HeadChunkID
	isoState *isolationState
}

func (c *safeHeadChunk) Iterator(reuseIter chunkenc.Iterator) chunkenc.Iterator {
	c.s.Lock()
	it := c.s.iterator(c.cid, c.Chunk, c.isoState, reuseIter)
	c.s.Unlock()
	return it
}

// iterator returns a chunk iterator for the requested chunkID, or a NopIterator if the requested ID is out of range.
// It is unsafe to call this concurrently with s.append(...) without holding the series lock.
func (s *memSeries) iterator(id chunks.HeadChunkID, c chunkenc.Chunk, isoState *isolationState, it chunkenc.Iterator) chunkenc.Iterator {
	ix := int(id) - int(s.firstChunkID)

	numSamples := c.NumSamples()
	stopAfter := numSamples

	if isoState != nil && !isoState.IsolationDisabled() {
		totalSamples := 0    // Total samples in this series.
		previousSamples := 0 // Samples before this chunk.

		for j, d := range s.mmappedChunks {
			totalSamples += int(d.numSamples)
			if j < ix {
				previousSamples += int(d.numSamples)
			}
		}

		ix -= len(s.mmappedChunks)
		if s.headChunks != nil {
			// Iterate all head chunks from the oldest to the newest.
			headChunksLen := s.headChunks.len()
			for j := headChunksLen - 1; j >= 0; j-- {
				chk := s.headChunks.atOffset(j)
				chkSamples := chk.chunk.NumSamples()
				totalSamples += chkSamples
				// Chunk ID is len(s.mmappedChunks) + $(headChunks list position).
				// Where $(headChunks list position) is zero for the oldest chunk and $(s.headChunks.len() - 1)
				// for the newest (open) chunk.
				if headChunksLen-1-j < ix {
					previousSamples += chkSamples
				}
			}
		}

		// Removing the extra transactionIDs that are relevant for samples that
		// come after this chunk, from the total transactionIDs.
		appendIDsToConsider := int(s.txs.txIDCount) - (totalSamples - (previousSamples + numSamples))

		// Iterate over the appendIDs, find the first one that the isolation state says not
		// to return.
		it := s.txs.iterator()
		for index := 0; index < appendIDsToConsider; index++ {
			appendID := it.At()
			if appendID <= isoState.maxAppendID { // Easy check first.
				if _, ok := isoState.incompleteAppends[appendID]; !ok {
					it.Next()
					continue
				}
			}
			stopAfter = numSamples - (appendIDsToConsider - index)
			if stopAfter < 0 {
				stopAfter = 0 // Stopped in a previous chunk.
			}
			break
		}
	}

	if stopAfter == 0 {
		return chunkenc.NewNopIterator()
	}
	if stopAfter == numSamples {
		return c.Iterator(it)
	}
	return makeStopIterator(c, it, stopAfter)
}

// stopIterator wraps an Iterator, but only returns the first
// stopAfter values, if initialized with i=-1.
type stopIterator struct {
	chunkenc.Iterator

	i, stopAfter int
}

func (it *stopIterator) Next() chunkenc.ValueType {
	if it.i+1 >= it.stopAfter {
		return chunkenc.ValNone
	}
	it.i++
	return it.Iterator.Next()
}

func makeStopIterator(c chunkenc.Chunk, it chunkenc.Iterator, stopAfter int) chunkenc.Iterator {
	// Re-use the Iterator object if it is a stopIterator.
	if stopIter, ok := it.(*stopIterator); ok {
		stopIter.Iterator = c.Iterator(stopIter.Iterator)
		stopIter.i = -1
		stopIter.stopAfter = stopAfter
		return stopIter
	}

	return &stopIterator{
		Iterator:  c.Iterator(it),
		i:         -1,
		stopAfter: stopAfter,
	}
}
