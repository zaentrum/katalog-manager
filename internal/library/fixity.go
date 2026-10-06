package library

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// qh1Chunk is how much of each end of a file its quick hash reads.
const qh1Chunk = 64 << 10

// QH1 is the size of the file at path and its quick hash, as the library
// records a file's fixity: "sha256:" and the hex SHA-256 of its first 64 KiB,
// its last 64 KiB (the second read only for a file larger than 64 KiB) and
// its size as a big-endian uint64. Two reads however large the file is; it
// tells the file again after a move, and is no hash of its content.
func QH1(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	size := fi.Size()
	h := sha256.New()
	buf := make([]byte, qh1Chunk)
	read := func() error {
		n, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		h.Write(buf[:n])
		return nil
	}
	if err := read(); err != nil {
		return 0, "", err
	}
	if size > qh1Chunk {
		if _, err := f.Seek(size-qh1Chunk, io.SeekStart); err != nil {
			return 0, "", err
		}
		if err := read(); err != nil {
			return 0, "", err
		}
	}
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(size))
	h.Write(n[:])
	return size, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
