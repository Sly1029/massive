package datastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/google/uuid"
)

const localMetadataDirName = ".massive-datastore-metadata"

type LocalConfig struct {
	Root string
}

type LocalDatastore struct {
	root string
}

type localMetadata struct {
	ContentType string `json:"contentType"`
}

func NewLocalDatastore(config LocalConfig) (*LocalDatastore, error) {
	if config.Root == "" {
		return nil, fmt.Errorf("local datastore root cannot be empty")
	}

	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve local datastore root: %w", err)
	}

	return &LocalDatastore{root: root}, nil
}

func (d *LocalDatastore) Put(ctx context.Context, key Key, body []byte, options PutOptions) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, fmt.Errorf("put %s: %w", key, err)
	}
	contentType := defaultContentType(options.ContentType)

	if err := os.MkdirAll(d.root, 0o755); err != nil {
		return ObjectInfo{}, fmt.Errorf("create datastore root: %w", err)
	}

	root, err := os.OpenRoot(d.root)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("open datastore root: %w", err)
	}
	defer root.Close()

	target, err := d.pathForKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	if err := root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ObjectInfo{}, fmt.Errorf("create parent for %s: %w", key, err)
	}

	temporary := filepath.Join(filepath.Dir(target), ".tmp-"+filepath.Base(target)+"-"+uuid.NewString())
	if err := root.WriteFile(temporary, body, 0o644); err != nil {
		return ObjectInfo{}, fmt.Errorf("write temporary object for %s: %w", key, err)
	}

	installed := false
	defer func() {
		if !installed {
			_ = root.Remove(temporary)
		}
	}()

	if options.IfAbsent {
		metadataInstalled, err := d.writeMetadataIfAbsent(root, key, contentType)
		if err != nil {
			return ObjectInfo{}, err
		}
		if !metadataInstalled {
			existingContentType, err := d.readContentType(root, key)
			if err != nil {
				return ObjectInfo{}, err
			}
			if existingContentType != contentType {
				return ObjectInfo{}, fmt.Errorf("put %s if absent: existing content type %q differs from %q: %w", key, existingContentType, contentType, ErrAlreadyExists)
			}
		}
		if err := root.Link(temporary, target); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ObjectInfo{}, fmt.Errorf("put %s if absent: %w", key, ErrAlreadyExists)
			}
			return ObjectInfo{}, fmt.Errorf("install object %s if absent: %w", key, err)
		}
		installed = true
		if err := root.Remove(temporary); err != nil {
			return ObjectInfo{}, fmt.Errorf("remove temporary object for %s: %w", key, err)
		}
	} else {
		if err := root.Rename(temporary, target); err != nil {
			return ObjectInfo{}, fmt.Errorf("rename temporary object for %s: %w", key, err)
		}
		installed = true
	}

	if !options.IfAbsent {
		if err := d.writeMetadata(root, key, contentType); err != nil {
			return ObjectInfo{}, err
		}
	}

	return ObjectInfo{Key: key, Size: int64(len(body)), ContentType: contentType}, nil
}

func (d *LocalDatastore) Get(ctx context.Context, key Key) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, fmt.Errorf("get %s: %w", key, err)
	}

	root, err := os.OpenRoot(d.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Object{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
		}
		return Object{}, fmt.Errorf("open datastore root: %w", err)
	}
	defer root.Close()

	target, err := d.pathForKey(key)
	if err != nil {
		return Object{}, err
	}

	body, err := root.ReadFile(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Object{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
		}
		return Object{}, fmt.Errorf("read object %s: %w", key, err)
	}

	contentType, err := d.readContentType(root, key)
	if err != nil {
		return Object{}, err
	}

	return Object{
		Info: ObjectInfo{Key: key, Size: int64(len(body)), ContentType: contentType},
		Body: body,
	}, nil
}

func (d *LocalDatastore) Exists(ctx context.Context, key Key) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("exists %s: %w", key, err)
	}

	root, err := os.OpenRoot(d.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open datastore root: %w", err)
	}
	defer root.Close()

	target, err := d.pathForKey(key)
	if err != nil {
		return false, err
	}

	info, err := root.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat object %s: %w", key, err)
	}

	return !info.IsDir(), nil
}

func (d *LocalDatastore) List(ctx context.Context, prefix Key) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}

	root, err := os.OpenRoot(d.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open datastore root: %w", err)
	}
	defer root.Close()

	prefixPath, err := d.pathForKey(prefix)
	if err != nil {
		return nil, err
	}

	if _, err := root.Stat(prefixPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat prefix %s: %w", prefix, err)
	}

	objects := []ObjectInfo{}
	err = fs.WalkDir(root.FS(), filepath.ToSlash(prefixPath), func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if current == localMetadataDirName {
				return filepath.SkipDir
			}
			return nil
		}

		key, err := ParseKey(current)
		if err != nil {
			return err
		}
		info, err := root.Stat(filepath.FromSlash(current))
		if err != nil {
			return err
		}
		contentType, err := d.readContentType(root, key)
		if err != nil {
			return err
		}
		objects = append(objects, ObjectInfo{Key: key, Size: info.Size(), ContentType: contentType})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}

	sort.Slice(objects, func(left, right int) bool {
		return objects[left].Key.String() < objects[right].Key.String()
	})

	return objects, nil
}

func (d *LocalDatastore) pathForKey(key Key) (string, error) {
	local, err := filepath.Localize(key.String())
	if err != nil || local == "." {
		return "", fmt.Errorf("%w %q: object path must be local to the datastore", ErrInvalidKey, key.String())
	}
	return local, nil
}

func (d *LocalDatastore) metadataPath(key Key) string {
	sum := sha256.Sum256([]byte(key.String()))
	return filepath.Join(localMetadataDirName, hex.EncodeToString(sum[:])+".json")
}

func (d *LocalDatastore) writeMetadata(root *os.Root, key Key, contentType string) error {
	metadataPath := d.metadataPath(key)
	if err := root.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		return fmt.Errorf("create metadata parent for %s: %w", key, err)
	}

	body, err := json.Marshal(localMetadata{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("encode metadata for %s: %w", key, err)
	}

	temporary := metadataPath + ".tmp-" + uuid.NewString()
	if err := root.WriteFile(temporary, body, 0o644); err != nil {
		return fmt.Errorf("write temporary metadata for %s: %w", key, err)
	}
	if err := root.Rename(temporary, metadataPath); err != nil {
		_ = root.Remove(temporary)
		return fmt.Errorf("rename temporary metadata for %s: %w", key, err)
	}
	return nil
}

// writeMetadataIfAbsent establishes a complete metadata record before an
// IfAbsent body can become visible. A metadata-only record is an intentional
// recoverable crash state: a later matching IfAbsent call may install the
// missing body, while a differing content type cannot replace the record.
func (d *LocalDatastore) writeMetadataIfAbsent(root *os.Root, key Key, contentType string) (bool, error) {
	metadataPath := d.metadataPath(key)
	if err := root.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		return false, fmt.Errorf("create metadata parent for %s: %w", key, err)
	}

	body, err := json.Marshal(localMetadata{ContentType: contentType})
	if err != nil {
		return false, fmt.Errorf("encode metadata for %s: %w", key, err)
	}
	temporary := metadataPath + ".tmp-" + uuid.NewString()
	if err := root.WriteFile(temporary, body, 0o644); err != nil {
		return false, fmt.Errorf("write temporary metadata for %s: %w", key, err)
	}
	defer func() { _ = root.Remove(temporary) }()

	if err := root.Link(temporary, metadataPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("install metadata for %s if absent: %w", key, err)
	}
	return true, nil
}

func (d *LocalDatastore) readContentType(root *os.Root, key Key) (string, error) {
	body, err := root.ReadFile(d.metadataPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultContentType(""), nil
		}
		return "", fmt.Errorf("read metadata for %s: %w", key, err)
	}

	var metadata localMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return "", fmt.Errorf("decode metadata for %s: %w", key, err)
	}
	return defaultContentType(metadata.ContentType), nil
}
