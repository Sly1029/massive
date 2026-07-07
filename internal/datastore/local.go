package datastore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	target, err := d.pathForKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ObjectInfo{}, fmt.Errorf("create parent for %s: %w", key, err)
	}

	temporary := filepath.Join(filepath.Dir(target), ".tmp-"+filepath.Base(target)+"-"+uuid.NewString())
	if err := os.WriteFile(temporary, body, 0o644); err != nil {
		return ObjectInfo{}, fmt.Errorf("write temporary object for %s: %w", key, err)
	}

	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(temporary)
		}
	}()

	if options.IfAbsent {
		if err := os.Link(temporary, target); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ObjectInfo{}, fmt.Errorf("put %s if absent: %w", key, ErrAlreadyExists)
			}
			return ObjectInfo{}, fmt.Errorf("install object %s if absent: %w", key, err)
		}
		installed = true
		if err := os.Remove(temporary); err != nil {
			return ObjectInfo{}, fmt.Errorf("remove temporary object for %s: %w", key, err)
		}
	} else {
		if err := os.Rename(temporary, target); err != nil {
			return ObjectInfo{}, fmt.Errorf("rename temporary object for %s: %w", key, err)
		}
		installed = true
	}

	contentType := defaultContentType(options.ContentType)
	if err := d.writeMetadata(key, contentType); err != nil {
		return ObjectInfo{}, err
	}

	return ObjectInfo{Key: key, Size: int64(len(body)), ContentType: contentType}, nil
}

func (d *LocalDatastore) Get(ctx context.Context, key Key) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, fmt.Errorf("get %s: %w", key, err)
	}

	target, err := d.pathForKey(key)
	if err != nil {
		return Object{}, err
	}

	body, err := os.ReadFile(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Object{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
		}
		return Object{}, fmt.Errorf("read object %s: %w", key, err)
	}

	contentType, err := d.readContentType(key)
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

	target, err := d.pathForKey(key)
	if err != nil {
		return false, err
	}

	info, err := os.Stat(target)
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

	prefixPath, err := d.pathForKey(prefix)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(prefixPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat prefix %s: %w", prefix, err)
	}

	objects := []ObjectInfo{}
	err = filepath.WalkDir(prefixPath, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if current == filepath.Join(d.root, localMetadataDirName) {
				return filepath.SkipDir
			}
			return nil
		}

		relative, err := filepath.Rel(d.root, current)
		if err != nil {
			return err
		}
		key, err := ParseKey(filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		contentType, err := d.readContentType(key)
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
	resolved := filepath.Join(d.root, filepath.FromSlash(key.String()))
	relative, err := filepath.Rel(d.root, resolved)
	if err != nil {
		return "", fmt.Errorf("resolve key %s under local datastore root: %w", key, err)
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("%w %q: resolved path escapes datastore root", ErrInvalidKey, key.String())
	}
	return resolved, nil
}

func (d *LocalDatastore) metadataPath(key Key) string {
	sum := sha256.Sum256([]byte(key.String()))
	return filepath.Join(d.root, localMetadataDirName, hex.EncodeToString(sum[:])+".json")
}

func (d *LocalDatastore) writeMetadata(key Key, contentType string) error {
	metadataPath := d.metadataPath(key)
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		return fmt.Errorf("create metadata parent for %s: %w", key, err)
	}

	body, err := json.Marshal(localMetadata{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("encode metadata for %s: %w", key, err)
	}

	temporary := metadataPath + ".tmp-" + uuid.NewString()
	if err := os.WriteFile(temporary, body, 0o644); err != nil {
		return fmt.Errorf("write temporary metadata for %s: %w", key, err)
	}
	if err := os.Rename(temporary, metadataPath); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("rename temporary metadata for %s: %w", key, err)
	}
	return nil
}

func (d *LocalDatastore) readContentType(key Key) (string, error) {
	body, err := os.ReadFile(d.metadataPath(key))
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
