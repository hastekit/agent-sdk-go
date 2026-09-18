package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type S3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}
type S3Config struct {
	Bucket string
	Prefix string
}

// S3Store stores each complete skill in one private object. The caller owns
// the S3 client, credentials, bucket, and lifecycle. Replacements are atomic.
type S3Store struct {
	listCache      listCache
	client         S3API
	bucket, prefix string
}

func NewS3Store(client S3API, cfg S3Config, opts ...StoreOption) (*S3Store, error) {
	if client == nil || strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("%w: S3 client and bucket required", ErrInvalid)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3Store{client: client, bucket: cfg.Bucket, prefix: prefix, listCache: configureStore(opts)}, nil
}
func (s *S3Store) ns(ctx context.Context, ns string) (string, error) {
	key, err := namespaceKey(ctx, ns)
	return s.prefix + key + "/", err
}
func s3Error(err error) error {
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") {
		return ErrNotFound
	}
	return err
}
func (s *S3Store) Put(ctx context.Context, ns string, b Bundle) (Metadata, error) {
	m, err := Validate(b)
	if err != nil {
		return m, err
	}
	prefix, err := s.ns(ctx, ns)
	if err != nil {
		return m, err
	}
	key, _ := skillKey(m.Name)
	data, err := json.Marshal(b)
	if err != nil {
		return m, err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(prefix + key), Body: bytes.NewReader(data), ContentType: aws.String("application/json")})
	return m, invalidateList(ctx, s.listCache, s.cacheScope(ns), err)
}
func (s *S3Store) read(ctx context.Context, key string) (Bundle, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return Bundle{}, s3Error(err)
	}
	defer out.Body.Close()
	return decodeBundle(out.Body)
}
func (s *S3Store) Get(ctx context.Context, ns, name string) (Bundle, error) {
	prefix, err := s.ns(ctx, ns)
	if err != nil {
		return Bundle{}, err
	}
	key, err := skillKey(name)
	if err != nil {
		return Bundle{}, err
	}
	return s.read(ctx, prefix+key)
}
func (s *S3Store) Delete(ctx context.Context, ns, name string) error {
	prefix, err := s.ns(ctx, ns)
	if err != nil {
		return err
	}
	key, err := skillKey(name)
	if err != nil {
		return err
	}
	// S3 delete is idempotent, including for absent keys.
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(prefix + key)})
	return invalidateList(ctx, s.listCache, s.cacheScope(ns), s3Error(err))
}
func (s *S3Store) List(ctx context.Context, ns string, opts ListOptions) (Page, error) {
	if _, err := namespaceKey(ctx, ns); err != nil {
		return Page{}, err
	}
	return s.listCache.list(ctx, s.cacheScope(ns), opts, func() (Page, error) { return s.list(ctx, ns, opts) })
}

func (s *S3Store) list(ctx context.Context, ns string, opts ListOptions) (Page, error) {
	page := Page{Skills: []Metadata{}}
	prefix, err := s.ns(ctx, ns)
	if err != nil {
		return page, err
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(int32(limit(opts.Limit)))}
	if opts.Cursor != "" {
		in.ContinuationToken = aws.String(opts.Cursor)
	}
	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return page, err
	}
	for _, obj := range out.Contents {
		key := aws.ToString(obj.Key)
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, ".json") {
			continue
		}
		b, err := s.read(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return page, err
		}
		m, err := Validate(b)
		if err != nil {
			return page, err
		}
		page.Skills = append(page.Skills, m)
	}
	page.NextCursor = aws.ToString(out.NextContinuationToken)
	return page, nil
}

func (s *S3Store) cacheScope(ns string) string {
	return "s3:" + s.bucket + "/" + s.prefix + "\x00" + ns
}
