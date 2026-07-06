package datastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var (
	ErrInvalidKey    = errors.New("invalid datastore key")
	ErrNotFound      = errors.New("datastore object not found")
	ErrAlreadyExists = errors.New("datastore object already exists")
)

var digestHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Key struct {
	value string
}

func ParseKey(value string) (Key, error) {
	if value == "" {
		return Key{}, fmt.Errorf("%w %q: key cannot be empty", ErrInvalidKey, value)
	}
	if strings.HasPrefix(value, "/") {
		return Key{}, fmt.Errorf("%w %q: key cannot have a leading slash", ErrInvalidKey, value)
	}
	if filepath.IsAbs(value) || isWindowsAbs(value) {
		return Key{}, fmt.Errorf("%w %q: key cannot be absolute", ErrInvalidKey, value)
	}
	if strings.Contains(value, "\\") {
		return Key{}, fmt.Errorf("%w %q: key must use forward slashes", ErrInvalidKey, value)
	}

	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return Key{}, fmt.Errorf("%w %q: invalid path segment %q", ErrInvalidKey, value, segment)
		}
	}

	cleaned := path.Clean(value)
	if cleaned != value {
		return Key{}, fmt.Errorf("%w %q: key is not normalized", ErrInvalidKey, value)
	}

	return Key{value: value}, nil
}

func MustKey(value string) Key {
	key, err := ParseKey(value)
	if err != nil {
		panic(err)
	}
	return key
}

func BlobKeySHA256Hex(digestHex string) (Key, error) {
	if !digestHexPattern.MatchString(digestHex) {
		return Key{}, fmt.Errorf("%w %q: blob digest must be 64 lowercase hex characters", ErrInvalidKey, digestHex)
	}
	return ParseKey("blobs/sha256/" + digestHex)
}

func BlobKeyForBytes(body []byte) Key {
	sum := sha256.Sum256(body)
	key, err := BlobKeySHA256Hex(hex.EncodeToString(sum[:]))
	if err != nil {
		panic(err)
	}
	return key
}

func (k Key) String() string {
	return k.value
}

type PutOptions struct {
	ContentType string
	IfAbsent    bool
}

type ObjectInfo struct {
	Key         Key
	Size        int64
	ContentType string
}

type Object struct {
	Info ObjectInfo
	Body []byte
}

type Datastore interface {
	Put(ctx context.Context, key Key, body []byte, options PutOptions) (ObjectInfo, error)
	Get(ctx context.Context, key Key) (Object, error)
	Exists(ctx context.Context, key Key) (bool, error)
	List(ctx context.Context, prefix Key) ([]ObjectInfo, error)
}

func defaultContentType(contentType string) string {
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

func isWindowsAbs(value string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	if len(value) < 3 {
		return false
	}
	drive := value[0]
	return ((drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')) && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}
