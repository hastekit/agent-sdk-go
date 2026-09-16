package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
)

// S3API is the subset of the AWS SDK v2 S3 client used by S3Store.
type S3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type S3StoreConfig struct {
	Bucket       string
	Prefix       string // optional object key prefix
	MaxFileBytes int64  // zero: 20 MiB; uploads are buffered up to this limit
}

// S3Store stores private attachments as individual objects with their metadata.
// The caller owns the client and bucket. Bucket versioning is not required.
// Namespace access is determined by the caller, as with FileStore.
type S3Store struct {
	client                   S3API
	bucket, prefix, identity string
	maxBytes                 int64
}

func NewS3Store(client S3API, cfg S3StoreConfig) (*S3Store, error) {
	if client == nil || strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("%w: S3 client and bucket are required", ErrInvalid)
	}
	if cfg.MaxFileBytes < 0 || cfg.MaxFileBytes == math.MaxInt64 {
		return nil, fmt.Errorf("%w: invalid maximum file size", ErrInvalid)
	}
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 20 << 20
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3Store{client: client, bucket: cfg.Bucket, prefix: prefix, identity: "s3:" + uuid.NewString(), maxBytes: cfg.MaxFileBytes}, nil
}

func (s *S3Store) namespace(ctx context.Context, namespace string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if namespace == "" {
		return "", ErrDenied
	}
	hash := sha256.Sum256([]byte(namespace))
	return hex.EncodeToString(hash[:]), nil
}

func (s *S3Store) Put(ctx context.Context, namespace string, upload Upload) (Ref, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return Ref{}, err
	}
	media, _, err := mime.ParseMediaType(upload.MediaType)
	if err != nil || upload.Content == nil {
		return Ref{}, fmt.Errorf("%w: upload requires content and media type", ErrInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, upload.Content}, s.maxBytes+1))
	if err != nil {
		return Ref{}, err
	}
	if int64(len(data)) > s.maxBytes {
		return Ref{}, ErrTooLarge
	}
	if len(data) == 0 {
		return Ref{}, fmt.Errorf("%w: empty file", ErrInvalid)
	}
	if InlineImageMediaType(media) && http.DetectContentType(data) != media {
		return Ref{}, fmt.Errorf("%w: image media type does not match content", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	hash := sha256.Sum256(data)
	version := hex.EncodeToString(hash[:])
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.prefix + ns + "/" + id),
		Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String(media),
		IfNoneMatch: aws.String("*"),
		Metadata:    map[string]string{"sha256": version, "filename": base64.RawURLEncoding.EncodeToString([]byte(filepath.Base(upload.Filename)))},
	})
	if err != nil {
		return Ref{}, s3StoreError(err)
	}
	return Ref{ID: id, Version: version}, nil
}

func (s *S3Store) Lookup(ctx context.Context, namespace string, ref Ref) (Descriptor, error) {
	ns, err := s.namespace(ctx, namespace)
	if err != nil {
		return Descriptor{}, err
	}
	if !validID(ref.ID) {
		return Descriptor{}, ErrInvalid
	}
	key := s.prefix + ns + "/" + ref.ID
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return Descriptor{}, s3StoreError(err)
	}
	hash := out.Metadata["sha256"]
	rawHash, hashErr := hex.DecodeString(hash)
	filename, nameErr := base64.RawURLEncoding.DecodeString(out.Metadata["filename"])
	media, _, mediaErr := mime.ParseMediaType(aws.ToString(out.ContentType))
	if hashErr != nil || len(rawHash) != sha256.Size || nameErr != nil || mediaErr != nil || aws.ToInt64(out.ContentLength) <= 0 || aws.ToString(out.ETag) == "" {
		return Descriptor{}, fmt.Errorf("%w: invalid S3 attachment metadata", ErrInvalid)
	}
	if ref.Version != "" && ref.Version != hash {
		return Descriptor{}, ErrNotFound
	}
	return Descriptor{Namespace: s.identity + ":" + ns, Key: key, Version: aws.ToString(out.ETag), SHA256: hash, Filename: string(filename), MediaType: media, Size: aws.ToInt64(out.ContentLength)}, nil
}

// Open uses the ETag from Lookup to reject an object changed between metadata
// lookup and download. Attachment versions remain SHA-256 digests, not S3 ETags.
func (s *S3Store) Open(ctx context.Context, d Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ns, ok := strings.CutPrefix(d.Namespace, s.identity+":")
	raw, err := hex.DecodeString(ns)
	prefix := s.prefix + ns + "/"
	if !ok || err != nil || len(raw) != sha256.Size || !strings.HasPrefix(d.Key, prefix) || !validID(strings.TrimPrefix(d.Key, prefix)) || d.Version == "" {
		return nil, ErrDenied
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(d.Key), IfMatch: aws.String(d.Version)})
	if err != nil {
		return nil, s3StoreError(err)
	}
	if out.Body == nil {
		return nil, fmt.Errorf("%w: missing S3 object body", ErrInvalid)
	}
	return &contextReadCloser{contextReader{ctx, out.Body}, out.Body}, nil
}

func s3StoreError(err error) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchVersion", "PreconditionFailed":
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		case "AccessDenied", "Forbidden":
			return fmt.Errorf("%w: %w", ErrDenied, err)
		}
	}
	return err
}

var _ UploadStore = (*S3Store)(nil)
var _ S3API = (*s3.Client)(nil)
