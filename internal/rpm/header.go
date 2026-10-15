package rpm

import (
	"bytes"
	"encoding/binary"
	"io"
)

const r_MaxHeaderSize = 33554432

// headerMagic opens every header structure; the fourth byte is the version.
var headerMagic = []byte{0x8E, 0xAD, 0xE8}

// A Header stores metadata about an rpm package.
type Header struct {
	Version int
	Tags    map[int]*Tag
	Size    int
}

// GetTag returns the tag with the given identifier.
//
// Nil is returned if the specified tag does not exist or the header is nil.
func (c *Header) GetTag(id int) *Tag {
	if c == nil || len(c.Tags) == 0 {
		return nil
	}
	return c.Tags[id]
}

type rpmHeader [16]byte

func (b rpmHeader) Magic() []byte   { return b[:3] }
func (b rpmHeader) Version() int    { return int(b[3]) }
func (b rpmHeader) IndexCount() int { return int(binary.BigEndian.Uint32(b[8:12])) }
func (b rpmHeader) Size() int       { return int(binary.BigEndian.Uint32(b[12:16])) }

type rpmIndex [16]byte

func (b rpmIndex) Tag() int        { return int(binary.BigEndian.Uint32(b[:4])) }
func (b rpmIndex) Type() TagType   { return TagType(binary.BigEndian.Uint32(b[4:8])) }
func (b rpmIndex) Offset() int     { return int(binary.BigEndian.Uint32(b[8:12])) }
func (b rpmIndex) ValueCount() int { return int(binary.BigEndian.Uint32(b[12:16])) }

// ParseHeader decodes one header structure (magic, index and data store, with
// no padding) from raw. It is for callers that read the header bytes
// themselves because they also need to hash them.
func ParseHeader(raw []byte) (*Header, error) {
	r := bytes.NewReader(raw)
	h, err := readHeader(r, false)
	if err != nil {
		return nil, err
	}
	if r.Len() != 0 {
		return nil, errorf("%d bytes after the end of the header", r.Len())
	}
	return h, nil
}

// readHeader reads an RPM package file header structure from r.
func readHeader(r io.Reader, pad bool) (*Header, error) {
	// decode the header structure header
	var hdrBytes rpmHeader
	if _, err := io.ReadFull(r, hdrBytes[:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(hdrBytes.Magic(), headerMagic) {
		return nil, errorf("bad header magic: % x", hdrBytes.Magic())
	}
	if hdrBytes.Size() > r_MaxHeaderSize {
		return nil, errorf(
			"header size exceeds the maximum of %d: %d",
			r_MaxHeaderSize,
			hdrBytes.Size(),
		)
	}
	if hdrBytes.IndexCount()*len(hdrBytes) > r_MaxHeaderSize {
		return nil, errorf(
			"header index size exceeds the maximum of %d: %d",
			r_MaxHeaderSize,
			hdrBytes.IndexCount()*len(hdrBytes),
		)
	}

	// decode the index
	indexBytes := make([]rpmIndex, hdrBytes.IndexCount())
	for i := range indexBytes {
		if _, err := io.ReadFull(r, indexBytes[i][:]); err != nil {
			return nil, err
		}
		if indexBytes[i].Offset() >= hdrBytes.Size() {
			return nil, errorf(
				"offset of index %d is out of range: %d",
				i,
				indexBytes[i].Offset(),
			)
		}
	}

	// decode the store
	tags := make(map[int]*Tag, len(indexBytes))
	buf := make([]byte, hdrBytes.Size())
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	for i, ix := range indexBytes {
		if ix.ValueCount() < 1 {
			return nil, errorf("invalid value count for index %d: %d", i, ix.ValueCount())
		}
		// Every value occupies at least one byte, so a count larger than
		// what is left of the store is a lie. Checking before allocating
		// keeps a forged count from claiming gigabytes of memory.
		o := ix.Offset()
		if ix.ValueCount() > len(buf)-o {
			return nil, errorf("value count for index %d runs past the data store: %d", i+1, ix.ValueCount())
		}
		var v any
		switch ix.Type() {
		case TagTypeBinary, TagTypeChar, TagTypeInt8:
			a := make([]byte, ix.ValueCount())
			copy(a, buf[o:o+ix.ValueCount()])
			v = a

		case TagTypeInt16, TagTypeInt32, TagTypeInt64:
			size := map[TagType]int{TagTypeInt16: 2, TagTypeInt32: 4, TagTypeInt64: 8}[ix.Type()]
			if o%size != 0 {
				return nil, errorf("%s value for index %d is not aligned", ix.Type(), i+1)
			}
			if ix.ValueCount() > (len(buf)-o)/size {
				return nil, errorf("%s value for index %d is out of range", ix.Type(), i+1)
			}
			a := make([]int64, ix.ValueCount())
			for n := range a {
				switch size {
				case 2:
					a[n] = int64(binary.BigEndian.Uint16(buf[o : o+2]))
				case 4:
					a[n] = int64(binary.BigEndian.Uint32(buf[o : o+4]))
				default:
					a[n] = int64(binary.BigEndian.Uint64(buf[o : o+8]))
				}
				o += size
			}
			v = a

		case TagTypeString, TagTypeStringArray, TagTypeI18NString:
			if ix.Type() == TagTypeString && ix.ValueCount() != 1 {
				return nil, errorf("string value for index %d has count %d", i+1, ix.ValueCount())
			}
			a := make([]string, ix.ValueCount())
			for s := range a {
				j := bytes.IndexByte(buf[o:], 0)
				if j < 0 {
					return nil, errorf("string value for index %d is not terminated", i+1)
				}
				a[s] = string(buf[o : o+j])
				o += j + 1
			}
			v = a

		case TagTypeNull:
			// nothing to do here

		default:
			// unknown data type
			return nil, errorf("unknown index data type: %0X", ix.Type())
		}
		if _, dup := tags[ix.Tag()]; dup {
			return nil, errorf("tag %d appears more than once", ix.Tag())
		}
		tags[ix.Tag()] = &Tag{
			ID:    ix.Tag(),
			Type:  ix.Type(),
			Value: v,
		}
	}

	// pad to next header
	var padding int64
	if pad {
		if padding = int64(8-(hdrBytes.Size()%8)) % 8; padding != 0 {
			if _, err := io.CopyN(io.Discard, r, padding); err != nil {
				return nil, err
			}
		}
	}

	return &Header{
		Version: hdrBytes.Version(),
		Tags:    tags,
		Size:    16 + hdrBytes.Size() + hdrBytes.IndexCount()*16 + int(padding),
	}, nil
}
