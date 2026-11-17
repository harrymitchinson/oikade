package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
)

// bbolt's file layout, as far as checkFile needs it. A page starts with a 16-byte header, and the
// first two pages are meta pages, each with the same fields in the machine's byte order. bbolt uses
// the valid one with the higher transaction ID.
const (
	boltPageHeader = 16
	metaMagic      = 0xED0CDAED
	metaVersion    = 2
	// boltMetaSummed is the meta's fields up to its checksum, which is an FNV-1a of them.
	boltMetaSummed = 56
	// minPageSize and maxPageSize bound where bbolt looks for the second meta page.
	minPageSize = 1 << 10
	maxPageSize = 1 << 24
)

// meta is what checkFile reads from a meta page.
type meta struct {
	pageSize       uint32
	root, freelist uint64
	pages, txid    uint64
}

// noFreelist is a freelist bbolt didn't write.
const noFreelist = ^uint64(0)

// readMeta reads the meta page at off, and says whether it's valid.
func readMeta(f *os.File, off int64) (meta, bool) {
	buf := make([]byte, boltPageHeader+boltMetaSummed+8)
	if _, err := f.ReadAt(buf, off); err != nil {
		return meta{}, false
	}
	b := buf[boltPageHeader:]
	order := binary.NativeEndian
	h := fnv.New64a()
	_, _ = h.Write(b[:boltMetaSummed])
	if order.Uint32(b[0:]) != metaMagic || order.Uint32(b[4:]) != metaVersion || order.Uint64(b[boltMetaSummed:]) != h.Sum64() {
		return meta{}, false
	}
	return meta{
		pageSize: order.Uint32(b[8:]), root: order.Uint64(b[16:]), freelist: order.Uint64(b[32:]),
		pages: order.Uint64(b[40:]), txid: order.Uint64(b[48:]),
	}, true
}

// checkFile refuses a file bbolt would crash on or can't read, with ErrFormat, before bbolt maps it:
// one that isn't a bbolt file, or one shorter than its meta page says. bbolt maps what the meta page
// says is there, so a file cut short faults the process when it reads past the end, which nothing
// can recover from. An empty or missing file is a new store.
func checkFile(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	// The first meta page is at the start, and says the page size, so where the second is. If it's
	// torn, bbolt looks for the second at each page size it could be.
	first, ok0 := readMeta(f, 0)
	var second meta
	var ok1 bool
	if ok0 {
		second, ok1 = readMeta(f, int64(first.pageSize))
	} else {
		for off := int64(minPageSize); off <= maxPageSize && off < size; off <<= 1 {
			if second, ok1 = readMeta(f, off); ok1 {
				break
			}
		}
	}
	// bbolt uses the newer valid meta page.
	m := first
	switch {
	case !ok0 && !ok1:
		return fmt.Errorf("%w: %s isn't a store", ErrFormat, path)
	case !ok0 || (ok1 && second.txid > first.txid):
		m = second
	}
	if m.pageSize < minPageSize || m.pageSize > maxPageSize || m.pages < 2 {
		return fmt.Errorf("%w: %s has a page size of %d and %d pages", ErrFormat, path, m.pageSize, m.pages)
	}
	if want := int64(m.pages) * int64(m.pageSize); size < want {
		return fmt.Errorf("%w: %s is cut short: it has %d bytes of %d", ErrFormat, path, size, want)
	}
	if m.root >= m.pages || (m.freelist != noFreelist && m.freelist >= m.pages) {
		return fmt.Errorf("%w: %s's meta page points past its %d pages", ErrFormat, path, m.pages)
	}
	return nil
}
