package datastore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Config struct {
	Endpoint           string
	Bucket             string
	Region             string
	Prefix             string
	Secure             bool
	AccessKeyEnv       string
	SecretAccessKeyEnv string
	SessionTokenEnv    string
	CreateBucket       bool
}

type S3Datastore struct {
	client *minio.Client
	bucket string
	prefix string
}

func NewS3Datastore(ctx context.Context, config S3Config) (*S3Datastore, error) {
	if config.Endpoint == "" {
		return nil, fmt.Errorf("s3 datastore endpoint cannot be empty")
	}
	if config.Bucket == "" {
		return nil, fmt.Errorf("s3 datastore bucket cannot be empty")
	}

	prefix, err := normalizeS3Prefix(config.Prefix)
	if err != nil {
		return nil, err
	}

	accessKeyEnv := config.AccessKeyEnv
	if accessKeyEnv == "" {
		accessKeyEnv = "AWS_ACCESS_KEY_ID"
	}
	secretAccessKeyEnv := config.SecretAccessKeyEnv
	if secretAccessKeyEnv == "" {
		secretAccessKeyEnv = "AWS_SECRET_ACCESS_KEY"
	}
	sessionTokenEnv := config.SessionTokenEnv
	if sessionTokenEnv == "" {
		sessionTokenEnv = "AWS_SESSION_TOKEN"
	}

	accessKey := os.Getenv(accessKeyEnv)
	secretAccessKey := os.Getenv(secretAccessKeyEnv)
	if accessKey == "" || secretAccessKey == "" {
		return nil, fmt.Errorf("s3 datastore credentials require %s and %s", accessKeyEnv, secretAccessKeyEnv)
	}

	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretAccessKey, os.Getenv(sessionTokenEnv)),
		Region: config.Region,
		Secure: config.Secure,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 datastore client: %w", err)
	}

	if config.CreateBucket {
		exists, err := client.BucketExists(ctx, config.Bucket)
		if err != nil {
			return nil, fmt.Errorf("check s3 bucket %s: %w", config.Bucket, err)
		}
		if !exists {
			if err := client.MakeBucket(ctx, config.Bucket, minio.MakeBucketOptions{Region: config.Region}); err != nil {
				return nil, fmt.Errorf("create s3 bucket %s: %w", config.Bucket, err)
			}
		}
	}

	return &S3Datastore{client: client, bucket: config.Bucket, prefix: prefix}, nil
}

func (d *S3Datastore) Put(ctx context.Context, key Key, body []byte, options PutOptions) (ObjectInfo, error) {
	putOptions := minio.PutObjectOptions{ContentType: defaultContentType(options.ContentType)}
	if options.IfAbsent {
		putOptions.SetMatchETagExcept("*")
	}

	_, err := d.client.PutObject(ctx, d.bucket, d.objectName(key), bytes.NewReader(body), int64(len(body)), putOptions)
	if err != nil {
		if isS3Conflict(err) {
			return ObjectInfo{}, fmt.Errorf("put %s if absent: %w", key, ErrAlreadyExists)
		}
		return ObjectInfo{}, fmt.Errorf("put s3 object %s: %w", key, err)
	}

	return ObjectInfo{Key: key, Size: int64(len(body)), ContentType: putOptions.ContentType}, nil
}

func (d *S3Datastore) Get(ctx context.Context, key Key) (Object, error) {
	reader, err := d.client.GetObject(ctx, d.bucket, d.objectName(key), minio.GetObjectOptions{})
	if err != nil {
		return Object{}, fmt.Errorf("get s3 object %s: %w", key, err)
	}
	defer reader.Close()

	info, err := reader.Stat()
	if err != nil {
		if isS3NotFound(err) {
			return Object{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
		}
		return Object{}, fmt.Errorf("stat s3 object %s: %w", key, err)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return Object{}, fmt.Errorf("read s3 object %s: %w", key, err)
	}

	return Object{
		Info: ObjectInfo{Key: key, Size: info.Size, ContentType: defaultContentType(info.ContentType)},
		Body: body,
	}, nil
}

func (d *S3Datastore) Exists(ctx context.Context, key Key) (bool, error) {
	_, err := d.client.StatObject(ctx, d.bucket, d.objectName(key), minio.StatObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat s3 object %s: %w", key, err)
	}
	return true, nil
}

func (d *S3Datastore) List(ctx context.Context, prefix Key) ([]ObjectInfo, error) {
	objectPrefix := d.objectName(prefix)
	if objectPrefix != "" {
		objectPrefix += "/"
	}

	objects := []ObjectInfo{}
	for object := range d.client.ListObjects(ctx, d.bucket, minio.ListObjectsOptions{Prefix: objectPrefix, Recursive: true}) {
		if object.Err != nil {
			return nil, fmt.Errorf("list s3 prefix %s: %w", prefix, object.Err)
		}

		trimmed := strings.TrimPrefix(object.Key, d.prefix)
		key, err := ParseKey(trimmed)
		if err != nil {
			return nil, err
		}

		// Bucket listings do not carry per-object content types, so stat each
		// object to honor the datastore contract's ContentType guarantee.
		stat, err := d.client.StatObject(ctx, d.bucket, object.Key, minio.StatObjectOptions{})
		if err != nil {
			return nil, fmt.Errorf("stat s3 object %s during list: %w", key, err)
		}
		objects = append(objects, ObjectInfo{Key: key, Size: stat.Size, ContentType: defaultContentType(stat.ContentType)})
	}

	sort.Slice(objects, func(left, right int) bool {
		return objects[left].Key.String() < objects[right].Key.String()
	})

	return objects, nil
}

func (d *S3Datastore) objectName(key Key) string {
	return d.prefix + key.String()
}

func normalizeS3Prefix(prefix string) (string, error) {
	if prefix == "" {
		return "", nil
	}

	trimmed := strings.TrimSuffix(prefix, "/")
	if _, err := ParseKey(trimmed); err != nil {
		return "", fmt.Errorf("invalid s3 prefix: %w", err)
	}
	return trimmed + "/", nil
}

func isS3NotFound(err error) bool {
	var response minio.ErrorResponse
	if !errors.As(err, &response) {
		return false
	}
	return response.Code == "NoSuchKey" || response.Code == "NoSuchBucket" || response.StatusCode == 404
}

func isS3Conflict(err error) bool {
	var response minio.ErrorResponse
	if !errors.As(err, &response) {
		return false
	}
	return response.Code == "PreconditionFailed" || response.StatusCode == 412
}
