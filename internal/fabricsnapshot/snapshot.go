package fabricsnapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const formatVersion = byte(1)

func decodeNamespaces(name string, data []byte) ([]Namespace, error) {
	if len(data) == 0 || data[0] != formatVersion {
		return nil, fmt.Errorf("%s: unsupported format byte", name)
	}
	count, offset, err := consumeUvarint(data, 1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if count > uint64(len(data)-offset)/2 {
		return nil, fmt.Errorf("%s: namespace count exceeds metadata size", name)
	}
	result := make([]Namespace, 0, count)
	for range count {
		namespace, next, err := sized(data, offset)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		offset = next
		records, next, err := consumeUvarint(data, offset)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		offset = next
		result = append(result, Namespace{Name: string(namespace), Records: records})
	}
	if offset != len(data) {
		return nil, fmt.Errorf("%s: %d trailing bytes", name, len(data)-offset)
	}
	return result, nil
}

func protobufBytes(data []byte) (map[uint64][]byte, error) {
	fields := map[uint64][]byte{}
	seen := map[uint64]bool{}
	for offset := 0; offset < len(data); {
		tag, next, err := consumeUvarint(data, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		field := tag >> 3
		if tag&7 != 2 || field < 1 || field > 4 || seen[field] {
			return nil, fmt.Errorf("unsupported or duplicate protobuf field %d", field)
		}
		value, next, err := sized(data, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		fields[field] = value
		seen[field] = true
	}
	return fields, nil
}

func decodeHeight(data []byte) (uint64, uint64, error) {
	block, offset, err := orderPreservingUint(data, 0)
	if err != nil {
		return 0, 0, err
	}
	transaction, offset, err := orderPreservingUint(data, offset)
	if err != nil {
		return 0, 0, err
	}
	if offset != len(data) {
		return 0, 0, errors.New("fabric height has trailing bytes")
	}
	return block, transaction, nil
}

func orderPreservingUint(data []byte, offset int) (uint64, int, error) {
	size, next, err := consumeUvarint(data, offset)
	if err != nil {
		return 0, 0, err
	}
	if size > 8 || size > uint64(len(data)-next) {
		return 0, 0, errors.New("invalid Fabric order-preserving uint")
	}
	end := next + int(size)
	var padded [8]byte
	copy(padded[8-int(size):], data[next:end])
	return binary.BigEndian.Uint64(padded[:]), end, nil
}

func sized(data []byte, offset int) ([]byte, int, error) {
	size, next, err := consumeUvarint(data, offset)
	if err != nil {
		return nil, 0, err
	}
	if size > uint64(len(data)-next) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	end := next + int(size)
	return bytes.Clone(data[next:end]), end, nil
}

func consumeUvarint(data []byte, offset int) (uint64, int, error) {
	if offset < 0 || offset >= len(data) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	value, read := binary.Uvarint(data[offset:])
	if read <= 0 {
		return 0, 0, errors.New("invalid varint")
	}
	return value, offset + read, nil
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func readRegular(path string) ([]byte, error) {
	file, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(file)
	return data, errors.Join(readErr, file.Close())
}

func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("snapshot entry is not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(fmt.Errorf("snapshot entry changed while opening: %s", path), file.Close())
	}
	return file, nil
}

func hash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
