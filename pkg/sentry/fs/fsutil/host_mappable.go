// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fsutil

import (
	"math"
	"sync"

	"gvisor.googlesource.com/gvisor/pkg/sentry/context"
	"gvisor.googlesource.com/gvisor/pkg/sentry/fs"
	"gvisor.googlesource.com/gvisor/pkg/sentry/memmap"
	"gvisor.googlesource.com/gvisor/pkg/sentry/platform"
	"gvisor.googlesource.com/gvisor/pkg/sentry/safemem"
	"gvisor.googlesource.com/gvisor/pkg/sentry/usermem"
)

// HostMappable implements memmap.Mappable and platform.File over a
// CachedFileObject.
//
// Lock order (compare the lock order model in mm/mm.go):
//   truncateMu ("fs locks")
//     mu ("memmap.Mappable locks not taken by Translate")
//       ("platform.File locks")
//   	     backingFile ("CachedFileObject locks")
//
// +stateify savable
type HostMappable struct {
	hostFileMapper *HostFileMapper

	backingFile CachedFileObject

	mu sync.Mutex `state:"nosave"`

	// mappings tracks mappings of the cached file object into
	// memmap.MappingSpaces so it can invalidated upon save. Protected by mu.
	mappings memmap.MappingSet

	// truncateMu protects writes and truncations. See Truncate() for details.
	truncateMu sync.RWMutex `state:"nosave"`
}

// NewHostMappable creates a new mappable that maps directly to host FD.
func NewHostMappable(backingFile CachedFileObject) *HostMappable {
	return &HostMappable{
		hostFileMapper: NewHostFileMapper(),
		backingFile:    backingFile,
	}
}

// AddMapping implements memmap.Mappable.AddMapping.
func (h *HostMappable) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar usermem.AddrRange, offset uint64, writable bool) error {
	// Hot path. Avoid defers.
	h.mu.Lock()
	mapped := h.mappings.AddMapping(ms, ar, offset, writable)
	for _, r := range mapped {
		h.hostFileMapper.IncRefOn(r)
	}
	h.mu.Unlock()
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (h *HostMappable) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar usermem.AddrRange, offset uint64, writable bool) {
	// Hot path. Avoid defers.
	h.mu.Lock()
	unmapped := h.mappings.RemoveMapping(ms, ar, offset, writable)
	for _, r := range unmapped {
		h.hostFileMapper.DecRefOn(r)
	}
	h.mu.Unlock()
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (h *HostMappable) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR usermem.AddrRange, offset uint64, writable bool) error {
	return h.AddMapping(ctx, ms, dstAR, offset, writable)
}

// Translate implements memmap.Mappable.Translate.
func (h *HostMappable) Translate(ctx context.Context, required, optional memmap.MappableRange, at usermem.AccessType) ([]memmap.Translation, error) {
	return []memmap.Translation{
		{
			Source: optional,
			File:   h,
			Offset: optional.Start,
		},
	}, nil
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
func (h *HostMappable) InvalidateUnsavable(ctx context.Context) error {
	h.mu.Lock()
	h.mappings.InvalidateAll(memmap.InvalidateOpts{})
	h.mu.Unlock()
	return nil
}

// MapInto implements platform.File.MapInto.
func (h *HostMappable) MapInto(as platform.AddressSpace, addr usermem.Addr, fr platform.FileRange, at usermem.AccessType, precommit bool) error {
	return as.MapFile(addr, h.backingFile.FD(), fr, at, precommit)
}

// MapInternal implements platform.File.MapInternal.
func (h *HostMappable) MapInternal(fr platform.FileRange, at usermem.AccessType) (safemem.BlockSeq, error) {
	return h.hostFileMapper.MapInternal(fr, h.backingFile.FD(), at.Write)
}

// IncRef implements platform.File.IncRef.
func (h *HostMappable) IncRef(fr platform.FileRange) {
	mr := memmap.MappableRange{Start: fr.Start, End: fr.End}
	h.hostFileMapper.IncRefOn(mr)
}

// DecRef implements platform.File.DecRef.
func (h *HostMappable) DecRef(fr platform.FileRange) {
	mr := memmap.MappableRange{Start: fr.Start, End: fr.End}
	h.hostFileMapper.DecRefOn(mr)
}

// Truncate truncates the file, invalidating any mapping that may have been
// removed after the size change.
//
// Truncation and writes are synchronized to prevent races where writes make the
// file grow between truncation and invalidation below:
//   T1: Calls SetMaskedAttributes and stalls
//   T2: Appends to file causing it to grow
//   T2: Writes to mapped pages and COW happens
//   T1: Continues and wronly invalidates the page mapped in step above.
func (h *HostMappable) Truncate(ctx context.Context, newSize int64) error {
	h.truncateMu.Lock()
	defer h.truncateMu.Unlock()

	mask := fs.AttrMask{Size: true}
	attr := fs.UnstableAttr{Size: newSize}
	if err := h.backingFile.SetMaskedAttributes(ctx, mask, attr); err != nil {
		return err
	}

	// Invalidate COW mappings that may exist beyond the new size in case the file
	// is being shrunk. Other mappinsg don't need to be invalidated because
	// translate will just return identical mappings after invalidation anyway,
	// and SIGBUS will be raised and handled when the mappings are touched.
	//
	// Compare Linux's mm/truncate.c:truncate_setsize() =>
	// truncate_pagecache() =>
	// mm/memory.c:unmap_mapping_range(evencows=1).
	h.mu.Lock()
	defer h.mu.Unlock()
	mr := memmap.MappableRange{
		Start: fs.OffsetPageEnd(newSize),
		End:   fs.OffsetPageEnd(math.MaxInt64),
	}
	h.mappings.Invalidate(mr, memmap.InvalidateOpts{InvalidatePrivate: true})

	return nil
}

// Write writes to the file backing this mappable.
func (h *HostMappable) Write(ctx context.Context, src usermem.IOSequence, offset int64) (int64, error) {
	h.truncateMu.RLock()
	n, err := src.CopyInTo(ctx, &writer{ctx: ctx, hostMappable: h, off: offset})
	h.truncateMu.RUnlock()
	return n, err
}

type writer struct {
	ctx          context.Context
	hostMappable *HostMappable
	off          int64
}

// WriteFromBlocks implements safemem.Writer.WriteFromBlocks.
func (w *writer) WriteFromBlocks(src safemem.BlockSeq) (uint64, error) {
	n, err := w.hostMappable.backingFile.WriteFromBlocksAt(w.ctx, src, uint64(w.off))
	w.off += int64(n)
	return n, err
}