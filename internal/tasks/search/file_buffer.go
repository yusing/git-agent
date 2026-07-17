package search

import (
	"errors"
	"io"
	"os"
	"slices"

	"github.com/yusing/goutils/synk"
)

var (
	errSearchFileOversized = errors.New("search file exceeds maximum size")
	recyclableBytes        = synk.GetUnsizedBytesPool()
)

type searchFileBuffer struct {
	data []byte
}

func readSearchFile(path string, sizeHint int) (searchFileBuffer, error) {
	file, err := os.Open(path)
	if err != nil {
		return searchFileBuffer{}, err
	}
	defer file.Close()

	const readLimit = MaxFileBytes + 1
	initialSize := min(max(sizeHint+1, 1), readLimit)
	data := recyclableBytes.GetAtLeast(initialSize)
	data = data[:0:min(cap(data), readLimit)]
	for {
		if len(data) == cap(data) {
			if len(data) == readLimit {
				recyclableBytes.Put(data)
				return searchFileBuffer{}, errSearchFileOversized
			}
			previous := data
			growBy := min(max(cap(data), 1), readLimit-len(data))
			data = slices.Grow(data, growBy)
			data = data[:len(data):min(cap(data), readLimit)]
			recyclableBytes.Put(previous)
		}

		n, readErr := file.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if len(data) == readLimit {
			recyclableBytes.Put(data)
			return searchFileBuffer{}, errSearchFileOversized
		}
		switch readErr {
		case nil:
			if n == 0 {
				recyclableBytes.Put(data)
				return searchFileBuffer{}, io.ErrNoProgress
			}
		case io.EOF:
			return searchFileBuffer{data: data}, nil
		default:
			recyclableBytes.Put(data)
			return searchFileBuffer{}, readErr
		}
	}
}

func (b *searchFileBuffer) release() {
	if b.data == nil {
		return
	}
	recyclableBytes.Put(b.data)
	b.data = nil
}
