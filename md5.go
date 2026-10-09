package main

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
)

// partialMD5 is KOReader's name for a book by its contents (util.partialMD5 in its
// frontend/util.lua): an MD5 over 1 KB samples at 1024 << 2i for i = -1 … 10, stopping at the
// end of the file. KOReader shifts with LuaJIT's bit.lshift, which keeps the low five bits of the
// count, so i = -1 shifts by 30 and leaves nothing in 32 bits: the first sample is at 0, then
// 1 KB, 4 KB, 16 KB … 1 GB. A sample cut short by the end of the file still counts.
func partialMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	buf := make([]byte, 1024)
	for i := -1; i <= 10; i++ {
		var offset int64
		if i >= 0 {
			offset = int64(1024) << (2 * uint(i))
		}
		n, err := f.ReadAt(buf, offset)
		if n == 0 {
			break
		}
		h.Write(buf[:n])
		if err != nil && err != io.EOF {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
